package directclient

import (
	"context"
	"fmt"
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
	restarted, err := NewManager(&pm, &m.config, configDir)
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

// TestManagerSubmitRemovalRetainsFailedRemoteCleanup is the round-2 rewrite
// of the round-1 lifecycle test. The round-1 version fabricated a 200
// response the transport never produced, which exercised the post-200
// cancellation race instead of the post-commit failure the finding is
// about. This version models the real outcome: the request reaches the
// server, which commits the transfer to the account, and the client's
// transport then reports a cancellation because a concurrent removal
// cancelled the in-flight request. The client never saw a response, so the
// row stores no transfer ID — only the reconcile scan over the account's
// transfer list can still reach the orphan.
func TestManagerSubmitRemovalRetainsFailedRemoteCleanup(t *testing.T) {
	var committed atomic.Bool
	var orphanDeletes, folderDeletes atomic.Int32
	pm := managerTestPremiumize(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/folder/list":
			fmt.Fprint(w, `{"status":"success","content":[]}`)
		case "/api/folder/create":
			fmt.Fprint(w, `{"status":"success","id":"folder-1"}`)
		case "/api/transfer/delete":
			// DeleteTransfer posts the id as a form field; the scan
			// deletes by the transfer's own ID, so count only the
			// deletion of the committed orphan.
			_ = r.ParseForm()
			if r.FormValue("id") == "T-orph-1" {
				orphanDeletes.Add(1)
			}
			fmt.Fprint(w, `{"status":"success"}`)
		case "/api/folder/delete":
			folderDeletes.Add(1)
			fmt.Fprint(w, `{"status":"success"}`)
		case "/api/transfer/list":
			// The account's truth the reconcile scan reads: the
			// transfer exists only if the create was committed
			// server-side.
			if committed.Load() {
				fmt.Fprint(w, `{"status":"success","transfers":[{"id":"T-orph-1","name":"Example.Release","status":"downloading","progress":0.5,"folder_id":"folder-1"}]}`)
			} else {
				fmt.Fprint(w, `{"status":"success","transfers":[]}`)
			}
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
		// The request reaches the server, which commits the transfer to
		// the account; then a concurrent removal arrives and cancels the
		// in-flight request before the client sees the response.
		committed.Store(true)
		if err := m.RemoveTorrent(id, true); err != nil {
			t.Fatal(err)
		}
		// The transport's post-cancellation outcome: no response at all.
		return nil, context.Canceled
	})
	if err := m.submit(context.Background(), id); err == nil {
		t.Fatal("a submission the client never saw a response for must not report success")
	}
	if orphanDeletes.Load() < 1 {
		t.Fatalf("orphan transfer deletes = %d, want >= 1: the committed transfer no row references is unowned until the reconcile scan finds it through the just-cleared folder", orphanDeletes.Load())
	}
	if folderDeletes.Load() < 1 {
		t.Fatalf("folder delete calls = %d, want >= 1: the reserved folder must not outlive the removal", folderDeletes.Load())
	}
	if len(m.ListTorrents("tv")) != 0 {
		t.Fatalf("row remains after the reconciled removal: %#v", m.ListTorrents("tv"))
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
