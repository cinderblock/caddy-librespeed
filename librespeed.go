// Package librespeed is a Caddy HTTP handler that serves a speed test: the
// LibreSpeed backend protocol (garbage, empty, getIP) plus an embedded UI.
package librespeed

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/dustin/go-humanize"
	"go.uber.org/zap"
)

func init() {
	caddy.RegisterModule(Handler{})
	httpcaddyfile.RegisterHandlerDirective("librespeed", parseCaddyfile)
	httpcaddyfile.RegisterDirectiveOrder("librespeed", httpcaddyfile.Before, "file_server")
}

const (
	// chunkSize is LibreSpeed's download unit: garbage?ckSize=N sends N of these.
	chunkSize = 1 << 20

	defaultChunks = 4
	maxChunks     = 1024

	// DefaultMaxUploadSize caps one upload request. LibreSpeed clients send
	// 20 MB blobs, and Cloudflare rejects bodies over 100 MB.
	DefaultMaxUploadSize = 64 << 20
)

// chunk is the download payload: 1 MiB of random bytes, generated once and
// written as many times as a request asks for. Random so that nothing on the
// path can compress it.
var chunk = sync.OnceValue(func() []byte {
	b := make([]byte, chunkSize)
	rand.Read(b) // never returns an error; crashes the program if the OS RNG fails
	return b
})

// Handler serves a LibreSpeed-compatible speed test.
//
// API endpoints are matched by the request path's last segment, with or
// without a ".php" suffix, so the handler works at any path prefix:
//
//   - garbage: GET ?ckSize=N streams N MiB of random data (default 4, max 1024)
//   - empty:   drains the request body (upload test) or answers a ping
//   - getIP:   the client's IP, as LibreSpeed's JSON
//
// Every other path serves the embedded UI, unless it is disabled.
type Handler struct {
	// MaxUploadSize caps one request body sent to "empty", in bytes.
	// Default: 64 MiB.
	MaxUploadSize int64 `json:"max_upload_size,omitempty"`

	// DisableUI turns off the embedded UI; only the API endpoints answer.
	DisableUI bool `json:"disable_ui,omitempty"`

	// Servers lists the speed test servers the UI offers to test against.
	// Empty means just this one.
	Servers []Server `json:"servers,omitempty"`

	logger *zap.Logger
	config []byte // config.json for the UI
}

// Server is one entry in the UI's server menu.
type Server struct {
	// Name is shown in the UI, e.g. "San Francisco".
	Name string `json:"name"`
	// URL is the absolute base URL the handler answers at, e.g.
	// "https://speedtest.example.com/". It must end with "/".
	URL string `json:"url"`
}

// CaddyModule returns the Caddy module information.
func (Handler) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.librespeed",
		New: func() caddy.Module { return new(Handler) },
	}
}

// Provision sets defaults and prepares the UI's config.
func (h *Handler) Provision(ctx caddy.Context) error {
	h.logger = ctx.Logger()
	if h.MaxUploadSize == 0 {
		h.MaxUploadSize = DefaultMaxUploadSize
	}
	servers := h.Servers
	if servers == nil {
		servers = []Server{}
	}
	cfg, err := json.Marshal(struct {
		Servers       []Server `json:"servers"`
		MaxUploadSize int64    `json:"maxUploadSize"`
	}{servers, h.MaxUploadSize})
	if err != nil {
		return err
	}
	h.config = cfg
	chunk() // generate the payload now rather than during the first test
	return nil
}

// Validate checks the configuration.
func (h *Handler) Validate() error {
	if h.MaxUploadSize < 0 {
		return fmt.Errorf("max_upload_size must not be negative: %d", h.MaxUploadSize)
	}
	for _, s := range h.Servers {
		if s.Name == "" {
			return fmt.Errorf("server %q has no name", s.URL)
		}
		u, err := url.Parse(s.URL)
		if err != nil {
			return fmt.Errorf("server %q: %v", s.Name, err)
		}
		if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("server %q: URL must be absolute http(s), got %q", s.Name, s.URL)
		}
		if !strings.HasSuffix(u.Path, "/") {
			return fmt.Errorf("server %q: URL must end with \"/\", got %q", s.Name, s.URL)
		}
		if u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("server %q: URL must not have a query or fragment, got %q", s.Name, s.URL)
		}
	}
	return nil
}

// endpoint names the API endpoint a path addresses, or "" for anything else.
func endpoint(p string) string {
	switch seg := strings.TrimSuffix(path.Base(p), ".php"); seg {
	case "garbage", "empty", "getIP":
		return seg
	}
	return ""
}

// ServeHTTP implements caddyhttp.MiddlewareHandler. It is a terminal handler:
// next is never called.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request, _ caddyhttp.Handler) error {
	ep := endpoint(r.URL.Path)
	if ep == "" {
		if h.DisableUI {
			return caddyhttp.Error(http.StatusNotFound, nil)
		}
		return h.serveUI(w, r)
	}

	hdr := w.Header()
	if r.URL.Query().Has("cors") {
		hdr.Set("Access-Control-Allow-Origin", "*")
		hdr.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		hdr.Set("Access-Control-Allow-Headers", "Content-Encoding, Content-Type")
		hdr.Set("Access-Control-Max-Age", "86400")
		// Lets a cross-origin UI read Resource Timing, for accurate pings.
		hdr.Set("Timing-Allow-Origin", "*")
	}
	hdr.Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0, s-maxage=0, no-transform")
	hdr.Set("Pragma", "no-cache")

	if r.Method == http.MethodOptions {
		hdr.Set("Allow", allowed(ep))
		w.WriteHeader(http.StatusNoContent)
		return nil
	}

	switch ep {
	case "garbage":
		return h.garbage(w, r)
	case "empty":
		return h.empty(w, r)
	default:
		return h.getIP(w, r)
	}
}

func allowed(ep string) string {
	if ep == "empty" {
		return "GET, HEAD, POST, PUT, OPTIONS"
	}
	return "GET, HEAD, OPTIONS"
}

func methodNotAllowed(w http.ResponseWriter, ep string) error {
	w.Header().Set("Allow", allowed(ep))
	return caddyhttp.Error(http.StatusMethodNotAllowed, nil)
}

// chunks parses ckSize the way LibreSpeed's garbage.php does: anything that
// isn't a positive integer means the default, and large values are clamped.
func chunks(ckSize string) int {
	if ckSize == "" || strings.TrimLeft(ckSize, "0123456789") != "" {
		return defaultChunks
	}
	n, err := strconv.Atoi(ckSize)
	if err != nil { // all digits, so the only failure is overflow
		return maxChunks
	}
	if n == 0 {
		return defaultChunks
	}
	return min(n, maxChunks)
}

func (h *Handler) garbage(w http.ResponseWriter, r *http.Request) error {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return methodNotAllowed(w, "garbage")
	}
	n := chunks(r.URL.Query().Get("ckSize"))

	hdr := w.Header()
	hdr.Set("Content-Type", "application/octet-stream")
	hdr.Set("Content-Disposition", "attachment; filename=random.dat")
	hdr.Set("Content-Length", strconv.Itoa(n*chunkSize))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return nil
	}

	buf := chunk()
	for i := range n {
		if _, err := w.Write(buf); err != nil {
			// The client hung up. That's how a download test ends: it aborts
			// its streams when time is up. The status is already sent, so
			// there is nothing to report to Caddy.
			h.logger.Debug("download ended early",
				zap.Int("sent_chunks", i), zap.Int("chunks", n), zap.Error(err))
			return nil
		}
	}
	return nil
}

func (h *Handler) empty(w http.ResponseWriter, r *http.Request) error {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut:
	default:
		return methodNotAllowed(w, "empty")
	}
	body := http.MaxBytesReader(w, r.Body, h.MaxUploadSize)
	if _, err := io.Copy(io.Discard, body); err != nil {
		if mbe := (*http.MaxBytesError)(nil); errors.As(err, &mbe) {
			return caddyhttp.Error(http.StatusRequestEntityTooLarge, err)
		}
		// Usually the client aborting its upload when time is up.
		return caddyhttp.Error(http.StatusBadRequest, err)
	}
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusOK)
	return nil
}

func (h *Handler) getIP(w http.ResponseWriter, r *http.Request) error {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return methodNotAllowed(w, "getIP")
	}
	ip, _ := caddyhttp.GetVar(r.Context(), caddyhttp.ClientIPVarKey).(string)
	if ip == "" {
		return caddyhttp.Error(http.StatusInternalServerError,
			errors.New("caddy did not set the client_ip variable"))
	}
	body, err := json.Marshal(struct {
		ProcessedString string `json:"processedString"`
		RawISPInfo      string `json:"rawIspInfo"`
	}{ip, ""})
	if err != nil {
		return caddyhttp.Error(http.StatusInternalServerError, err)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return nil
	}
	_, err = w.Write(body)
	return err
}

// UnmarshalCaddyfile sets up the handler from Caddyfile tokens:
//
//	librespeed [<matcher>] {
//	    max_upload_size <size>
//	    ui on|off
//	    server <name> <url>
//	}
func (h *Handler) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.Next() // directive name
	if d.NextArg() {
		return d.ArgErr()
	}
	for d.NextBlock(0) {
		switch d.Val() {
		case "max_upload_size":
			var size string
			if !d.Args(&size) || d.NextArg() {
				return d.ArgErr()
			}
			n, err := humanize.ParseBytes(size)
			if err != nil {
				return d.Errf("parsing max_upload_size: %v", err)
			}
			if n == 0 || n > 1<<62 {
				return d.Errf("max_upload_size out of range: %s", size)
			}
			h.MaxUploadSize = int64(n)
		case "ui":
			var v string
			if !d.Args(&v) || d.NextArg() {
				return d.ArgErr()
			}
			switch v {
			case "on":
				h.DisableUI = false
			case "off":
				h.DisableUI = true
			default:
				return d.Errf("ui must be on or off, got %q", v)
			}
		case "server":
			var s Server
			if !d.Args(&s.Name, &s.URL) || d.NextArg() {
				return d.ArgErr()
			}
			h.Servers = append(h.Servers, s)
		default:
			return d.Errf("unrecognized librespeed option %q", d.Val())
		}
	}
	return nil
}

func parseCaddyfile(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	var m Handler
	err := m.UnmarshalCaddyfile(h.Dispenser)
	return &m, err
}

// Interface guards
var (
	_ caddy.Provisioner           = (*Handler)(nil)
	_ caddy.Validator             = (*Handler)(nil)
	_ caddyhttp.MiddlewareHandler = (*Handler)(nil)
	_ caddyfile.Unmarshaler       = (*Handler)(nil)
)
