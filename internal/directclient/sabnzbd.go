package directclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
)

const (
	// nzbFileLimit bounds a single uploaded NZB file.
	nzbFileLimit = 64 << 20
	// sabBodyLimit bounds the whole POST body: the NZB file plus the
	// multipart envelope around it. It is applied up front so an
	// oversized body is rejected BEFORE ParseMultipartForm can spool
	// it to temp storage.
	sabBodyLimit = nzbFileLimit + (1 << 20)
)

// errBodyLimitExceeded is returned once a request body exceeds
// sabBodyLimit.
var errBodyLimitExceeded = errors.New("request body too large")

// limitedBody counts every byte handed to the wrapped request body; once
// the total exceeds the limit, subsequent reads fail with
// errBodyLimitExceeded. The handler checks tripped() after a parse
// attempt: Go's multipart parser obscures the underlying limit error
// behind its own buffering errors, so the trip must be detected from the
// reader itself, not from the parse error.
type limitedBody struct {
	inner     io.Reader
	remain    int64
	overLimit bool
}

func newLimitedBody(r io.Reader, limit int64) *limitedBody {
	return &limitedBody{inner: r, remain: limit}
}

func (b *limitedBody) Read(p []byte) (int, error) {
	if b.overLimit {
		return 0, errBodyLimitExceeded
	}
	if b.remain <= 0 {
		// The limit is consumed. Probe for more data: a body of
		// exactly the limit size ends in a clean EOF, while a longer
		// body trips here.
		var probe [1]byte
		n, err := b.inner.Read(probe[:])
		if err == io.EOF && n == 0 {
			return 0, io.EOF
		}
		b.overLimit = true
		return 0, errBodyLimitExceeded
	}
	if int64(len(p)) > b.remain {
		// Serve exactly what remains so the parser sees a clean end
		// of data; the trip is reported on the next read.
		p = p[:b.remain]
	}
	n, err := b.inner.Read(p)
	b.remain -= int64(n)
	return n, err
}

func (b *limitedBody) tripped() bool { return b.overLimit }

// Close closes the wrapped body if it can be closed; request bodies
// are ReadClosers.
func (b *limitedBody) Close() error {
	if c, ok := b.inner.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

// SABJobView is the subset of a download job needed to present it through the
// SABnzbd API expected by Sonarr, Radarr and Lidarr.
type SABJobView struct {
	ID             string
	Name           string
	Category       string
	State          string // queued, downloading, completed, or failed
	Progress       float64
	SizeBytes      int64
	RemainingBytes int64
	OutputPath     string
	Error          string
}

// SABBackend owns NZB ingestion and the durable job lifecycle.
type SABBackend interface {
	AddNZB(ctx context.Context, data []byte, filename, category string) (SABJobView, error)
	ListNZB(category string) []SABJobView
	RemoveNZB(id string, deleteFiles bool) error
}

// SABOutputRoot may be implemented by a backend to report the directory *arr
// should scan for completed jobs. It is optional to keep SABBackend minimal.
type SABOutputRoot interface{ SABOutputRoot() string }
type sabCategories interface{ SABCategories() []string }

// NewSABHandler returns the bounded SABnzbd-compatible API used by *arr
// clients. Unsupported modes return a SAB-shaped error response.
func NewSABHandler(backend SABBackend, apiKey string) http.Handler {
	return &sabHandler{backend: backend, apiKey: apiKey}
}

type sabHandler struct {
	backend SABBackend
	apiKey  string
}

func (h *sabHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.apiKey == "" || r.URL.Query().Get("apikey") != h.apiKey {
		w.WriteHeader(http.StatusUnauthorized)
		h.write(w, map[string]any{"status": false, "error": "API Key Incorrect"})
		return
	}
	// Bound the whole POST body before any branch parses it: the mode
	// detection below and addfile both call ParseMultipartForm, which
	// spools the body to temp storage before the per-file limit can
	// reject it. limitedBody reports its own trip, because the
	// multipart parser obscures any limit error it encounters.
	var bodyLim *limitedBody
	if r.Method == http.MethodPost {
		bodyLim = newLimitedBody(r.Body, sabBodyLimit)
		r.Body = bodyLim
	}
	mode := r.URL.Query().Get("mode")
	if mode == "" && r.Method == http.MethodPost {
		_ = r.ParseMultipartForm(64 << 20)
		if r.MultipartForm == nil {
			_ = r.ParseForm()
		}
		if bodyLim.tripped() {
			h.bad(w, errBodyLimitExceeded)
			return
		}
		mode = r.Form.Get("mode")
	}
	var data []byte
	var filename string
	if mode == "addfile" {
		if err := r.ParseMultipartForm(nzbFileLimit); err != nil {
			// The parser surfaces the wrapper's trip as the parse
			// error, so name it as the size rejection it is.
			if bodyLim != nil && bodyLim.tripped() {
				h.bad(w, errBodyLimitExceeded)
				return
			}
			h.bad(w, err)
			return
		}
		f, hdr, err := r.FormFile("name")
		if err != nil {
			f, hdr, err = r.FormFile("nzbfile")
		}
		if err != nil {
			h.bad(w, fmt.Errorf("missing NZB file: %w", err))
			return
		}
		defer f.Close()
		data, err = io.ReadAll(io.LimitReader(f, nzbFileLimit+1))
		if err != nil {
			h.bad(w, err)
			return
		}
		if len(data) > nzbFileLimit {
			h.bad(w, fmt.Errorf("NZB exceeds 64 MiB limit"))
			return
		}
		filename = hdr.Filename
	}
	if r.Method == http.MethodPost && mode != "addfile" { // SAB commonly submits mode in POST form fields.
		_ = r.ParseForm()
		if mode == "" {
			mode = r.Form.Get("mode")
		}
	}
	switch mode {
	case "version":
		h.write(w, map[string]any{"version": "4.5.0"})
	case "get_config":
		categories := []map[string]any{{"name": "*", "dir": ""}}
		if b, ok := h.backend.(sabCategories); ok {
			for _, name := range b.SABCategories() {
				categories = append(categories, map[string]any{"name": name, "dir": ""})
			}
		}
		h.write(w, map[string]any{"config": map[string]any{"misc": map[string]any{"complete_dir": h.outputRoot(), "enable_tv_sorting": false, "enable_movie_sorting": false, "enable_date_sorting": false, "pre_check": false}, "categories": categories}})
	case "addfile":
		job, err := h.backend.AddNZB(r.Context(), data, filename, r.URL.Query().Get("cat"))
		if err != nil {
			h.bad(w, err)
			return
		}
		h.write(w, map[string]any{"status": true, "nzo_ids": []string{job.ID}})
	case "queue":
		if r.URL.Query().Get("name") == "delete" {
			id := r.URL.Query().Get("value")
			if id == "" {
				id = r.URL.Query().Get("id")
			}
			if err := h.backend.RemoveNZB(id, r.URL.Query().Get("del_files") == "1"); err != nil {
				h.bad(w, err)
				return
			}
			h.write(w, map[string]any{"status": true})
			return
		}
		jobs := h.backend.ListNZB(r.URL.Query().Get("category"))
		h.write(w, map[string]any{"queue": map[string]any{"version": "4.5.0", "paused": false, "pause_int": "0", "paused_all": false, "speed": "0", "mbleft": "0", "mb": "0", "sizeleft": "0 B", "size": "0 B", "noofslots_total": len(jobs), "noofslots": len(jobs), "status": queueStatus(jobs), "timeleft": "0:00:00", "start": 0, "limit": 0, "slots": queueSlots(jobs)}})
	case "history":
		if r.URL.Query().Get("name") == "delete" {
			if err := h.backend.RemoveNZB(r.URL.Query().Get("value"), r.URL.Query().Get("del_files") == "1"); err != nil {
				h.bad(w, err)
				return
			}
			h.write(w, map[string]any{"status": true})
			return
		}
		jobs := h.backend.ListNZB(r.URL.Query().Get("category"))
		h.write(w, map[string]any{"history": map[string]any{"slots": historySlots(jobs), "status": true}})
	case "fullstatus":
		h.write(w, map[string]any{"status": map[string]any{"complete_dir": h.outputRoot(), "version": "4.5.0"}})
	default:
		h.write(w, map[string]any{"status": false, "error": "Unknown mode"})
	}
}

func (h *sabHandler) outputRoot() string {
	if provider, ok := h.backend.(SABOutputRoot); ok {
		return provider.SABOutputRoot()
	}
	return ""
}

func queueStatus(jobs []SABJobView) string {
	for _, j := range jobs {
		if j.State == "downloading" {
			return "Downloading"
		}
		if j.State == "queued" {
			return "Queued"
		}
	}
	return "Idle"
}
func queueSlots(jobs []SABJobView) []map[string]any {
	out := []map[string]any{}
	for _, j := range jobs {
		if j.State == "completed" || j.State == "failed" {
			continue
		}
		size, left := mb(j.SizeBytes), mb(j.RemainingBytes)
		status := "Downloading"
		if j.State == "queued" {
			status = "Queued"
		}
		out = append(out, map[string]any{"nzo_id": j.ID, "filename": j.Name, "cat": j.Category, "status": status, "percentage": strconv.Itoa(int(j.Progress)), "mbleft": strconv.FormatFloat(left, 'f', 2, 64), "mb": strconv.FormatFloat(size, 'f', 2, 64), "size": fmt.Sprintf("%.2f MB", size), "sizeleft": fmt.Sprintf("%.2f MB", left), "priority": "Normal", "timeleft": "0:00:00"})
	}
	return out
}
func historySlots(jobs []SABJobView) []map[string]any {
	out := []map[string]any{}
	for _, j := range jobs {
		if j.State != "completed" && j.State != "failed" {
			continue
		}
		status := "Completed"
		if j.State == "failed" {
			status = "Failed"
		}
		out = append(out, map[string]any{"nzo_id": j.ID, "name": j.Name, "category": j.Category, "status": status, "bytes": j.SizeBytes, "storage": j.OutputPath, "fail_message": j.Error, "pp": map[string]any{"stage_log": []any{}}})
	}
	return out
}
func mb(n int64) float64 { return float64(n) / (1024 * 1024) }

func (h *sabHandler) bad(w http.ResponseWriter, err error) {
	h.write(w, map[string]any{"status": false, "error": err.Error()})
}
func (h *sabHandler) write(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
