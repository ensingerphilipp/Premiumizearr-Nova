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
	RootPath string
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
// trimmed, without a trailing slash, and with a leading slash when
// non-empty, so assets are referenced as absolute URLs like "/nova/bundle.js".
func normalizeWebRoot(webRoot string) string {
	webRoot = strings.TrimSpace(webRoot)
	webRoot = strings.TrimSuffix(webRoot, "/")
	if webRoot != "" && !strings.HasPrefix(webRoot, "/") {
		webRoot = "/" + webRoot
	}
	return webRoot
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
		log.Tracef("Config updated, restarting web server...")
		s.srv.Close()
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
	log.Info("Starting web server...")
	tmpl, err := template.ParseFiles("./static/index.html")
	if err != nil {
		log.Fatal(err)
	}

	webRoot := normalizeWebRoot(s.config.WebRoot)

	var ibytes bytes.Buffer
	err = tmpl.Execute(&ibytes, &IndexTemplates{webRoot})
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

	r.HandleFunc("/api/transfers", s.TransfersHandler)
	r.HandleFunc("/api/downloads", s.DownloadsHandler)
	r.HandleFunc("/api/blackhole", s.BlackholeHandler)
	r.HandleFunc("/api/config", s.ConfigHandler)
	r.HandleFunc("/api/testArr", s.TestArrHandler)

	r.PathPrefix("/").Handler(spa)

	address := fmt.Sprintf("%s:%s", s.config.BindIP, s.config.BindPort)

	ln, err := net.Listen("tcp", address)
	if err != nil {
		log.Fatal(err)
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
