package librespeed

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

func provisioned(t testing.TB, h *Handler) *Handler {
	t.Helper()
	ctx, cancel := caddy.NewContext(caddy.Context{Context: context.Background()})
	t.Cleanup(cancel)
	if err := h.Provision(ctx); err != nil {
		t.Fatal(err)
	}
	if err := h.Validate(); err != nil {
		t.Fatal(err)
	}
	return h
}

// serve runs one request through h, with Caddy's client_ip variable set as
// the server would set it.
func serve(t testing.TB, h *Handler, r *http.Request) (*httptest.ResponseRecorder, error) {
	t.Helper()
	vars := map[string]any{caddyhttp.ClientIPVarKey: "203.0.113.7"}
	r = r.WithContext(context.WithValue(r.Context(), caddyhttp.VarsCtxKey, vars))
	w := httptest.NewRecorder()
	return w, h.ServeHTTP(w, r, nil)
}

func status(t testing.TB, w *httptest.ResponseRecorder, err error) int {
	t.Helper()
	if err == nil {
		return w.Code
	}
	var he caddyhttp.HandlerError
	if !errors.As(err, &he) {
		t.Fatalf("not a caddyhttp.HandlerError: %v", err)
	}
	return he.StatusCode
}

func TestEndpoint(t *testing.T) {
	for p, want := range map[string]string{
		"/garbage":                 "garbage",
		"/garbage.php":             "garbage",
		"/backend/garbage.php":     "garbage",
		"/speedtest/backend/empty": "empty",
		"/empty.php":               "empty",
		"/getIP":                   "getIP",
		"/backend/getIP.php":       "getIP",
		"/":                        "",
		"/index.html":              "",
		"/garbage/":                "garbage", // path.Base drops the trailing slash
		"/getip":                   "",
		"/garbage.php.php":         "",
		"/backend/":                "",
	} {
		if got := endpoint(p); got != want {
			t.Errorf("endpoint(%q) = %q, want %q", p, got, want)
		}
	}
}

func TestChunks(t *testing.T) {
	for in, want := range map[string]int{
		"":                        defaultChunks,
		"0":                       defaultChunks,
		"1":                       1,
		"100":                     100,
		"1024":                    1024,
		"1025":                    1024,
		"99999999999999999999999": 1024,
		"-5":                      defaultChunks,
		"+5":                      defaultChunks,
		"5MB":                     defaultChunks,
		" 5":                      defaultChunks,
	} {
		if got := chunks(in); got != want {
			t.Errorf("chunks(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestGarbage(t *testing.T) {
	h := provisioned(t, &Handler{})
	for _, target := range []string{"/backend/garbage.php?ckSize=3", "/garbage?ckSize=3"} {
		w, err := serve(t, h, httptest.NewRequest("GET", target, nil))
		if err != nil {
			t.Fatal(err)
		}
		if w.Code != 200 {
			t.Fatalf("%s: status %d", target, w.Code)
		}
		body := w.Body.Bytes()
		if len(body) != 3*chunkSize {
			t.Fatalf("%s: got %d bytes, want %d", target, len(body), 3*chunkSize)
		}
		if got := w.Header().Get("Content-Length"); got != strconv.Itoa(3*chunkSize) {
			t.Errorf("Content-Length = %q", got)
		}
		if !bytes.Equal(body[:chunkSize], chunk()) || !bytes.Equal(body[2*chunkSize:], chunk()) {
			t.Error("body is not the random chunk, repeated")
		}
		cc := w.Header().Get("Cache-Control")
		if !strings.Contains(cc, "no-transform") || !strings.Contains(cc, "no-store") {
			t.Errorf("Cache-Control = %q, want no-store and no-transform", cc)
		}
		if w.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Error("CORS headers sent without ?cors")
		}
	}
}

func TestGarbageDefaultSize(t *testing.T) {
	h := provisioned(t, &Handler{})
	w, err := serve(t, h, httptest.NewRequest("GET", "/garbage", nil))
	if err != nil {
		t.Fatal(err)
	}
	if w.Body.Len() != defaultChunks*chunkSize {
		t.Fatalf("got %d bytes, want %d", w.Body.Len(), defaultChunks*chunkSize)
	}
}

func TestChunkIsRandom(t *testing.T) {
	c := chunk()
	var counts [256]int
	for _, b := range c {
		counts[b]++
	}
	// 1 MiB of uniform bytes puts ~4096 in each bucket.
	for v, n := range counts {
		if n < 3500 || n > 4700 {
			t.Fatalf("byte %d appears %d times; chunk doesn't look random", v, n)
		}
	}
}

func TestGarbageHead(t *testing.T) {
	h := provisioned(t, &Handler{})
	w, err := serve(t, h, httptest.NewRequest("HEAD", "/garbage?ckSize=2", nil))
	if err != nil {
		t.Fatal(err)
	}
	if w.Body.Len() != 0 || w.Header().Get("Content-Length") != strconv.Itoa(2*chunkSize) {
		t.Fatalf("HEAD: body %d bytes, Content-Length %q", w.Body.Len(), w.Header().Get("Content-Length"))
	}
}

// failingWriter accepts n writes, then fails like a closed connection.
type failingWriter struct {
	*httptest.ResponseRecorder
	n int
}

func (f *failingWriter) Write(b []byte) (int, error) {
	if f.n == 0 {
		return 0, errors.New("connection reset by peer")
	}
	f.n--
	return f.ResponseRecorder.Write(b)
}

func TestGarbageClientHangsUp(t *testing.T) {
	h := provisioned(t, &Handler{})
	w := &failingWriter{httptest.NewRecorder(), 1}
	r := httptest.NewRequest("GET", "/garbage?ckSize=10", nil)
	if err := h.ServeHTTP(w, r, nil); err != nil {
		t.Fatalf("a client hanging up mid-download is normal, got error %v", err)
	}
	if w.Body.Len() != chunkSize {
		t.Fatalf("wrote %d bytes before the failure, want %d", w.Body.Len(), chunkSize)
	}
}

func TestEmpty(t *testing.T) {
	h := provisioned(t, &Handler{MaxUploadSize: 2 * chunkSize})
	for _, method := range []string{"POST", "PUT"} {
		w, err := serve(t, h, httptest.NewRequest(method, "/backend/empty.php", bytes.NewReader(make([]byte, chunkSize))))
		if code := status(t, w, err); code != 200 {
			t.Fatalf("%s: status %d", method, code)
		}
		if w.Body.Len() != 0 {
			t.Errorf("%s: body %q, want empty", method, w.Body.String())
		}
	}
	w, err := serve(t, h, httptest.NewRequest("GET", "/empty?r=123", nil))
	if code := status(t, w, err); code != 200 {
		t.Fatalf("ping: status %d", code)
	}
}

func TestEmptyTooLarge(t *testing.T) {
	h := provisioned(t, &Handler{MaxUploadSize: chunkSize})
	w, err := serve(t, h, httptest.NewRequest("POST", "/empty", bytes.NewReader(make([]byte, chunkSize+1))))
	if code := status(t, w, err); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d, want 413", code)
	}
}

type brokenBody struct{}

func (brokenBody) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestEmptyAbortedUpload(t *testing.T) {
	h := provisioned(t, &Handler{})
	w, err := serve(t, h, httptest.NewRequest("POST", "/empty", brokenBody{}))
	// 4xx, so Caddy logs it at debug level: an aborted upload is routine.
	if code := status(t, w, err); code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", code)
	}
}

func TestDefaultMaxUploadSize(t *testing.T) {
	h := provisioned(t, &Handler{})
	if h.MaxUploadSize != DefaultMaxUploadSize {
		t.Fatalf("MaxUploadSize = %d, want %d", h.MaxUploadSize, DefaultMaxUploadSize)
	}
}

func TestGetIP(t *testing.T) {
	h := provisioned(t, &Handler{})
	for _, target := range []string{"/getIP", "/backend/getIP.php?isp&distance=km"} {
		w, err := serve(t, h, httptest.NewRequest("GET", target, nil))
		if err != nil {
			t.Fatal(err)
		}
		var got struct {
			ProcessedString string `json:"processedString"`
			RawISPInfo      string `json:"rawIspInfo"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("%s: %v: %q", target, err, w.Body.String())
		}
		if got.ProcessedString != "203.0.113.7" || got.RawISPInfo != "" {
			t.Errorf("%s: got %+v", target, got)
		}
		if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("Content-Type = %q", ct)
		}
	}
}

func TestGetIPWithoutClientIP(t *testing.T) {
	h := provisioned(t, &Handler{})
	w := httptest.NewRecorder()
	err := h.ServeHTTP(w, httptest.NewRequest("GET", "/getIP", nil), nil)
	if code := status(t, w, err); code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500", code)
	}
}

func TestCORS(t *testing.T) {
	h := provisioned(t, &Handler{})
	for _, target := range []string{"/garbage?cors&ckSize=1", "/empty?cors", "/getIP?cors"} {
		w, err := serve(t, h, httptest.NewRequest("GET", target, nil))
		if err != nil {
			t.Fatal(err)
		}
		for k, want := range map[string]string{
			"Access-Control-Allow-Origin": "*",
			"Timing-Allow-Origin":         "*",
		} {
			if got := w.Header().Get(k); got != want {
				t.Errorf("%s: %s = %q, want %q", target, k, got, want)
			}
		}
	}
}

func TestPreflight(t *testing.T) {
	h := provisioned(t, &Handler{})
	r := httptest.NewRequest("OPTIONS", "/empty?cors&r=1", nil)
	r.Header.Set("Origin", "https://other.example")
	r.Header.Set("Access-Control-Request-Method", "POST")
	r.Header.Set("Access-Control-Request-Headers", "content-type")
	w, err := serve(t, h, r)
	if err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusNoContent {
		t.Fatalf("status %d, want 204", w.Code)
	}
	if !strings.Contains(w.Header().Get("Access-Control-Allow-Methods"), "POST") ||
		!strings.Contains(w.Header().Get("Access-Control-Allow-Headers"), "Content-Type") {
		t.Fatalf("preflight headers: %v", w.Header())
	}
}

func TestMethodNotAllowed(t *testing.T) {
	h := provisioned(t, &Handler{})
	for _, c := range []struct{ method, target string }{
		{"POST", "/garbage"},
		{"DELETE", "/empty"},
		{"POST", "/getIP"},
		{"POST", "/"},
	} {
		w, err := serve(t, h, httptest.NewRequest(c.method, c.target, nil))
		if code := status(t, w, err); code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s: status %d, want 405", c.method, c.target, code)
		}
		if w.Header().Get("Allow") == "" {
			t.Errorf("%s %s: no Allow header", c.method, c.target)
		}
	}
}

func TestUI(t *testing.T) {
	h := provisioned(t, &Handler{})
	for target, ctype := range map[string]string{
		"/":                       "text/html",
		"/index.html":             "text/html",
		"/speedtest/":             "text/html",
		"/app.js":                 "text/javascript",
		"/speedtest/speedtest.js": "text/javascript",
		"/style.css":              "text/css",
		"/favicon.svg":            "image/svg+xml",
		"/config.json":            "application/json",
	} {
		w, err := serve(t, h, httptest.NewRequest("GET", target, nil))
		if err != nil {
			t.Fatalf("%s: %v", target, err)
		}
		if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), ctype) {
			t.Errorf("%s: status %d, Content-Type %q", target, w.Code, w.Header().Get("Content-Type"))
		}
		hasCSP := w.Header().Get("Content-Security-Policy") != ""
		if hasCSP != (ctype == "text/html") {
			t.Errorf("%s: Content-Security-Policy present = %v", target, hasCSP)
		}
	}
	w, err := serve(t, h, httptest.NewRequest("GET", "/nope.html", nil))
	if code := status(t, w, err); code != 404 {
		t.Errorf("unknown file: status %d, want 404", code)
	}
}

func TestUIRevalidates(t *testing.T) {
	h := provisioned(t, &Handler{})
	w, _ := serve(t, h, httptest.NewRequest("GET", "/app.js", nil))
	tag := w.Header().Get("ETag")
	if tag == "" {
		t.Fatal("no ETag")
	}
	r := httptest.NewRequest("GET", "/app.js", nil)
	r.Header.Set("If-None-Match", tag)
	w, _ = serve(t, h, r)
	if w.Code != http.StatusNotModified {
		t.Fatalf("status %d, want 304", w.Code)
	}
}

func TestUIOff(t *testing.T) {
	h := provisioned(t, &Handler{DisableUI: true})
	for _, target := range []string{"/", "/app.js", "/config.json"} {
		w, err := serve(t, h, httptest.NewRequest("GET", target, nil))
		if code := status(t, w, err); code != 404 {
			t.Errorf("%s: status %d, want 404", target, code)
		}
	}
	w, err := serve(t, h, httptest.NewRequest("GET", "/getIP", nil))
	if code := status(t, w, err); code != 200 {
		t.Errorf("API with ui off: status %d", code)
	}
}

func TestConfigJSON(t *testing.T) {
	h := provisioned(t, &Handler{
		MaxUploadSize: 10 << 20,
		Servers: []Server{
			{"San Francisco", "https://sf.example/"},
			{"Helsinki", "https://hel.example/speedtest/"},
		},
	})
	w, err := serve(t, h, httptest.NewRequest("GET", "/config.json", nil))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"servers":[{"name":"San Francisco","url":"https://sf.example/"},` +
		`{"name":"Helsinki","url":"https://hel.example/speedtest/"}],"maxUploadSize":10485760}`
	if got := w.Body.String(); got != want {
		t.Fatalf("config.json:\n got %s\nwant %s", got, want)
	}

	h = provisioned(t, &Handler{})
	w, _ = serve(t, h, httptest.NewRequest("GET", "/config.json", nil))
	if !strings.Contains(w.Body.String(), `"servers":[]`) {
		t.Fatalf("no servers should be an empty list, got %s", w.Body.String())
	}
}

func TestValidate(t *testing.T) {
	for _, s := range []Server{
		{"", "https://a.example/"},
		{"A", "a.example/"},
		{"A", "ftp://a.example/"},
		{"A", "https://a.example"},
		{"A", "https://a.example/x"},
		{"A", "https://a.example/?x=1"},
		{"A", "https:///"},
	} {
		h := &Handler{Servers: []Server{s}}
		if err := h.Validate(); err == nil {
			t.Errorf("Validate accepted %+v", s)
		}
	}
	if err := (&Handler{MaxUploadSize: -1}).Validate(); err == nil {
		t.Error("Validate accepted a negative max_upload_size")
	}
	ok := &Handler{Servers: []Server{{"A", "http://a.example/"}, {"B", "https://b.example/sub/"}}}
	if err := ok.Validate(); err != nil {
		t.Errorf("Validate rejected valid servers: %v", err)
	}
}

func TestCaddyfile(t *testing.T) {
	var h Handler
	err := h.UnmarshalCaddyfile(caddyfile.NewTestDispenser(`librespeed {
		max_upload_size 32MiB
		ui off
		server "San Francisco" https://sf.example/
		server Helsinki https://hel.example/
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if h.MaxUploadSize != 32<<20 || !h.DisableUI || len(h.Servers) != 2 ||
		h.Servers[0] != (Server{"San Francisco", "https://sf.example/"}) {
		t.Fatalf("got %+v", h)
	}

	h = Handler{}
	if err := h.UnmarshalCaddyfile(caddyfile.NewTestDispenser(`librespeed`)); err != nil {
		t.Fatal(err)
	}
	if h.MaxUploadSize != 0 || h.DisableUI || h.Servers != nil {
		t.Fatalf("bare directive set options: %+v", h)
	}

	// Each option goes in a real block: a Caddyfile's closing brace must be
	// on its own line, or the dispenser reads it as one more argument.
	bads := []string{"librespeed extra"}
	for _, option := range []string{
		"max_upload_size",
		"max_upload_size lots",
		"max_upload_size 0",
		"max_upload_size 1MiB 2MiB",
		"ui maybe",
		"ui",
		"server OnlyName",
		"server A https://a.example/ extra",
		"bogus",
	} {
		bads = append(bads, "librespeed {\n"+option+"\n}")
	}
	for _, bad := range bads {
		var h Handler
		if err := h.UnmarshalCaddyfile(caddyfile.NewTestDispenser(bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

// discard is a ResponseWriter that throws the body away, to time the handler
// rather than a buffer.
type discard struct{ h http.Header }

func (d *discard) Header() http.Header         { return d.h }
func (d *discard) Write(b []byte) (int, error) { return len(b), nil }
func (d *discard) WriteHeader(int)             {}

func BenchmarkGarbage(b *testing.B) {
	h := provisioned(b, &Handler{})
	r := httptest.NewRequest("GET", "/garbage?ckSize=100", nil)
	b.SetBytes(100 * chunkSize)
	for b.Loop() {
		if err := h.ServeHTTP(&discard{http.Header{}}, r, nil); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkEmpty(b *testing.B) {
	h := provisioned(b, &Handler{})
	payload := make([]byte, 20*chunkSize)
	b.SetBytes(int64(len(payload)))
	for b.Loop() {
		r := httptest.NewRequest("POST", "/empty", bytes.NewReader(payload))
		if err := h.ServeHTTP(&discard{http.Header{}}, r, nil); err != nil {
			b.Fatal(err)
		}
	}
}
