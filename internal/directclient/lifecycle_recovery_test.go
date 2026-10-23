package directclient

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/ensingerphilipp/premiumizearr-nova/internal/config"
	"github.com/ensingerphilipp/premiumizearr-nova/pkg/premiumizeme"
)

func recoveryTestManager(t *testing.T, pm *premiumizeme.Premiumizeme) *Manager {
	t.Helper()
	cfg := &config.Config{SimultaneousDownloads: 5, TransferDirectory: "arrDownloads"}
	m, err := NewManager(pm, cfg, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := m.AddMagnet(context.Background(), testMagnet, "tv"); err != nil {
		t.Fatal(err)
	}
	return m
}

type recoveryTransport func(*http.Request) (*http.Response, error)

func (f recoveryTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestPreSendFailureCanBeRemovedAndReadded(t *testing.T) {
	var createReachedServer, folderDeletes atomic.Int32
	pm := managerTestPremiumize(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/account/info":
			fmt.Fprint(w, `{"status":"success","limit_used":0,"booster_points":100}`)
		case "/api/folder/list":
			fmt.Fprint(w, `{"status":"success","content":[]}`)
		case "/api/folder/create":
			fmt.Fprint(w, `{"status":"success","id":"reserved-folder"}`)
		case "/api/transfer/create":
			createReachedServer.Add(1)
			fmt.Fprint(w, `{"status":"success","id":"unexpected"}`)
		case "/api/transfer/list":
			fmt.Fprint(w, `{"status":"success","transfers":[]}`)
		case "/api/folder/delete":
			folderDeletes.Add(1)
			fmt.Fprint(w, `{"status":"success"}`)
		default:
			http.NotFound(w, r)
		}
	})
	base := pm.HTTPClient.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	pm.HTTPClient.Transport = recoveryTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/api/transfer/create" {
			return nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
		}
		return base.RoundTrip(r)
	})
	m := recoveryTestManager(t, &pm)
	m.PollOnce(context.Background())
	const id = "0123456789abcdef0123456789abcdef01234567"
	if m.jobs[id].Phase != "failed" || m.jobs[id].CloudFolder == "" {
		t.Fatalf("submission did not reach the expected failure: %+v", m.jobs[id])
	}
	if err := m.RemoveTorrent(id, true); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		m.PollOnce(context.Background())
	}
	if createReachedServer.Load() != 0 || len(m.ListTorrents("tv")) != 0 || folderDeletes.Load() != 1 {
		t.Fatalf("rows=%d folderDeletes=%d creates=%d", len(m.ListTorrents("tv")), folderDeletes.Load(), createReachedServer.Load())
	}
	if err := m.AddMagnet(context.Background(), testMagnet, "tv"); err != nil {
		t.Fatal(err)
	}
	if m.jobs[id].Phase != "queued" {
		t.Fatalf("re-add did not queue a fresh job: %+v", m.jobs[id])
	}
}

func TestMissingCloudTransferReachesFailure(t *testing.T) {
	var gone atomic.Bool
	var creates atomic.Int32
	pm := managerTestPremiumize(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/account/info":
			fmt.Fprint(w, `{"status":"success","limit_used":0,"booster_points":100}`)
		case "/api/folder/list":
			fmt.Fprint(w, `{"status":"success","content":[]}`)
		case "/api/folder/create":
			fmt.Fprint(w, `{"status":"success","id":"reserved-folder"}`)
		case "/api/transfer/create":
			creates.Add(1)
			fmt.Fprint(w, `{"status":"success","id":"transfer-1"}`)
		case "/api/transfer/list":
			if gone.Load() {
				fmt.Fprint(w, `{"status":"success","transfers":[]}`)
			} else {
				fmt.Fprint(w, `{"status":"success","transfers":[{"id":"transfer-1","status":"running","progress":0.25,"folder_id":"reserved-folder"}]}`)
			}
		default:
			http.NotFound(w, r)
		}
	})
	m := recoveryTestManager(t, &pm)
	m.PollOnce(context.Background())
	const id = "0123456789abcdef0123456789abcdef01234567"
	if j := m.jobs[id]; j.Phase != "cloud" || j.TransferID != "transfer-1" {
		t.Fatalf("submission did not reach cloud: %+v", *j)
	}
	// The account holder removes the transfer out of band.
	gone.Store(true)
	for i := 0; i < 12; i++ {
		m.PollOnce(context.Background())
	}
	phase := m.jobs[id].Phase
	if err := m.AddMagnet(context.Background(), testMagnet, "tv"); err != nil {
		t.Fatal(err)
	}
	retriedPhase := m.jobs[id].Phase
	restarted, err := NewManager(&pm, &m.config, filepath.Dir(m.stateDir))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("after 12 absent listings: phase=%s regrab=%s restart=%s submissions=%d", phase, retriedPhase, restarted.jobs[id].Phase, creates.Load())
	if phase != "failed" || retriedPhase != "queued" {
		t.Fatal("missing remote transfer never fails and a re-grab cannot restart it")
	}
}

func TestCleanupRequeueWithoutOrphanUsesFreshFolder(t *testing.T) {
	runCleanupRequeue(t, false, false)
}

func TestCleanupRequeueWithOrphanUsesFreshFolder(t *testing.T) {
	runCleanupRequeue(t, true, false)
}

func TestCleanupRequeueRetainsKnownTransferForDeletion(t *testing.T) {
	runCleanupRequeue(t, false, true)
}

func runCleanupRequeue(t *testing.T, orphanPresent, knownTransfer bool) {
	t.Helper()
	entered, release := make(chan struct{}), make(chan struct{})
	var blockOnce, releaseOnce sync.Once
	var staleSubmissions atomic.Int32
	var orphanGone atomic.Bool
	var lateOrphan atomic.Bool
	var oldTransferDeletes atomic.Int32
	pm := managerTestPremiumize(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/account/info":
			fmt.Fprint(w, `{"status":"success","limit_used":0,"booster_points":100}`)
		case "/api/folder/list":
			fmt.Fprint(w, `{"status":"success","content":[]}`)
		case "/api/folder/create":
			fmt.Fprint(w, `{"status":"success","id":"fresh-folder"}`)
		case "/api/folder/delete":
			blockOnce.Do(func() { close(entered); <-release })
			fmt.Fprint(w, `{"status":"success"}`)
		case "/api/transfer/list":
			if (orphanPresent || lateOrphan.Load()) && !orphanGone.Load() {
				fmt.Fprint(w, `{"status":"success","transfers":[{"id":"orphan","status":"error","folder_id":"old-folder"}]}`)
			} else {
				fmt.Fprint(w, `{"status":"success","transfers":[]}`)
			}
		case "/api/transfer/delete":
			_ = r.ParseForm()
			if r.FormValue("id") == "old-transfer" {
				oldTransferDeletes.Add(1)
			}
			orphanGone.Store(true)
			fmt.Fprint(w, `{"status":"success"}`)
		case "/api/transfer/create":
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Error(err)
				return
			}
			defer r.MultipartForm.RemoveAll()
			if r.FormValue("folder_id") == "old-folder" {
				staleSubmissions.Add(1)
				fmt.Fprint(w, `{"status":"error","message":"folder does not exist"}`)
			} else {
				fmt.Fprint(w, `{"status":"success","id":"fresh-transfer"}`)
			}
		default:
			http.NotFound(w, r)
		}
	})
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	original := recoveryTestManager(t, &pm)
	const id = "0123456789abcdef0123456789abcdef01234567"
	original.jobs[id].Phase = "submitting"
	original.jobs[id].CloudFolder = "old-folder"
	if err := original.saveLocked(); err != nil {
		t.Fatal(err)
	}
	// Use the real boot recovery path to obtain the failed/unknown row.
	m, err := NewManager(&pm, &original.config, filepath.Dir(original.stateDir))
	if err != nil {
		t.Fatal(err)
	}
	if knownTransfer {
		m.jobs[id].TransferUnknown = false
		m.jobs[id].TransferID = "old-transfer"
	}
	done := make(chan struct{})
	go func() { m.PollOnce(context.Background()); close(done) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("folder cleanup never started")
	}
	if err := m.AddMagnet(context.Background(), testMagnet, "tv"); err != nil {
		t.Fatal(err)
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("poll did not finish")
	}
	j := *m.jobs[id]
	t.Logf("after requeue during cleanup: phase=%s folder=%s transfer=%s submissionsToDeletedFolder=%d", j.Phase, j.CloudFolder, j.TransferID, staleSubmissions.Load())
	if staleSubmissions.Load() != 0 || j.CloudFolder == "old-folder" || j.Phase != "cloud" {
		t.Fatal("retry reused the folder already deleted by the preceding attempt's cleanup")
	}
	if knownTransfer {
		m.PollOnce(context.Background())
		if oldTransferDeletes.Load() != 1 || len(m.jobs[id].PendingCleanup) != 0 || m.jobs[id].CloudFolder != "fresh-folder" {
			t.Fatalf("known previous transfer lost its cleanup handle: %+v", m.jobs[id])
		}
	} else if !orphanPresent {
		if len(j.PendingCleanup) != 1 || j.PendingCleanup[0].Folder != "old-folder" {
			t.Fatalf("uncertain previous attempt lost its cleanup handle: %+v", j)
		}
		restarted, err := NewManager(&pm, &m.config, filepath.Dir(m.stateDir))
		if err != nil {
			t.Fatal(err)
		}
		if len(restarted.jobs[id].PendingCleanup) != 1 {
			t.Fatal("previous attempt's cleanup did not survive restart")
		}
		lateOrphan.Store(true)
		restarted.PollOnce(context.Background())
		after := restarted.jobs[id]
		if !orphanGone.Load() || len(after.PendingCleanup) != 0 || after.CloudFolder != "fresh-folder" || after.TransferID != "fresh-transfer" {
			t.Fatalf("late orphan cleanup damaged the fresh attempt or failed to converge: %+v", after)
		}
	}
}

func TestMissingTransferGraceIgnoresErrorsAndResetsOnReappearance(t *testing.T) {
	var mode atomic.Int32
	pm := managerTestPremiumize(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/transfer/list" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch mode.Load() {
		case 0:
			fmt.Fprint(w, `{"status":"success","transfers":[{"id":"transfer-1","status":"running"}]}`)
		case 1:
			fmt.Fprint(w, `{"status":"success","transfers":[]}`)
		case 2:
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"status":"success","transfers":[]}`)
		case 3:
			fmt.Fprint(w, `{"status":"error","message":"temporarily unavailable"}`)
		}
	})
	m := recoveryTestManager(t, &pm)
	const id = "0123456789abcdef0123456789abcdef01234567"
	m.jobs[id].Phase, m.jobs[id].TransferID = "cloud", "transfer-1"
	assertCloud := func(passes int) {
		t.Helper()
		for i := 0; i < passes; i++ {
			m.PollOnce(context.Background())
			if j := m.jobs[id]; j.Phase != "cloud" {
				t.Fatalf("grace period failed a healthy job: %+v", j)
			}
		}
	}
	mode.Store(1)
	assertCloud(2)
	mode.Store(2)
	assertCloud(4)
	mode.Store(3)
	assertCloud(4)
	mode.Store(0)
	assertCloud(1)
	mode.Store(1)
	assertCloud(2)
	m.PollOnce(context.Background())
	if j := m.jobs[id]; j.Phase != "failed" || !strings.Contains(j.Error, "no longer present") {
		t.Fatalf("missing transfer did not fail after its grace period: %+v", j)
	}
}
