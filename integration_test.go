package librespeed

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2/caddytest"
)

// Runs a real Caddy from a Caddyfile: the directive is registered, ordered
// ahead of file_server, and serves over HTTP.
func TestIntegration(t *testing.T) {
	tester := caddytest.NewTester(t)
	tester.InitServer(`
	{
		skip_install_trust
		admin localhost:2999
		http_port 9080
		https_port 9443
		grace_period 1ns
	}
	http://localhost:9080 {
		handle /backend/* {
			librespeed {
				ui off
			}
		}
		handle {
			librespeed {
				max_upload_size 2MiB
				server "This one" http://localhost:9080/
			}
			file_server
		}
	}
	`, "caddyfile")

	res, err := tester.Client.Get("http://localhost:9080/backend/garbage.php?ckSize=2&cors")
	if err != nil {
		t.Fatal(err)
	}
	n, _ := io.Copy(io.Discard, res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || n != 2*chunkSize || res.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("garbage: status %d, %d bytes, headers %v", res.StatusCode, n, res.Header)
	}

	tester.AssertGetResponse("http://localhost:9080/backend/getIP", 200,
		`{"processedString":"127.0.0.1","rawIspInfo":""}`)
	tester.AssertGetResponse("http://localhost:9080/backend/", 404, "")

	tester.AssertPostResponseBody("http://localhost:9080/empty", nil,
		bytes.NewBuffer(make([]byte, chunkSize)), 200, "")
	tester.AssertPostResponseBody("http://localhost:9080/empty", nil,
		bytes.NewBuffer(make([]byte, 3*chunkSize)), http.StatusRequestEntityTooLarge, "")

	res, body := tester.AssertGetResponse("http://localhost:9080/config.json", 200,
		`{"servers":[{"name":"This one","url":"http://localhost:9080/"}],"maxUploadSize":2097152}`)
	_ = res
	_ = body

	res, err = tester.Client.Get("http://localhost:9080/")
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || !strings.Contains(string(page), `<script type="module" src="app.js">`) {
		t.Fatalf("UI: status %d, body %.200q", res.StatusCode, page)
	}
}
