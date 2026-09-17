package service

import (
	"bytes"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ensingerphilipp/premiumizearr-nova/internal/config"
	"github.com/gorilla/mux"
	log "github.com/sirupsen/logrus"
)

type IndexTemplates struct {
	// AssetBase is the URL prefix for the asset references rendered into
	// index.html: "./" when the app is served at the host root (or behind
	// a reverse proxy that strips the path prefix), "/<webRoot>/" when a
	// WebRoot is configured.
	AssetBase string
}

var indexBytes []byte

type WebServerService struct {
	transferManager         *TransferManagerService
	directoryWatcherService *DirectoryWatcherService
	arrsManagerService      *ArrsManagerService
	config                  *config.Config
	srv                     *http.Server
	listener                net.Listener
}

// normalizeWebRoot canonicalizes the configured WebRoot to the absolute
// URL prefix shared by the rendered index template and the SPA handler:
// trimmed, without trailing slashes, and with a leading slash when
// non-empty, so assets are referenced as absolute URLs like "/nova/bundle.js".
func normalizeWebRoot(webRoot string) string {
	webRoot = strings.TrimSpace(webRoot)
	webRoot = strings.TrimRight(webRoot, "/")
	if webRoot != "" && !strings.HasPrefix(webRoot, "/") {
		webRoot = "/" + webRoot
	}
	return webRoot
}

// validateWebRoot returns the canonical WebRoot usable as a mux route
// pattern, or an error. The WebRoot becomes a gorilla/mux route pattern on
// every (re)start (see issue #90 review R2-1), so values with mux
// metacharacters like "/{x:(y)}" must be rejected before they can panic
// route construction or otherwise take down the running web surface.
// Allowed: the empty string (host root) and plain nested paths such as
// "/apps/nova" — non-empty segments of ASCII letters, digits, ".", "_" and
// "-", separated by single slashes.
func validateWebRoot(webRoot string) (string, error) {
	webRoot = normalizeWebRoot(webRoot)
	if webRoot == "" {
		return "", nil
	}
	for _, segment := range strings.Split(strings.TrimPrefix(webRoot, "/"), "/") {
		if segment == "" {
			return "", fmt.Errorf("invalid WebRoot %q: repeated slashes are not allowed (expected a path like \"/apps/nova\")", webRoot)
		}
		for _, r := range segment {
			if !isWebRootSegmentChar(r) {
				return "", fmt.Errorf("invalid WebRoot %q: path segments may only contain ASCII letters, digits, \".\", \"_\" and \"-\" (found %q)", webRoot, string(r))
			}
		}
	}
	return webRoot, nil
}

func isWebRootSegmentChar(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
		r == '.' || r == '_' || r == '-'
}

func (s WebServerService) New() WebServerService {
	s.config = nil
	s.transferManager = nil
	s.directoryWatcherService = nil
	s.arrsManagerService = nil
	s.srv = nil
	s.listener = nil
	return s
}

func (s *WebServerService) ConfigUpdatedCallback(currentConfig config.Config, newConfig config.Config) {
	if currentConfig.BindIP != newConfig.BindIP ||
		currentConfig.BindPort != newConfig.BindPort ||
		currentConfig.WebRoot != newConfig.WebRoot {
		// Validate before touching the running server: a rejected change
		// must leave the current web surface up (R2-1).
		if _, err := validateWebRoot(newConfig.WebRoot); err != nil {
			log.Errorf("web server not restarted: %v", err)
			return
		}
		log.Tracef("Config updated, restarting web server...")
		// s.srv is nil when the (re)start never got a listener, e.g. the
		// previous bind attempt failed.
		if s.srv != nil {
			if err := s.srv.Close(); err != nil {
				log.Errorf("could not close web server during config update: %v", err)
			}
		}
		s.Start()
	}
}

func (s *WebServerService) Init(transferManager *TransferManagerService, directoryWatcher *DirectoryWatcherService, arrManager *ArrsManagerService, config *config.Config) {
	s.transferManager = transferManager
	s.directoryWatcherService = directoryWatcher
	s.arrsManagerService = arrManager
	s.config = config
}

func (s *WebServerService) Start() {
	// Reject a WebRoot that could not serve as a mux route pattern before
	// doing anything that mutates server state. Log and return instead of
	// log.Fatal: on the config-update path this runs in an HTTP handler
	// goroutine, and on a bad config.yaml the other services (arr pollers,
	// directory watcher) must keep working.
	webRoot, err := validateWebRoot(s.config.WebRoot)
	if err != nil {
		log.Errorf("web server not started: %v", err)
		return
	}

	log.Info("Starting web server...")
	tmpl, err := template.ParseFiles("./static/index.html")
	if err != nil {
		log.Fatal(err)
	}

	// With a non-empty WebRoot the page is served under /<webRoot>/ and
	// the assets must be requested as absolute "/<webRoot>/bundle.js"
	// (issue #90). With an empty WebRoot the references stay
	// document-relative ("./bundle.js") so deployments behind a reverse
	// proxy that strips the path prefix keep working.
	assetBase := "./"
	if webRoot != "" {
		assetBase = webRoot + "/"
	}

	var ibytes bytes.Buffer
	err = tmpl.Execute(&ibytes, &IndexTemplates{assetBase})
	if err != nil {
		log.Fatal(err)
	}
	indexBytes = ibytes.Bytes()

	spa := spaHandler{
		staticPath: "static",
		indexPath:  "index.html",
		webRoot:    webRoot,
	}

	r := mux.NewRouter()
	registerAPIRoutes := func(rt *mux.Router) {
		rt.HandleFunc("/api/transfers", s.TransfersHandler)
		rt.HandleFunc("/api/downloads", s.DownloadsHandler)
		rt.HandleFunc("/api/blackhole", s.BlackholeHandler)
		rt.HandleFunc("/api/config", s.ConfigHandler)
		rt.HandleFunc("/api/testArr", s.TestArrHandler)
	}
	registerAPIRoutes(r)
	// The UI under /<webRoot>/ calls the API relative to the page
	// location, so expose the API under the webRoot as well.
	if webRoot != "" {
		registerAPIRoutes(r.PathPrefix(webRoot).Subrouter())
	}

	r.PathPrefix("/").Handler(spa)

	address := fmt.Sprintf("%s:%s", s.config.BindIP, s.config.BindPort)

	ln, err := net.Listen("tcp", address)
	if err != nil {
		// Do not exit the daemon on a bind failure: Start can run inside
		// a config-update HTTP handler goroutine, where os.Exit would kill
		// the whole process. The web UI stays down, the daemon keeps running.
		log.Errorf("could not listen on %s: %v — web UI unavailable", address, err)
		s.srv = nil
		s.listener = nil
		return
	}
	s.listener = ln

	s.srv = &http.Server{
		Handler: r,
		Addr:    address,
		// Good practice: enforce timeouts for servers you create!
		WriteTimeout: 15 * time.Second,
		ReadTimeout:  15 * time.Second,
	}

	log.Infof("Web server started on %s", address)

	go s.srv.Serve(ln)
}

// Shamelessly stolen from mux examples https://github.com/gorilla/mux#examples
type spaHandler struct {
	staticPath string
	indexPath  string
	webRoot    string
}

func (h spaHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// get the absolute path to prevent directory traversal
	path, err := filepath.Abs(r.URL.Path)
	if err != nil {
		// if we failed to get the absolute path respond with a 400 bad request
		// and stop
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if h.webRoot != "" {
		path = strings.TrimPrefix(path, h.webRoot)
	}
	// prepend the path with the path to the static directory
	path = filepath.Join(h.staticPath, path)

	// check whether a file exists at the given path
	_, err = os.Stat(path)
	if os.IsNotExist(err) || strings.HasSuffix(path, h.staticPath) {
		// file does not exist, serve index.html
		// http.ServeFile(w, r, filepath.Join(h.staticPath, h.indexPath))
		// file does not exist, serve index.html template
		w.Write(indexBytes)
		return
	} else if err != nil {
		// if we got an error (that wasn't that the file doesn't exist) stating the
		// file, return a 500 internal server error and stop
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	r.URL.Path = strings.Replace(path, h.staticPath, "", -1)
	// otherwise, use http.FileServer to serve the static dir
	http.FileServer(http.Dir(h.staticPath)).ServeHTTP(w, r)
}
