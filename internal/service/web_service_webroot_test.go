package service

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
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
		{in: "/nova//", want: "/nova"},
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

// TestValidateWebRoot covers the literal allowlist enforced before a
// WebRoot is used as a mux route pattern (review findings R2-1/R2-5):
// mux metacharacters ({ } ( ) + |), malformed path syntax and repeated
// slashes must be rejected; the empty string and normal nested paths are
// accepted.
func TestValidateWebRoot(t *testing.T) {
	valid := []struct {
		in   string
		want string
	}{
		{in: "", want: ""},
		{in: "/", want: ""},
		{in: "nova", want: "/nova"},
		{in: "/nova", want: "/nova"},
		{in: "/nova/", want: "/nova"},
		{in: "/nova//", want: "/nova"},
		{in: "  /apps/nova  ", want: "/apps/nova"},
		{in: "/a-b.c_d", want: "/a-b.c_d"},
		{in: "/v1/2", want: "/v1/2"},
	}
	for _, c := range valid {
		got, err := validateWebRoot(c.in)
		if err != nil {
			t.Errorf("validateWebRoot(%q) unexpected error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("validateWebRoot(%q) = %q, want %q", c.in, got, c.want)
		}
	}

	invalid := []string{
		"/{x:(y)}", // R2-1: panics mux route construction (NumSubexp check)
		"{",
		"}",
		"(",
		")",
		"+",
		"|",
		"/nova{x}",
		"/nova|other",
		"/nova+x",
		"///nova",  // R2-5: repeated slashes
		"/nova//x", // R2-5: repeated slashes
		"/nova x",  // whitespace inside a segment
		"/nova?y",
		"/nova#y",
		"/nova\\y",
		"/nova~x",
		"/nøva", // non-ASCII
	}
	for _, in := range invalid {
		if got, err := validateWebRoot(in); err == nil {
			t.Errorf("validateWebRoot(%q) = %q, want error", in, got)
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

// startTestServer starts the real web server on an ephemeral port with the
// given WebRoot and returns the base URL (http://127.0.0.1:port) and the
// service. The real index.html template is served together with sentinel
// bundle assets.
func startTestServer(t *testing.T, webRoot string) (string, *WebServerService) {
	t.Helper()
	templateSrc, err := os.ReadFile(filepath.Join("..", "..", "web", "public", "index.html"))
	if err != nil {
		t.Fatalf("reading web/public/index.html: %v", err)
	}
	faviconSrc, err := os.ReadFile(filepath.Join("..", "..", "web", "public", "favicon.png"))
	if err != nil {
		t.Fatalf("reading web/public/favicon.png: %v", err)
	}

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
		WebRoot:  webRoot,
	})
	s.Start()
	if s.srv == nil {
		t.Fatalf("web server failed to start on an ephemeral port")
	}
	t.Cleanup(func() { s.srv.Close() })
	return fmt.Sprintf("http://127.0.0.1:%d", s.listener.Addr().(*net.TCPAddr).Port), &s
}

// TestWebRootServesAssets is a regression test for issue #90 and review
// finding R1-3: with any non-empty WebRoot the rendered index must
// reference the assets by an absolute "/<webRoot>/" URL, and those URLs
// must serve the actual asset files instead of the SPA fallback index.html
// (which rendered a blank page). With an empty WebRoot the references must
// stay document-relative ("./...") so the documented reverse-proxy
// deployment (proxy strips the path prefix, app served under a subpath)
// keeps working.
func TestWebRootServesAssets(t *testing.T) {
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
			base, _ := startTestServer(t, tc.webRoot)

			prefix := normalizeWebRoot(tc.webRoot)
			assetBase := "./"
			if prefix != "" {
				assetBase = prefix + "/"
			}
			client := &http.Client{Timeout: 10 * time.Second}

			// The rendered index must reference every asset by a URL that
			// resolves correctly from the page.
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
				fmt.Sprintf("href='%sfavicon.png'", assetBase),
				fmt.Sprintf("href='%sbundle.css'", assetBase),
				fmt.Sprintf("src='%sbundle.js'", assetBase),
			} {
				if !strings.Contains(string(page), wantRef) {
					t.Errorf("served index does not reference %q:\n%s", wantRef, page)
				}
			}

			// Each referenced asset URL must serve the real file, not the
			// SPA fallback. (For an empty WebRoot the page is served at the
			// host root, so the document-relative refs resolve to the same
			// "/<asset>" URLs the browser requests.)
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

// TestWebRootAPIRoutes is a regression test for review finding R1-1: with a
// non-empty WebRoot the UI is served under /<webRoot>/ and the front-end
// calls the API relative to the page location, so the /api routes must be
// reachable under the normalized webRoot as well. The root /api
// registrations are the existing contract and must be kept.
func TestWebRootAPIRoutes(t *testing.T) {
	for _, webRoot := range []string{"", "nova", "/nova"} {
		t.Run(fmt.Sprintf("webRoot=%q", webRoot), func(t *testing.T) {
			base, _ := startTestServer(t, webRoot)
			prefix := normalizeWebRoot(webRoot)
			client := &http.Client{Timeout: 10 * time.Second}

			assertAPIConfig := func(apiURL string) {
				t.Helper()
				resp, err := client.Get(apiURL)
				if err != nil {
					t.Fatalf("GET %s: %v", apiURL, err)
				}
				defer resp.Body.Close()
				body, err := io.ReadAll(resp.Body)
				if err != nil {
					t.Fatalf("reading body of %s: %v", apiURL, err)
				}
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("GET %s status = %d, want %d", apiURL, resp.StatusCode, http.StatusOK)
				}
				// The SPA fallback serves index.html; the API serves JSON.
				if !json.Valid(body) {
					t.Fatalf("GET %s returned non-JSON (SPA fallback served instead?): %q", apiURL, string(body))
				}
				if want := fmt.Sprintf(`"WebRoot":%q`, webRoot); !strings.Contains(string(body), want) {
					t.Errorf("GET %s body missing %s:\n%s", apiURL, want, body)
				}
			}
			assertAPIPoll := func(apiURL string) {
				t.Helper()
				resp, err := client.Post(apiURL, "application/json", nil)
				if err != nil {
					t.Fatalf("POST %s: %v", apiURL, err)
				}
				defer resp.Body.Close()
				// The test server has no directory watcher, so the registered
				// endpoint must answer 503 instead of serving the SPA page.
				if resp.StatusCode != http.StatusServiceUnavailable {
					t.Fatalf("POST %s status = %d, want %d", apiURL, resp.StatusCode, http.StatusServiceUnavailable)
				}
			}
			assertAPIPollMethodNotAllowed := func(apiURL string) {
				t.Helper()
				resp, err := client.Get(apiURL)
				if err != nil {
					t.Fatalf("GET %s: %v", apiURL, err)
				}
				defer resp.Body.Close()
				// R1-2: without a mux method filter a non-POST poll request
				// reaches the handler's 405 branch; with one it would fall
				// through to the SPA catch-all (200 index.html).
				if resp.StatusCode != http.StatusMethodNotAllowed {
					t.Fatalf("GET %s status = %d, want %d", apiURL, resp.StatusCode, http.StatusMethodNotAllowed)
				}
			}

			// Root registrations are the existing contract.
			assertAPIConfig(base + "/api/config")
			assertAPIPoll(base + "/api/blackhole/poll")
			assertAPIPollMethodNotAllowed(base + "/api/blackhole/poll")
			// The UI under /<webRoot>/ calls the API relative to the page.
			if prefix != "" {
				assertAPIConfig(base + prefix + "/api/config")
				assertAPIPoll(base + prefix + "/api/blackhole/poll")
				assertAPIPollMethodNotAllowed(base + prefix + "/api/blackhole/poll")
			}
		})
	}
}

// TestWebServerBindFailureDoesNotExitDaemon is a regression test for review
// finding R1-2: a web bind failure (e.g. the port taken during the
// config-update restart, which runs inside an HTTP handler goroutine) must
// log and return, not os.Exit the whole daemon. s.srv must stay nil so
// ConfigUpdatedCallback can skip the Close of a server that was never up.
func TestWebServerBindFailureDoesNotExitDaemon(t *testing.T) {
	templateSrc, err := os.ReadFile(filepath.Join("..", "..", "web", "public", "index.html"))
	if err != nil {
		t.Fatalf("reading web/public/index.html: %v", err)
	}

	dir := t.TempDir()
	staticDir := filepath.Join(dir, "static")
	if err := os.MkdirAll(staticDir, 0o755); err != nil {
		t.Fatalf("creating static dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(staticDir, "index.html"), templateSrc, 0o644); err != nil {
		t.Fatalf("writing index.html: %v", err)
	}
	t.Chdir(dir)

	// Occupy a port so net.Listen fails with address-in-use.
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	defer blocker.Close()
	port := blocker.Addr().(*net.TCPAddr).Port

	cfg := &config.Config{
		BindIP:   "127.0.0.1",
		BindPort: strconv.Itoa(port),
	}
	s := WebServerService{}.New()
	s.Init(nil, nil, nil, cfg)
	s.Start() // must return, not os.Exit (old code: log.Fatal -> os.Exit(1))
	if s.srv != nil {
		t.Fatalf("s.srv set after bind failure, want nil")
	}
	if s.listener != nil {
		t.Fatalf("s.listener set after bind failure, want nil")
	}

	// ConfigUpdatedCallback must survive a nil server: skip the Close,
	// retry the Start, hit the same bind failure, and return. The WebRoot
	// must differ so the callback takes the restart branch.
	oldCfg := *cfg
	cfg.WebRoot = "other"
	s.ConfigUpdatedCallback(oldCfg, *cfg)
	if s.srv != nil {
		t.Fatalf("s.srv set after ConfigUpdatedCallback with failed bind, want nil")
	}
}

// TestConfigUpdateWithMalformedWebRootKeepsServerRunning is a regression
// test for review finding R2-1: an unauthenticated POST /api/config can
// set any WebRoot, which is then used as a mux route pattern on restart.
// A value with mux metacharacters used to panic route construction after
// the old server was already closed, taking down the whole web surface
// (UI + all /api) until process restart. The update must be rejected and
// the running server must keep serving.
func TestConfigUpdateWithMalformedWebRootKeepsServerRunning(t *testing.T) {
	base, s := startTestServer(t, "nova")
	client := &http.Client{Timeout: 10 * time.Second}

	assertSurfaceUp := func() {
		t.Helper()
		for _, url := range []string{base + "/nova/", base + "/nova/api/config", base + "/api/config"} {
			resp, err := client.Get(url)
			if err != nil {
				t.Fatalf("GET %s after rejected update: %v", url, err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("GET %s after rejected update status = %d, want %d", url, resp.StatusCode, http.StatusOK)
			}
		}
	}

	assertSurfaceUp()

	// The config package applies the new config to the shared struct
	// before invoking the callbacks, so s.config carries the (malformed)
	// WebRoot by the time the restart runs.
	oldCfg := *s.config
	for _, bad := range []string{"/{x:(y)}", "}{", "///nova", "/nova|other"} {
		s.config.WebRoot = bad
		s.ConfigUpdatedCallback(oldCfg, *s.config)
		if s.srv == nil {
			t.Fatalf("s.srv is nil after rejected update with WebRoot %q — the running server was torn down", bad)
		}
		assertSurfaceUp()
	}

	// A subsequent valid update (nested path) restarts the server normally.
	s.config.WebRoot = "/apps/nova"
	s.ConfigUpdatedCallback(oldCfg, *s.config)
	if s.srv == nil {
		t.Fatal("s.srv nil after valid update with nested WebRoot /apps/nova")
	}
	newBase := fmt.Sprintf("http://127.0.0.1:%d", s.listener.Addr().(*net.TCPAddr).Port)
	resp, err := client.Get(newBase + "/apps/nova/")
	if err != nil {
		t.Fatalf("GET %s after valid update: %v", newBase+"/apps/nova/", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s after valid update status = %d, want %d", newBase+"/apps/nova/", resp.StatusCode, http.StatusOK)
	}
}

// TestWebServerRestartBindFailureAndRecovery is a regression test for
// review finding R2-4: it drives the live restart path that a cold start
// never exercises — non-nil s.srv → Close → failed bind → and recovery of
// the web surface on the next config update.
func TestWebServerRestartBindFailureAndRecovery(t *testing.T) {
	base, s := startTestServer(t, "nova")
	client := &http.Client{Timeout: 10 * time.Second}

	// Occupy a port so the restarted server cannot bind.
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	defer blocker.Close()
	blockedPort := strconv.Itoa(blocker.Addr().(*net.TCPAddr).Port)

	cfg := s.config
	oldCfg := *cfg
	// The config package applies the new config to the shared struct
	// before invoking the callbacks.
	cfg.BindPort = blockedPort
	s.ConfigUpdatedCallback(oldCfg, *cfg)

	if s.srv != nil {
		t.Fatalf("s.srv set after restart with failed bind, want nil")
	}
	// The old server was closed when the restart began.
	if resp, err := client.Get(base + "/nova/"); err == nil {
		resp.Body.Close()
		t.Fatalf("old server still serving after restart with failed bind")
	}

	// Recovery: a subsequent update to a free port brings the surface back.
	oldCfg = *cfg
	cfg.BindPort = "0"
	s.ConfigUpdatedCallback(oldCfg, *cfg)
	if s.srv == nil {
		t.Fatalf("s.srv nil after recovery restart")
	}
	newBase := fmt.Sprintf("http://127.0.0.1:%d", s.listener.Addr().(*net.TCPAddr).Port)
	resp, err := client.Get(newBase + "/nova/")
	if err != nil {
		t.Fatalf("GET %s after recovery: %v", newBase+"/nova/", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s after recovery status = %d, want %d", newBase+"/nova/", resp.StatusCode, http.StatusOK)
	}
}
