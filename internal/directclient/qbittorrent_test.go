package directclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

type qbitFake struct {
	torrents               []TorrentView
	addedFile, addedMagnet string
	category               string
	removed                string
	deleted                bool
	// err, when set, is returned by every mutating backend method so
	// tests can drive the non-200 API branches.
	err error
}

func (f *qbitFake) AddTorrent(_ context.Context, b []byte, n, c string) error {
	if f.err != nil {
		return f.err
	}
	f.addedFile = n + ":" + string(b) + ":" + c
	return nil
}
func (f *qbitFake) AddMagnet(_ context.Context, m, c string) error {
	if f.err != nil {
		return f.err
	}
	f.addedMagnet = m + ":" + c
	return nil
}
func (f *qbitFake) ListTorrents(c string) []TorrentView {
	if c == "" {
		return f.torrents
	}
	var out []TorrentView
	for _, t := range f.torrents {
		if t.Category == c {
			out = append(out, t)
		}
	}
	return out
}
func (f *qbitFake) RemoveTorrent(h string, d bool) error {
	if f.err != nil {
		return f.err
	}
	f.removed = h
	f.deleted = d
	return nil
}
func (f *qbitFake) SetCategory(h, c string) error {
	if f.err != nil {
		return f.err
	}
	f.category = h + ":" + c
	return nil
}
func login(t *testing.T, h http.Handler) string {
	t.Helper()
	r := httptest.NewRequest("POST", "/api/v2/auth/login", strings.NewReader("username=arr&password=secret"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("login status %d", w.Code)
	}
	return w.Result().Cookies()[0].String()
}
func request(h http.Handler, method, path, body, contentType, cookie string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	if cookie != "" {
		r.Header.Set("Cookie", cookie)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func TestQBitAuthAndInfo(t *testing.T) {
	if w := request(NewQBitHandler(&qbitFake{}, "arr", ""), "POST", "/api/v2/auth/login", "username=arr&password=", "application/x-www-form-urlencoded", ""); w.Code != http.StatusForbidden {
		t.Fatalf("empty configured password should fail closed, got %d", w.Code)
	}
	f := &qbitFake{torrents: []TorrentView{{Hash: "abc", Name: "Film", Category: "movies", State: "downloading", Progress: .5, Size: 100, AmountLeft: 50}}}
	h := NewQBitHandler(f, "arr", "secret")
	if w := request(h, "GET", "/api/v2/torrents/info", "", "", ""); w.Code != 401 {
		t.Fatalf("unauth status=%d", w.Code)
	}
	bad := request(h, "POST", "/api/v2/auth/login", "username=arr&password=bad", "application/x-www-form-urlencoded", "")
	if bad.Code != 403 {
		t.Fatalf("bad login status=%d", bad.Code)
	}
	c := login(t, h)
	w := request(h, "GET", "/api/v2/torrents/info?category=movies", "", "", c)
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	var got []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0]["hash"] != "abc" || got[0]["state"] != "downloading" {
		t.Fatalf("bad info: %s", w.Body.String())
	}
}
func TestQBitAddMagnetAndTorrent(t *testing.T) {
	f := &qbitFake{}
	h := NewQBitHandler(f, "arr", "secret")
	c := login(t, h)
	form := url.Values{"urls": {"magnet:?xt=urn:btih:abc"}, "category": {"tv"}}
	w := request(h, "POST", "/api/v2/torrents/add", form.Encode(), "application/x-www-form-urlencoded", c)
	if w.Code != 200 || f.addedMagnet != "magnet:?xt=urn:btih:abc:tv" {
		t.Fatalf("magnet: code %d; %q", w.Code, f.addedMagnet)
	}
	var body strings.Builder
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("category", "movies")
	part, err := mw.CreateFormFile("torrents", "movie.torrent")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write([]byte("payload"))
	_ = mw.Close()
	w = request(h, "POST", "/api/v2/torrents/add", body.String(), mw.FormDataContentType(), c)
	if w.Code != 200 || f.addedFile != "movie.torrent:payload:movies" {
		t.Fatalf("file: code %d; %q body=%s", w.Code, f.addedFile, w.Body.String())
	}
}
func TestQBitRemovalAndCategory(t *testing.T) {
	f := &qbitFake{}
	h := NewQBitHandler(f, "arr", "secret")
	c := login(t, h)
	w := request(h, "POST", "/api/v2/torrents/delete", "hashes=abc&deleteFiles=true", "application/x-www-form-urlencoded", c)
	if w.Code != 200 || f.removed != "abc" || !f.deleted {
		t.Fatalf("remove: %d %+v", w.Code, f)
	}
	w = request(h, "POST", "/api/v2/torrents/setCategory", "hashes=abc&category=tv", "application/x-www-form-urlencoded", c)
	if w.Code != 200 || f.category != "abc:tv" {
		t.Fatalf("category: %d %q", w.Code, f.category)
	}
}

// TestQBitInfoStateTransformsAndFields asserts the qbitState mappings that
// drive the *arr import contract (not just the identity mapping) and the
// JSON fields *arr reads: progress, completed, amount_left, content_path.
func TestQBitInfoStateTransformsAndFields(t *testing.T) {
	f := &qbitFake{torrents: []TorrentView{
		{Hash: "done", Name: "Done", Category: "tv", State: "completed", Progress: 1, Size: 200, AmountLeft: 0, ContentPath: "/downloads/direct/done", SavePath: "/downloads/direct"},
		{Hash: "up", Name: "Up", Category: "tv", State: "uploading", Progress: 1, Size: 100, AmountLeft: 0},
		{Hash: "seed", Name: "Seed", Category: "tv", State: "seeding", Progress: 1, Size: 100, AmountLeft: 0},
		{Hash: "bad", Name: "Bad", Category: "tv", State: "failed", Progress: 0.4, Size: 100, AmountLeft: 60, Error: "boom"},
		{Hash: "err", Name: "Err", Category: "tv", State: "error"},
		{Hash: "miss", Name: "Miss", Category: "tv", State: "missingfiles"},
		{Hash: "fresh", Name: "Fresh", Category: "tv"},
	}}
	h := NewQBitHandler(f, "arr", "secret")
	c := login(t, h)
	w := request(h, "GET", "/api/v2/torrents/info?category=tv", "", "", c)
	if w.Code != 200 {
		t.Fatalf("info status = %d", w.Code)
	}
	var got []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	byHash := map[string]map[string]any{}
	for _, m := range got {
		byHash[m["hash"].(string)] = m
	}
	wantStates := map[string]string{
		"done": "stoppedUP", "up": "stoppedUP", "seed": "stoppedUP",
		"bad": "error", "err": "error", "miss": "error",
		"fresh": "stalledDL",
	}
	for hash, want := range wantStates {
		m, ok := byHash[hash]
		if !ok {
			t.Fatalf("info missing torrent %s: %s", hash, w.Body.String())
		}
		if m["state"] != want {
			t.Errorf("state[%s] = %v, want %s", hash, m["state"], want)
		}
	}
	done := byHash["done"]
	if done["progress"] != float64(1) {
		t.Errorf("completed torrent progress = %v, want 1", done["progress"])
	}
	// JSON numbers decode into float64.
	if done["completed"] != float64(200) || done["size"] != float64(200) {
		t.Errorf("completed torrent completed/size = %v/%v, want 200/200", done["completed"], done["size"])
	}
	if done["amount_left"] != float64(0) {
		t.Errorf("completed torrent amount_left = %v, want 0", done["amount_left"])
	}
	if done["content_path"] != "/downloads/direct/done" {
		t.Errorf("completed torrent content_path = %v, want /downloads/direct/done", done["content_path"])
	}
	if byHash["bad"]["amount_left"] != float64(60) || byHash["bad"]["error"] != "boom" {
		t.Errorf("failed torrent fields = %v/%v, want 60/boom", byHash["bad"]["amount_left"], byHash["bad"]["error"])
	}
}

// TestQBitLogoutRevokesSession verifies that logging out invalidates the
// SID: a cookie presented after logout must no longer authenticate, while a
// fresh login still succeeds.
func TestQBitLogoutRevokesSession(t *testing.T) {
	h := NewQBitHandler(&qbitFake{}, "arr", "secret")
	c := login(t, h)
	if w := request(h, "GET", "/api/v2/torrents/info", "", "", c); w.Code != 200 {
		t.Fatalf("authenticated before logout = %d, want 200", w.Code)
	}
	if w := request(h, "POST", "/api/v2/auth/logout", "", "", c); w.Code != 200 {
		t.Fatalf("logout status = %d, want 200", w.Code)
	}
	if w := request(h, "GET", "/api/v2/torrents/info", "", "", c); w.Code != http.StatusUnauthorized {
		t.Fatalf("replayed cookie after logout = %d, want 401", w.Code)
	}
	c2 := login(t, h)
	if w := request(h, "GET", "/api/v2/torrents/info", "", "", c2); w.Code != 200 {
		t.Fatalf("re-login after logout = %d, want 200", w.Code)
	}
}

// TestQBitErrorBranches drives every non-200 branch of the qBittorrent API:
// invalid magnet URL (400), oversized torrent file and backend failures
// (500), and non-POST methods (405).
func TestQBitErrorBranches(t *testing.T) {
	const bigFile = 64<<20 + 1
	h := NewQBitHandler(&qbitFake{}, "arr", "secret")
	c := login(t, h)

	if w := request(h, "POST", "/api/v2/torrents/add", "urls=http://example.com/x.torrent", "application/x-www-form-urlencoded", c); w.Code != http.StatusBadRequest {
		t.Fatalf("non-magnet URL: %d, want 400", w.Code)
	}

	for _, tc := range []struct {
		name   string
		mutate func(f *qbitFake)
		path   string
		body   string
	}{
		{name: "AddMagnet fails", mutate: func(f *qbitFake) { f.err = errors.New("premiumize down") },
			path: "/api/v2/torrents/add", body: "urls=magnet:?xt=urn:btih:abc&category=tv"},
		{name: "RemoveTorrent fails", mutate: func(f *qbitFake) { f.err = errors.New("premiumize down") },
			path: "/api/v2/torrents/delete", body: "hashes=abc&deleteFiles=true"},
		{name: "SetCategory fails", mutate: func(f *qbitFake) { f.err = errors.New("premiumize down") },
			path: "/api/v2/torrents/setCategory", body: "hashes=abc&category=tv"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &qbitFake{}
			tc.mutate(f)
			h := NewQBitHandler(f, "arr", "secret")
			cc := login(t, h)
			w := request(h, "POST", tc.path, tc.body, "application/x-www-form-urlencoded", cc)
			if w.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500: %s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), "premiumize down") {
				t.Fatalf("error body = %q, want backend error", w.Body.String())
			}
		})
	}

	t.Run("AddTorrent fails", func(t *testing.T) {
		f := &qbitFake{err: errors.New("premiumize down")}
		h := NewQBitHandler(f, "arr", "secret")
		cc := login(t, h)
		var body strings.Builder
		mw := multipart.NewWriter(&body)
		_ = mw.WriteField("category", "tv")
		part, _ := mw.CreateFormFile("torrents", "movie.torrent")
		_, _ = part.Write([]byte("payload"))
		_ = mw.Close()
		w := request(h, "POST", "/api/v2/torrents/add", body.String(), mw.FormDataContentType(), cc)
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500: %s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "premiumize down") {
			t.Fatalf("error body = %q, want backend error", w.Body.String())
		}
	})

	t.Run("oversized torrent file", func(t *testing.T) {
		h := NewQBitHandler(&qbitFake{}, "arr", "secret")
		cc := login(t, h)
		var body strings.Builder
		mw := multipart.NewWriter(&body)
		_ = mw.WriteField("category", "tv")
		part, _ := mw.CreateFormFile("torrents", "big.torrent")
		if _, err := part.Write(bytes.Repeat([]byte("x"), bigFile)); err != nil {
			t.Fatal(err)
		}
		_ = mw.Close()
		w := request(h, "POST", "/api/v2/torrents/add", body.String(), mw.FormDataContentType(), cc)
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500: %s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "exceeds 64 MiB limit") {
			t.Fatalf("error body = %q, want size limit error", w.Body.String())
		}
	})

	for _, path := range []string{"/api/v2/torrents/add", "/api/v2/torrents/delete", "/api/v2/torrents/setCategory"} {
		t.Run("non-POST "+path, func(t *testing.T) {
			h := NewQBitHandler(&qbitFake{}, "arr", "secret")
			cc := login(t, h)
			w := request(h, "GET", path, "", "", cc)
			if w.Code != http.StatusMethodNotAllowed {
				t.Fatalf("GET %s status = %d, want 405", path, w.Code)
			}
		})
	}
}

// TestQBitAddMagnetWithSpacesSurvivesURLSplit is the regression test for
// the finding that "urls" was split on arbitrary whitespace (strings.Fields),
// shredding a magnet that carries a space into a non-magnet token (400)
// while persisting the truncated first token as a job. qBittorrent splits
// the field on newlines only.
func TestQBitAddMagnetWithSpacesSurvivesURLSplit(t *testing.T) {
	f := &qbitFake{}
	h := NewQBitHandler(f, "arr", "secret")
	cc := login(t, h)
	magnet := "magnet:?xt=urn:btih:abc&dn=My Release"
	w := request(h, "POST", "/api/v2/torrents/add", "urls="+url.QueryEscape(magnet)+"&category=tv", "application/x-www-form-urlencoded", cc)
	if w.Code != http.StatusOK {
		t.Fatalf("add status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if f.addedMagnet != magnet+":tv" {
		t.Fatalf("AddMagnet received %q, want the full magnet %q (no whitespace split)", f.addedMagnet, magnet+":tv")
	}
	// CRLF-delimited URLs are the qBittorrent multi-URL form: the split
	// must drop the trailing \r of each line without touching the
	// spaces inside a magnet.
	f = &qbitFake{}
	h = NewQBitHandler(f, "arr", "secret")
	cc = login(t, h)
	multi := "magnet:?xt=urn:btih:a&dn=One Two\r\nmagnet:?xt=urn:btih:b&dn=Three Four"
	w = request(h, "POST", "/api/v2/torrents/add", "urls="+url.QueryEscape(multi), "application/x-www-form-urlencoded", cc)
	if w.Code != http.StatusOK {
		t.Fatalf("multi-url add status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if f.addedMagnet != "magnet:?xt=urn:btih:b&dn=Three Four:" {
		t.Fatalf("AddMagnet received %q, want the second magnet intact", f.addedMagnet)
	}
}
