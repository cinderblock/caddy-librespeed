package librespeed

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/certmagic"
)

// TestMain keeps Caddy's state out of the real user directories. caddytest
// runs a full Caddy, which otherwise autosaves its config to the user's
// AppConfigDir and creates a local CA and storage under AppDataDir.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "caddy-librespeed-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	// Read at runtime (e.g. instance.uuid).
	os.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	os.Setenv("XDG_DATA_HOME", filepath.Join(dir, "data"))
	// Computed when package caddy initialized, before this ran.
	caddy.ConfigAutosavePath = filepath.Join(dir, "config", "caddy", "autosave.json")
	caddy.DefaultStorage = &certmagic.FileStorage{Path: filepath.Join(dir, "data", "caddy")}

	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
