// Package directclient contains compatibility HTTP APIs used by *arr.
package directclient

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// TorrentView is the qBittorrent-shaped view of a Premiumizearr download.
type TorrentView struct {
	Hash, Name, Category, State  string
	Progress                     float64
	Size, AmountLeft             int64
	ContentPath, SavePath, Error string
}

// QBitBackend is the narrow set of operations needed by *arr qBittorrent clients.
type QBitBackend interface {
	AddTorrent(ctx context.Context, data []byte, filename, category string) error
	AddMagnet(ctx context.Context, magnet, category string) error
	ListTorrents(category string) []TorrentView
	RemoveTorrent(hash string, deleteFiles bool) error
	SetCategory(hash, category string) error
}

type qbitCategories interface {
	ListCategories() []string
	CreateCategory(string) error
}

// QBitOutputRoot can be implemented by the backend to expose the actual
// *arr-visible download directory in preferences. It is optional because
// some backends derive that path per job.
type QBitOutputRoot interface {
	QBitOutputRoot() string
}

type qbitHandler struct {
	backend            QBitBackend
	username, password string
	sidMu              sync.Mutex // guards sid: rotated on logout
	sid                string
}

// NewQBitHandler serves the qBittorrent Web API v2 subset used by Sonarr, Radarr and Lidarr.
func NewQBitHandler(backend QBitBackend, username, password string) http.Handler {
	var token [24]byte
	if _, err := rand.Read(token[:]); err != nil {
		panic("unable to create qBittorrent session token")
	}
	return &qbitHandler{backend: backend, username: username, password: password, sid: hex.EncodeToString(token[:])}
}

func newSessionToken() string {
	var token [24]byte
	if _, err := rand.Read(token[:]); err != nil {
		panic("unable to create qBittorrent session token")
	}
	return hex.EncodeToString(token[:])
}

// outputRoot resolves the *arr-visible download root advertised by the
// backend's preferences endpoints.
func (h *qbitHandler) outputRoot() string {
	if b, ok := h.backend.(QBitOutputRoot); ok {
		return b.QBitOutputRoot()
	}
	return ""
}

func (h *qbitHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimSuffix(r.URL.Path, "/")
	if path == "/api/v2/auth/login" {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, "Invalid form", 400)
			return
		}
		if h.password == "" || !equalSecret(r.Form.Get("username"), h.username) || !equalSecret(r.Form.Get("password"), h.password) {
			http.Error(w, "Fails.", http.StatusForbidden)
			return
		}
		h.sidMu.Lock()
		sid := h.sid
		h.sidMu.Unlock()
		// qBittorrent marks its session cookie Secure only over SSL. A
		// forced Secure attribute would not be returned by *arr clients
		// (Sonarr/Radarr/Lidarr) that authenticate over plain HTTP, so the
		// attribute follows the transport instead.
		http.SetCookie(w, &http.Cookie{Name: "SID", Value: sid, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: r.TLS != nil})
		writeQBitText(w, "Ok.")
		return
	}
	if path == "/" {
		writeQBitText(w, "qBittorrent Web UI")
		return
	}
	if !h.authenticated(r) {
		// *arr retries login on qBittorrent's 403 response for an expired SID.
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	if path == "/api/v2/auth/logout" {
		// Rotate the session so a cookie presented after logout (stolen or
		// replayed) no longer authenticates; the client must log in again.
		h.sidMu.Lock()
		h.sid = newSessionToken()
		h.sidMu.Unlock()
		// Secure follows the transport, as on login: see the login comment.
		http.SetCookie(w, &http.Cookie{Name: "SID", Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: r.TLS != nil})
		writeQBitText(w, "Ok.")
		return
	}
	switch path {
	case "/api/v2/app/webapiVersion":
		writeQBitText(w, "2.7.0")
	case "/api/v2/app/version":
		writeQBitText(w, "v4.3.3")
	case "/api/v2/app/preferences":
		root := h.outputRoot()
		writeQBitJSON(w, map[string]any{"save_path": root, "temp_path": root, "temp_path_enabled": false, "auto_tmm_enabled": false, "create_subfolder_enabled": false, "start_paused_enabled": false, "web_ui_username": h.username, "dht": true, "queueing_enabled": true})
	case "/api/v2/app/defaultSavePath":
		writeQBitText(w, h.outputRoot())
	case "/api/v2/torrents/categories":
		cats := map[string]any{}
		if b, ok := h.backend.(qbitCategories); ok {
			for _, name := range b.ListCategories() {
				cats[name] = map[string]string{"name": name, "savePath": ""}
			}
		}
		for _, t := range h.backend.ListTorrents("") {
			if t.Category != "" {
				cats[t.Category] = map[string]string{"name": t.Category, "savePath": t.SavePath}
			}
		}
		writeQBitJSON(w, cats)
	case "/api/v2/torrents/add":
		h.add(w, r)
	case "/api/v2/torrents/info":
		h.info(w, r)
	case "/api/v2/torrents/delete":
		h.remove(w, r)
	case "/api/v2/torrents/setCategory":
		h.setCategory(w, r)
	case "/api/v2/torrents/createCategory":
		if b, ok := h.backend.(qbitCategories); ok {
			if err := r.ParseForm(); err != nil {
				http.Error(w, "Invalid category request", 400)
				return
			}
			if err := b.CreateCategory(r.Form.Get("category")); err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
		}
		writeQBitText(w, "Ok.")
	default:
		http.NotFound(w, r)
	}
}
func (h *qbitHandler) authenticated(r *http.Request) bool {
	c, err := r.Cookie("SID")
	if err != nil {
		return false
	}
	h.sidMu.Lock()
	sid := h.sid
	h.sidMu.Unlock()
	return equalSecret(c.Value, sid)
}
func equalSecret(a, b string) bool {
	return len(a) == len(b) && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
func writeQBitText(w http.ResponseWriter, s string) {
	w.Header().Set("Content-Type", "text/plain; charset=UTF-8")
	_, _ = io.WriteString(w, s)
}
func writeQBitJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (h *qbitHandler) add(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", 405)
		return
	}
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		// Bound the whole body before the parse: 64 MiB is a spool
		// THRESHOLD, not a cap, so an oversized file part would be spooled
		// to a temp file the parser never removes. The wrapper reports its
		// own trip, because the multipart parser obscures any limit error
		// it encounters (same guard as the SAB handler).
		bodyLim := newLimitedBody(r.Body, (64<<20)+(1<<20))
		r.Body = bodyLim
		if err := r.ParseMultipartForm(64 << 20); err != nil {
			if bodyLim.tripped() {
				http.Error(w, "torrent file exceeds 64 MiB limit", http.StatusInternalServerError)
				return
			}
			http.Error(w, "Invalid add request", 400)
			return
		}
		if r.MultipartForm != nil {
			defer r.MultipartForm.RemoveAll()
		}
	} else if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid add request", 400)
		return
	}
	category := r.FormValue("category")
	// qBittorrent accepts several URLs separated by newlines in "urls".
	// Split on newlines only: a magnet is a single opaque URL and
	// splitting on arbitrary whitespace would truncate magnets that carry
	// spaces in their parameters.
	for _, magnet := range strings.Split(r.FormValue("urls"), "\n") {
		magnet = strings.TrimSpace(magnet)
		if magnet == "" {
			continue
		}
		if !strings.HasPrefix(strings.ToLower(magnet), "magnet:") {
			http.Error(w, "Invalid torrent URL", 400)
			return
		}
		if err := h.backend.AddMagnet(r.Context(), magnet, category); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
	}
	if r.MultipartForm != nil {
		for _, files := range r.MultipartForm.File {
			for _, fh := range files {
				if err := h.addFile(r, fh, category); err != nil {
					http.Error(w, err.Error(), 500)
					return
				}
			}
		}
	}
	writeQBitText(w, "Ok.")
}
func (h *qbitHandler) addFile(r *http.Request, fh *multipart.FileHeader, category string) error {
	f, err := fh.Open()
	if err != nil {
		return err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, (64<<20)+1))
	if err != nil {
		return err
	}
	if len(data) > 64<<20 {
		return fmt.Errorf("torrent file exceeds 64 MiB limit")
	}
	return h.backend.AddTorrent(r.Context(), data, fh.Filename, category)
}
func (h *qbitHandler) info(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	ts := h.backend.ListTorrents(q.Get("category"))
	out := make([]map[string]any, 0, len(ts))
	for _, t := range ts {
		if hashes := q.Get("hashes"); hashes != "" && !containsPipeValue(hashes, t.Hash) {
			continue
		}
		if n := q.Get("name"); n != "" && t.Name != n {
			continue
		}
		out = append(out, map[string]any{"hash": t.Hash, "name": t.Name, "category": t.Category, "state": qbitState(t.State), "progress": t.Progress, "size": t.Size, "amount_left": t.AmountLeft, "content_path": t.ContentPath, "save_path": t.SavePath, "error": t.Error, "dlspeed": int64(0), "upspeed": int64(0), "eta": int64(8640000), "num_seeds": 0, "num_leechs": 0, "ratio": 0, "ratio_limit": 0, "priority": 0, "seq_dl": false, "force_start": false, "completed": t.Size - t.AmountLeft, "completion_on": int64(0), "added_on": int64(0), "last_activity": int64(0), "amount_uploaded": int64(0), "uploaded": int64(0), "downloaded": t.Size - t.AmountLeft, "downloaded_session": int64(0), "uploaded_session": int64(0), "max_ratio": -1, "max_seeding_time": -1, "auto_tmm": false, "magnet_uri": "", "tags": "", "tracker": "", "comment": "", "piece_size": 0, "num_complete": 0, "num_incomplete": 0, "num_downloaded": 0, "total_size": t.Size})
	}
	if off, _ := strconv.Atoi(q.Get("offset")); off > 0 {
		if off >= len(out) {
			out = []map[string]any{}
		} else {
			out = out[off:]
		}
	}
	if lim, _ := strconv.Atoi(q.Get("limit")); lim > 0 && lim < len(out) {
		out = out[:lim]
	}
	writeQBitJSON(w, out)
}
func qbitState(s string) string {
	switch strings.ToLower(s) {
	case "downloading", "downloading_metadata", "metadl":
		return "downloading"
	case "completed", "uploading", "seeding":
		return "stoppedUP"
	case "paused", "pauseddl", "pausedup":
		return "pausedDL"
	case "queued", "queued_to_check", "queued_dl":
		return "queuedDL"
	case "failed", "error", "missingfiles":
		return "error"
	case "checking", "checkingdl":
		return "checkingDL"
	default:
		if s == "" {
			return "stalledDL"
		}
		return s
	}
}
func containsPipeValue(s, v string) bool {
	for _, x := range strings.Split(s, "|") {
		if strings.EqualFold(x, v) {
			return true
		}
	}
	return false
}

// eachHash parses the pipe-delimited "hashes" form field shared by the
// torrent mutation endpoints and applies op to every hash. A POST that
// fails for one hash aborts with 500, matching qBittorrent's all-or-nothing
// response for these calls.
func (h *qbitHandler) eachHash(w http.ResponseWriter, r *http.Request, op func(hash string) error) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", 405)
		return
	}
	_ = r.ParseForm()
	for _, hash := range strings.Split(r.Form.Get("hashes"), "|") {
		if hash != "" {
			if err := op(hash); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
		}
	}
	writeQBitText(w, "Ok.")
}
func (h *qbitHandler) remove(w http.ResponseWriter, r *http.Request) {
	h.eachHash(w, r, func(hash string) error {
		return h.backend.RemoveTorrent(hash, r.Form.Get("deleteFiles") == "true")
	})
}
func (h *qbitHandler) setCategory(w http.ResponseWriter, r *http.Request) {
	h.eachHash(w, r, func(hash string) error {
		return h.backend.SetCategory(hash, r.Form.Get("category"))
	})
}
