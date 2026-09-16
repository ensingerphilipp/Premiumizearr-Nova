package service

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ensingerphilipp/premiumizearr-nova/internal/config"
)

// TestNormalizeWebRoot verifies the canonical form of the configured
// WebRoot shared by the index template and the SPA handler.
func TestNormalizeWebRoot(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{in: "", want: ""},
		{in: "nova", want: "/nova"},
		{in: "/nova", want: "/nova"},
		{in: "/nova/", want: "/nova"},
		{in: "nova/", want: "/nova"},
		{in: "  nova  ", want: "/nova"},
		{in: "/", want: ""},
	}
	for _, c := range cases {
		if got := normalizeWebRoot(c.in); got != c.want {
			t.Errorf("normalizeWebRoot(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

const (
	webRootTestBundleJS  = "console.log('premiumizearr-nova bundle');\n"
	webRootTestBundleCSS = "body { margin: 0; }\n"
)

// matchesContentType reports whether ct starts with any of the given
// content types (the charset suffix varies by platform mime database).
func matchesContentType(ct string, want []string) bool {
	for _, w := range want {
		if strings.HasPrefix(ct, w) {
			return true
		}
	}
	return false
}

// TestWebRootServesAssets is a regression test for issue #90: with any
// non-empty WebRoot the rendered index must reference the assets by
// absolute URL, and those URLs must serve the actual asset files instead
// of the SPA fallback index.html (which rendered a blank page).
func TestWebRootServesAssets(t *testing.T) {
	templateSrc, err := os.ReadFile(filepath.Join("..", "..", "web", "public", "index.html"))
	if err != nil {
		t.Fatalf("reading web/public/index.html: %v", err)
	}
	faviconSrc, err := os.ReadFile(filepath.Join("..", "..", "web", "public", "favicon.png"))
	if err != nil {
		t.Fatalf("reading web/public/favicon.png: %v", err)
	}

	cases := []struct {
		name    string
		webRoot string
	}{
		{name: "empty", webRoot: ""},
		{name: "relative", webRoot: "nova"},
		{name: "absolute", webRoot: "/nova"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			staticDir := filepath.Join(dir, "static")
			if err := os.MkdirAll(staticDir, 0o755); err != nil {
				t.Fatalf("creating static dir: %v", err)
			}
			for name, content := range map[string][]byte{
				"index.html":  templateSrc,
				"bundle.js":   []byte(webRootTestBundleJS),
				"bundle.css":  []byte(webRootTestBundleCSS),
				"favicon.png": faviconSrc,
			} {
				if err := os.WriteFile(filepath.Join(staticDir, name), content, 0o644); err != nil {
					t.Fatalf("writing %s: %v", name, err)
				}
			}
			// The web server loads ./static/index.html relative to the process CWD.
			t.Chdir(dir)

			s := WebServerService{}.New()
			s.Init(nil, nil, nil, &config.Config{
				BindIP:   "127.0.0.1",
				BindPort: "0",
				WebRoot:  tc.webRoot,
			})
			s.Start()
			defer s.srv.Close()

			prefix := normalizeWebRoot(tc.webRoot)
			base := fmt.Sprintf("http://127.0.0.1:%d", s.listener.Addr().(*net.TCPAddr).Port)
			client := &http.Client{Timeout: 10 * time.Second}

			// The rendered index must reference every asset by absolute URL.
			pageResp, err := client.Get(base + prefix + "/")
			if err != nil {
				t.Fatalf("GET %s: %v", base+prefix+"/", err)
			}
			page, err := io.ReadAll(pageResp.Body)
			pageResp.Body.Close()
			if err != nil {
				t.Fatalf("reading index body: %v", err)
			}
			if pageResp.StatusCode != http.StatusOK {
				t.Fatalf("GET %s status = %d, want %d", base+prefix+"/", pageResp.StatusCode, http.StatusOK)
			}
			for _, wantRef := range []string{
				fmt.Sprintf("href='%s/favicon.png'", prefix),
				fmt.Sprintf("href='%s/bundle.css'", prefix),
				fmt.Sprintf("src='%s/bundle.js'", prefix),
			} {
				if !strings.Contains(string(page), wantRef) {
					t.Errorf("served index does not reference %q:\n%s", wantRef, page)
				}
			}

			// Each referenced asset URL must serve the real file, not the SPA fallback.
			// wantContentTypes accepts every Content-Type the Go mime database may
			// report for the asset extension on this system (e.g. .js is
			// application/javascript or text/javascript depending on /etc/mime.types).
			assertServesFile := func(asset string, wantContentTypes []string, wantBody string) {
				t.Helper()
				assetResp, err := client.Get(base + prefix + asset)
				if err != nil {
					t.Fatalf("GET %s: %v", base+prefix+asset, err)
				}
				defer assetResp.Body.Close()
				if assetResp.StatusCode != http.StatusOK {
					t.Fatalf("GET %s status = %d, want %d", base+prefix+asset, assetResp.StatusCode, http.StatusOK)
				}
				if ct := assetResp.Header.Get("Content-Type"); !matchesContentType(ct, wantContentTypes) {
					t.Errorf("GET %s Content-Type = %q, want one of %v", base+prefix+asset, ct, wantContentTypes)
				}
				body, err := io.ReadAll(assetResp.Body)
				if err != nil {
					t.Fatalf("reading %s body: %v", asset, err)
				}
				if string(body) != wantBody {
					t.Errorf("GET %s body is not the asset file (SPA fallback served instead?)", base+prefix+asset)
				}
			}
			assertServesFile("/bundle.js", []string{"application/javascript", "text/javascript"}, webRootTestBundleJS)
			assertServesFile("/bundle.css", []string{"text/css"}, webRootTestBundleCSS)
			assertServesFile("/favicon.png", []string{"image/png"}, string(faviconSrc))
		})
	}
}
