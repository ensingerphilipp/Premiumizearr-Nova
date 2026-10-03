package directclient

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestQBitStaleSessionAllowsArrReauthentication(t *testing.T) {
	h := NewQBitHandler(&qbitFake{}, "premiumizearr", "key")
	form := url.Values{"username": {"premiumizearr"}, "password": {"key"}}
	r := httptest.NewRequest(http.MethodPost, "/api/v2/auth/login", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	cookie := w.Result().Cookies()[0]
	restarted := NewQBitHandler(&qbitFake{}, "premiumizearr", "key")
	r = httptest.NewRequest(http.MethodGet, "/api/v2/torrents/info", nil)
	r.AddCookie(cookie)
	w = httptest.NewRecorder()
	restarted.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("stale cookie returned %d; Sonarr only reauthenticates on 403", w.Code)
	}
	r = httptest.NewRequest(http.MethodPost, "/api/v2/auth/login", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	restarted.ServeHTTP(w, r)
	r = httptest.NewRequest(http.MethodGet, "/api/v2/torrents/info", nil)
	r.AddCookie(w.Result().Cookies()[0])
	w = httptest.NewRecorder()
	restarted.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("authenticated retry returned %d", w.Code)
	}
}

func TestManagerRemovalRetriesAfterFolderDeleteFailure(t *testing.T) {
	var transfers, folders atomic.Int32
	pm := managerTestPremiumize(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/transfer/delete":
			if transfers.Add(1) > 1 {
				fmt.Fprint(w, `{"status":"error","message":"Transfer not found"}`)
				return
			}
			fmt.Fprint(w, `{"status":"success"}`)
		case "/api/folder/delete":
			if folders.Add(1) == 1 {
				http.Error(w, "transient outage", 503)
				return
			}
			fmt.Fprint(w, `{"status":"success"}`)
		default:
			http.NotFound(w, r)
		}
	})
	configDir := t.TempDir()
	m := newTestManager(t, &pm, configDir)
	if err := m.AddMagnet(context.Background(), testMagnet, "tv"); err != nil {
		t.Fatal(err)
	}
	const id = "0123456789abcdef0123456789abcdef01234567"
	m.mu.Lock()
	m.jobs[id].Phase = "completed"
	m.jobs[id].TransferID = "transfer-1"
	m.jobs[id].CloudFolder = "folder-1"
	m.mu.Unlock()
	if err := m.RemoveTorrent(id, true); err == nil {
		t.Fatal("first removal should surface transient folder failure")
	}
	restarted, err := NewManager(&pm, m.config, configDir)
	if err != nil {
		t.Fatal(err)
	}
	if j := restarted.jobs[id]; j == nil || j.TransferID != "" || j.CloudFolder != "folder-1" || !j.DeleteRequested {
		t.Fatalf("cleanup progress was not persisted: %#v", j)
	}
	if err := restarted.RemoveTorrent(id, true); err != nil {
		t.Fatalf("cleanup cannot converge after transfer already deleted: %v; folder attempts=%d", err, folders.Load())
	}
	if transfers.Load() != 1 || folders.Load() != 2 || len(restarted.ListTorrents("tv")) != 0 {
		t.Fatalf("unexpected cleanup state: transfers=%d folders=%d", transfers.Load(), folders.Load())
	}
}

type lifecycleTransport func(*http.Request) (*http.Response, error)

func (f lifecycleTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestManagerSubmitRemovalRetainsFailedRemoteCleanup(t *testing.T) {
	var unavailable atomic.Bool
	unavailable.Store(true)
	pm := managerTestPremiumize(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/folder/list":
			fmt.Fprint(w, `{"status":"success","content":[]}`)
		case "/api/folder/create":
			fmt.Fprint(w, `{"status":"success","id":"folder-1"}`)
		case "/api/transfer/delete", "/api/folder/delete":
			if unavailable.Load() {
				http.Error(w, "transient outage", 503)
				return
			}
			fmt.Fprint(w, `{"status":"success"}`)
		case "/api/transfer/list":
			fmt.Fprint(w, `{"status":"success","transfers":[]}`)
		default:
			http.NotFound(w, r)
		}
	})
	m := newTestManager(t, &pm, t.TempDir())
	if err := m.AddMagnet(context.Background(), testMagnet, "tv"); err != nil {
		t.Fatal(err)
	}
	const id = "0123456789abcdef0123456789abcdef01234567"
	original := pm.HTTPClient.Transport
	pm.HTTPClient.Transport = lifecycleTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/api/transfer/create" {
			return original.RoundTrip(r)
		}
		// Model a successful response already received as a concurrent removal arrives.
		if err := m.RemoveTorrent(id, true); err != nil {
			t.Fatal(err)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"status":"success","id":"transfer-1"}`)), Header: make(http.Header), Request: r}, nil
	})
	if err := m.submit(context.Background(), id); err == nil {
		t.Fatal("submission/removal should report the remote cleanup failure")
	}
	if len(m.ListTorrents("tv")) == 0 {
		t.Fatal("row discarded despite remote deletion failing; transfer and folder now unowned")
	}
	unavailable.Store(false)
	m.PollOnce(context.Background())
	if len(m.ListTorrents("tv")) != 0 {
		t.Fatal("pending removal was not retried after the outage")
	}
}

func TestManagerRemovalConvergesWhenTransferAlreadyGone(t *testing.T) {
	pm := managerTestPremiumize(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/transfer/delete":
			fmt.Fprint(w, `{"status":"error","message":"Transfer not found"}`)
		case "/api/folder/delete":
			fmt.Fprint(w, `{"status":"success"}`)
		default:
			http.NotFound(w, r)
		}
	})
	m := newTestManager(t, &pm, t.TempDir())
	if err := m.AddMagnet(context.Background(), testMagnet, "tv"); err != nil {
		t.Fatal(err)
	}
	const id = "0123456789abcdef0123456789abcdef01234567"
	m.jobs[id].TransferID, m.jobs[id].CloudFolder = "transfer-1", "folder-1"
	if err := m.RemoveTorrent(id, true); err != nil {
		t.Fatal(err)
	}
	if len(m.ListTorrents("tv")) != 0 {
		t.Fatal("already-deleted transfer kept the job pending")
	}
}

func TestManagerRemovalRetainsRowOnLocalCleanupFailure(t *testing.T) {
	pm := managerTestPremiumize(t, func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	m := newTestManager(t, &pm, t.TempDir())
	if err := m.AddMagnet(context.Background(), testMagnet, "tv"); err != nil {
		t.Fatal(err)
	}
	const id = "0123456789abcdef0123456789abcdef01234567"
	source, err := m.jobSourcePath(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(source); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	blocked := filepath.Join(source, "blocked")
	if err := os.WriteFile(blocked, []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := m.RemoveTorrent(id, true); err == nil {
		t.Fatal("local cleanup should fail for a nonempty source directory")
	}
	if j := m.jobs[id]; j == nil || !j.DeleteRequested {
		t.Fatal("failed local cleanup discarded its pending row")
	}
	if err := os.Remove(blocked); err != nil {
		t.Fatal(err)
	}
	if err := m.RemoveTorrent(id, true); err != nil {
		t.Fatal(err)
	}
	if len(m.ListTorrents("tv")) != 0 {
		t.Fatal("local cleanup retry did not finish removal")
	}
}
