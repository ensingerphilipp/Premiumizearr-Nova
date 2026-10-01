package directclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeSABBackend struct {
	added       SABJobView
	removed     string
	deleteFiles bool
	jobs        []SABJobView
	removeErr   error
}

func (f *fakeSABBackend) SABOutputRoot() string   { return "/media/complete" }
func (f *fakeSABBackend) SABCategories() []string { return []string{"tv", "movies"} }

func (f *fakeSABBackend) AddNZB(_ context.Context, data []byte, name, category string) (SABJobView, error) {
	if len(data) == 0 {
		return SABJobView{}, errors.New("empty NZB")
	}
	f.added = SABJobView{ID: "job-1", Name: name, Category: category, State: "queued"}
	return f.added, nil
}
func (f *fakeSABBackend) ListNZB(category string) []SABJobView {
	if category == "" {
		return f.jobs
	}
	out := []SABJobView{}
	for _, j := range f.jobs {
		if j.Category == category {
			out = append(out, j)
		}
	}
	return out
}
func (f *fakeSABBackend) RemoveNZB(id string, deleteFiles bool) error {
	f.removed = id
	f.deleteFiles = deleteFiles
	return f.removeErr
}

func TestSABConfigReportsOutputRootAndCategories(t *testing.T) {
	b := &fakeSABBackend{}
	h := NewSABHandler(b, "key")
	r := httptest.NewRequest(http.MethodGet, "/api?mode=get_config&apikey=key", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	response := w.Body.String()
	for _, expected := range []string{`"complete_dir":"/media/complete"`, `"name":"*"`, `"name":"tv"`, `"name":"movies"`} {
		if !strings.Contains(response, expected) {
			t.Fatalf("config missing %s: %s", expected, response)
		}
	}
	r = httptest.NewRequest(http.MethodGet, "/api?mode=fullstatus&apikey=key", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if !strings.Contains(w.Body.String(), `"complete_dir":"/media/complete"`) {
		t.Fatalf("fullstatus missing output root: %s", w.Body.String())
	}
}

func TestSABHistoryCompletionAndFailure(t *testing.T) {
	b := &fakeSABBackend{jobs: []SABJobView{
		{ID: "done-id", Name: "Movie", Category: "movies", State: "completed", SizeBytes: 2048, OutputPath: "/media/complete/Movie"},
		{ID: "fail-id", Name: "Broken", Category: "tv", State: "failed", Error: "transfer failed"},
	}}
	h := NewSABHandler(b, "key")
	r := httptest.NewRequest(http.MethodGet, "/api?mode=history&category=movies&apikey=key", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	response := w.Body.String()
	for _, expected := range []string{`"nzo_id":"done-id"`, `"status":"Completed"`, `"storage":"/media/complete/Movie"`} {
		if !strings.Contains(response, expected) {
			t.Fatalf("completed history missing %s: %s", expected, response)
		}
	}
	if strings.Contains(response, "fail-id") {
		t.Fatalf("history leaked another category: %s", response)
	}
	r = httptest.NewRequest(http.MethodGet, "/api?mode=history&category=tv&apikey=key", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	response = w.Body.String()
	for _, expected := range []string{`"nzo_id":"fail-id"`, `"status":"Failed"`, `"fail_message":"transfer failed"`} {
		if !strings.Contains(response, expected) {
			t.Fatalf("failed history missing %s: %s", expected, response)
		}
	}
}

func TestSABRemovePreservesOrDeletesFilesAndReturnsErrors(t *testing.T) {
	b := &fakeSABBackend{}
	h := NewSABHandler(b, "key")
	for _, tc := range []struct {
		del  string
		want bool
	}{{"0", false}, {"1", true}} {
		r := httptest.NewRequest(http.MethodGet, "/api?mode=queue&name=delete&value=job-1&apikey=key&del_files="+tc.del, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if b.removed != "job-1" || b.deleteFiles != tc.want || !strings.Contains(w.Body.String(), `"status":true`) {
			t.Fatalf("remove(%s): response=%s backend=%#v", tc.del, w.Body.String(), b)
		}
	}
	b.removeErr = errors.New("unknown job")
	r := httptest.NewRequest(http.MethodGet, "/api?mode=queue&name=delete&value=missing&apikey=key", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if !strings.Contains(w.Body.String(), `"status":false`) || !strings.Contains(w.Body.String(), "unknown job") {
		t.Fatalf("expected SAB error envelope, got %s", w.Body.String())
	}
}

func TestSABAuth(t *testing.T) {
	h := NewSABHandler(&fakeSABBackend{}, "secret")
	r := httptest.NewRequest(http.MethodGet, "/api?mode=version", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
	unset := NewSABHandler(&fakeSABBackend{}, "")
	w = httptest.NewRecorder()
	unset.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api?mode=version", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("empty configured API key should fail closed, got %d", w.Code)
	}
	r = httptest.NewRequest(http.MethodGet, "/api?mode=version&apikey=secret", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"version":"4.5.0"`) {
		t.Fatalf("unexpected response %d %s", w.Code, w.Body.String())
	}
}

func TestSABAddListRemove(t *testing.T) {
	b := &fakeSABBackend{jobs: []SABJobView{{ID: "job-1", Name: "Example", Category: "tv", State: "downloading", Progress: 25, SizeBytes: 100, RemainingBytes: 75}}}
	h := NewSABHandler(b, "key")
	var body strings.Builder
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("name", "Example.nzb")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write([]byte("<nzb/>"))
	_ = mw.Close()
	r := httptest.NewRequest(http.MethodPost, "/api?mode=addfile&apikey=key&cat=tv", strings.NewReader(body.String()))
	r.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if !strings.Contains(w.Body.String(), `"nzo_ids":["job-1"]`) || b.added.Category != "tv" || b.added.Name != "Example.nzb" {
		t.Fatalf("bad add: %s %#v", w.Body.String(), b.added)
	}
	r = httptest.NewRequest(http.MethodGet, "/api?mode=queue&apikey=key&category=tv", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if !strings.Contains(w.Body.String(), `"nzo_id":"job-1"`) || !strings.Contains(w.Body.String(), `"percentage":"25"`) {
		t.Fatalf("bad queue: %s", w.Body.String())
	}
	r = httptest.NewRequest(http.MethodGet, "/api?mode=queue&name=delete&value=job-1&del_files=1&apikey=key", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if b.removed != "job-1" || !b.deleteFiles || !strings.Contains(w.Body.String(), `"status":true`) {
		t.Fatalf("bad remove: %s %#v", w.Body.String(), b)
	}
}

// TestSABRejectsOversizedBodyBeforeSpooling is the regression test for the
// finding that ParseMultipartForm spooled the whole body to temp storage
// before any size check could reject it, so an authenticated client could
// exhaust temp storage with a fat body. The bound now fires up front.
func TestSABRejectsOversizedBodyBeforeSpooling(t *testing.T) {
	b := &fakeSABBackend{}
	h := NewSABHandler(b, "key")
	// A well-formed addfile whose file part drives the whole body
	// past the up-front bound (64 MiB file + 1 MiB envelope). The
	// bound trips mid-spool, so the parse fails and the wrapper's
	// state names the rejection.
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("name", "Fat.nzb")
	if err != nil {
		t.Fatal(err)
	}
	chunk := bytes.Repeat([]byte("0123456789abcdef"), (1<<20)/16)
	var content int64
	for content < sabBodyLimit+(1<<20) {
		n := int64(len(chunk))
		if n > sabBodyLimit+(1<<20)-content {
			n = sabBodyLimit + (1 << 20) - content
		}
		_, _ = part.Write(chunk[:n])
		content += n
	}
	_ = mw.Close()
	r := httptest.NewRequest(http.MethodPost, "/api?mode=addfile&apikey=key", bytes.NewReader(body.Bytes()))
	r.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	// A real server writes 413 when the bound fires; a recorder has no
	// such path and gets the SAB error envelope instead. Both are
	// rejections: the client is told about the size and nothing reaches
	// the backend.
	rejected := w.Code == http.StatusRequestEntityTooLarge
	if !rejected {
		var envelope struct {
			Status bool   `json:"status"`
			Error  string `json:"error"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil || envelope.Status || !strings.Contains(envelope.Error, "too large") {
			t.Fatalf("oversized body: status = %d, body = %s", w.Code, w.Body.String())
		}
	}
	if b.added.ID != "" {
		t.Fatalf("oversized body reached the backend: %#v", b.added)
	}
	// The handler must still work after the rejection.
	var small strings.Builder
	smallW := multipart.NewWriter(&small)
	smallPart, err := smallW.CreateFormFile("name", "Small.nzb")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = smallPart.Write([]byte("<nzb/>"))
	_ = smallW.Close()
	r = httptest.NewRequest(http.MethodPost, "/api?mode=addfile&apikey=key", strings.NewReader(small.String()))
	r.Header.Set("Content-Type", smallW.FormDataContentType())
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if !strings.Contains(w.Body.String(), `"nzo_ids":["job-1"]`) {
		t.Fatalf("valid small addfile after rejection failed: %s", w.Body.String())
	}
}
