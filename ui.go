package librespeed

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

//go:embed ui
var uiFS embed.FS

// contentTypes is explicit rather than package mime's, which reads the host's
// tables (on Windows, the registry can map .js to text/plain).
var contentTypes = map[string]string{
	".html": "text/html; charset=utf-8",
	".js":   "text/javascript; charset=utf-8",
	".css":  "text/css; charset=utf-8",
	".svg":  "image/svg+xml",
}

type asset struct {
	body  []byte
	ctype string
	etag  string
}

// assets maps a UI file's name to its content. The UI is flat (no
// subdirectories) and refers to its files by relative name, so it works at
// any path prefix.
var assets = func() map[string]asset {
	files, err := fs.ReadDir(uiFS, "ui")
	if err != nil {
		panic(err)
	}
	m := make(map[string]asset, len(files))
	for _, f := range files {
		if f.IsDir() {
			panic("librespeed: the embedded UI must be flat, found directory " + f.Name())
		}
		body, err := uiFS.ReadFile("ui/" + f.Name())
		if err != nil {
			panic(err)
		}
		ctype, ok := contentTypes[path.Ext(f.Name())]
		if !ok {
			panic("librespeed: no content type for " + f.Name())
		}
		m[f.Name()] = asset{body, ctype, etag(body)}
	}
	if _, ok := m["index.html"]; !ok {
		panic("librespeed: the embedded UI has no index.html")
	}
	return m
}()

func etag(b []byte) string {
	sum := sha256.Sum256(b)
	return `"` + base64.RawURLEncoding.EncodeToString(sum[:12]) + `"`
}

// contentSecurityPolicy allows the UI's own files, plus connections to any
// server, since it can test against other servers.
const contentSecurityPolicy = "default-src 'self'; connect-src *; img-src 'self' data:; " +
	"base-uri 'none'; form-action 'none'; frame-ancestors 'self'"

func (h *Handler) serveUI(w http.ResponseWriter, r *http.Request) error {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		return caddyhttp.Error(http.StatusMethodNotAllowed, nil)
	}

	name := path.Base(r.URL.Path)
	if strings.HasSuffix(r.URL.Path, "/") {
		name = "index.html"
	}

	hdr := w.Header()
	hdr.Set("X-Content-Type-Options", "nosniff")

	var a asset
	if name == "config.json" {
		a = asset{h.config, "application/json; charset=utf-8", etag(h.config)}
	} else if found, ok := assets[name]; ok {
		a = found
	} else {
		return caddyhttp.Error(http.StatusNotFound, nil)
	}

	if strings.HasPrefix(a.ctype, "text/html") {
		hdr.Set("Content-Security-Policy", contentSecurityPolicy)
	}
	hdr.Set("Content-Type", a.ctype)
	hdr.Set("ETag", a.etag)
	// Always revalidate, so a new release reaches browsers on the next load.
	hdr.Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(a.body))
	return nil
}
