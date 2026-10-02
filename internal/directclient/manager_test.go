package directclient

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ensingerphilipp/premiumizearr-nova/internal/config"
	"github.com/ensingerphilipp/premiumizearr-nova/pkg/premiumizeme"
)

// This exercises the real manager and Premiumize HTTP client as one flow.
// The fake server owns all Premiumize endpoints and the generated file URL;
// no account, sleep-based scheduler, or live Premiumize service is involved.
func TestManagerSubmissionRestartProgressAndCompletedDownload(t *testing.T) {
	const (
		magnet   = "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567&dn=Example.Release"
		transfer = "transfer-1"
		folder   = "job-folder-1"
		payload  = "finished media payload"
	)
	var transferStatus atomic.Value
	transferStatus.Store("downloading")
	var createCount atomic.Int32
	var folderCount atomic.Int32
	var api *httptest.Server
	api = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/media" && r.URL.Query().Get("apikey") != "test-key" {
			t.Errorf("%s %s missing API key", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/account/info":
			fmt.Fprint(w, `{"status":"success","limit_used":0,"booster_points":0}`)
		case "/api/folder/list":
			if id := r.URL.Query().Get("id"); id == folder {
				fmt.Fprint(w, `{"status":"success","content":[{"id":"file-1","name":"episode.mkv","type":"file"}]}`)
			} else {
				fmt.Fprint(w, `{"status":"success","content":[]}`)
			}
		case "/api/folder/create":
			if folderCount.Add(1) == 1 {
				if got := r.URL.Query().Get("name"); got != "arrDownloads-direct" {
					t.Errorf("root folder name = %q, want arrDownloads-direct", got)
				}
				fmt.Fprint(w, `{"status":"success","id":"direct-root"}`)
			} else {
				if got := r.URL.Query().Get("name"); got != "0123456789abcdef0123456789abcdef01234567" {
					t.Errorf("job folder name = %q, want torrent hash", got)
				}
				if got := r.URL.Query().Get("parent_id"); got != "direct-root" {
					t.Errorf("job folder parent = %q, want direct-root", got)
				}
				fmt.Fprint(w, `{"status":"success","id":"job-folder-1"}`)
			}
		case "/api/transfer/create":
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Errorf("parse transfer submission: %v", err)
			}
			if got := r.FormValue("src"); got != magnet {
				t.Errorf("submitted source = %q, want magnet payload", got)
			}
			if got := r.FormValue("folder_id"); got != folder {
				t.Errorf("submission folder_id = %q, want %q", got, folder)
			}
			createCount.Add(1)
			fmt.Fprintf(w, `{"status":"success","id":%q,"name":"Example.Release","type":"torrent"}`, transfer)
		case "/api/transfer/list":
			fmt.Fprintf(w, `{"status":"success","transfers":[{"id":%q,"name":"Example.Release","status":%q,"progress":0.5}]}`, transfer, transferStatus.Load().(string))
		case "/api/item/details":
			if r.URL.Query().Get("id") != "file-1" {
				t.Errorf("details id = %q, want file-1", r.URL.Query().Get("id"))
			}
			fmt.Fprintf(w, `{"status":"success","id":"file-1","name":"episode.mkv","type":"file","link":%q}`, api.URL+"/media")
		case "/media":
			w.Header().Set("Content-Type", "application/octet-stream")
			fmt.Fprint(w, payload)
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()

	// Every Premiumize call in this test routes through pm.HTTPClient above;
	// the process-global DefaultTransport must stay untouched for the rest
	// of the test process (no test may swap it).
	oldTransport := http.DefaultTransport
	t.Cleanup(func() {
		if http.DefaultTransport != oldTransport {
			t.Errorf("http.DefaultTransport was modified by the test")
		}
	})

	configDir := t.TempDir()
	downloadsDir := filepath.Join(t.TempDir(), "downloads")
	if err := os.MkdirAll(downloadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		TransferDirectory:     "arrDownloads",
		DownloadsDirectory:    downloadsDir,
		SimultaneousDownloads: 1,
		EnableTlsCheck:        true,
	}
	pm := premiumizeme.NewPremiumizemeClient("test-key")
	pm.APIBaseURL = api.URL + "/api/"
	pm.HTTPClient = api.Client()

	manager, err := NewManager(&pm, cfg, configDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.AddMagnet(context.Background(), magnet, "tv"); err != nil {
		t.Fatalf("queue magnet: %v", err)
	}
	wantID := "0123456789abcdef0123456789abcdef01234567"
	queued := manager.ListTorrents("tv")
	if len(queued) != 1 || queued[0].Hash != wantID || queued[0].State != "queued" {
		t.Fatalf("queued jobs = %#v", queued)
	}

	// First poll creates the cloud folder, submits exactly once, and publishes
	// Premiumize's progress through the qBittorrent-shaped view.
	manager.PollOnce(context.Background())
	progress := manager.ListTorrents("tv")
	if len(progress) != 1 || progress[0].Progress != 0.45 || progress[0].State != "downloading" {
		t.Fatalf("progress after first poll = %#v, want downloading at 0.45", progress)
	}
	if createCount.Load() != 1 {
		t.Fatalf("transfer/create calls = %d, want 1", createCount.Load())
	}

	// Simulate process restart. The transfer ID and progress must be recovered
	// from disk, and the resumed manager must poll the existing transfer rather
	// than submit the source a second time.
	restarted, err := NewManager(&pm, cfg, configDir)
	if err != nil {
		t.Fatalf("reload job registry: %v", err)
	}
	restored := restarted.ListTorrents("tv")
	if len(restored) != 1 || restored[0].Hash != wantID || restored[0].Progress != 0.45 {
		t.Fatalf("restored job = %#v", restored)
	}
	restarted.mu.RLock()
	restoredJobPtr := restarted.jobs[wantID]
	if restoredJobPtr == nil {
		restarted.mu.RUnlock()
		t.Fatalf("restored registry is missing job %q", wantID)
	}
	restoredJob := *restoredJobPtr
	restarted.mu.RUnlock()
	if restoredJob.TransferID != transfer || restoredJob.CloudFolder != folder {
		t.Fatalf("restored Premiumize references = transfer %q, folder %q", restoredJob.TransferID, restoredJob.CloudFolder)
	}
	transferStatus.Store("finished")
	restarted.PollOnce(context.Background())
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		jobs := restarted.ListTorrents("tv")
		if len(jobs) == 1 && jobs[0].State == "completed" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	completed := restarted.ListTorrents("tv")
	if len(completed) != 1 || completed[0].State != "completed" || completed[0].Progress != 1 {
		t.Fatalf("completed jobs = %#v", completed)
	}
	if createCount.Load() != 1 {
		t.Fatalf("transfer/create calls after restart = %d, want exactly 1", createCount.Load())
	}
	got, err := os.ReadFile(filepath.Join(completed[0].ContentPath, "episode.mkv"))
	if err != nil {
		t.Fatalf("read imported output: %v", err)
	}
	if string(got) != payload {
		t.Fatalf("downloaded content = %q, want %q", got, payload)
	}
	if strings.Contains(completed[0].ContentPath, ".partial") {
		t.Fatalf("published content path is still staged: %q", completed[0].ContentPath)
	}
	waitQuiescent(t, manager)
	waitQuiescent(t, restarted)
}

func TestManagerQuotaExhaustionKeepsJobQueued(t *testing.T) {
	var createCalls atomic.Int32
	pm := managerTestPremiumize(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/account/info":
			fmt.Fprint(w, `{"status":"success","limit_used":1,"booster_points":0}`)
		case "/api/transfer/create":
			createCalls.Add(1)
			fmt.Fprint(w, `{"status":"success","id":"unexpected"}`)
		default:
			http.NotFound(w, r)
		}
	})
	manager := newTestManager(t, &pm, t.TempDir())
	if err := manager.AddMagnet(context.Background(), testMagnet, "tv"); err != nil {
		t.Fatal(err)
	}
	manager.PollOnce(context.Background())
	jobs := manager.ListTorrents("tv")
	if len(jobs) != 1 || jobs[0].State != "queued" {
		t.Fatalf("jobs after exhausted quota = %#v, want queued", jobs)
	}
	if createCalls.Load() != 0 {
		t.Fatalf("transfer/create calls = %d, want 0", createCalls.Load())
	}
	waitQuiescent(t, manager)
}

func TestManagerFailedPremiumizeSubmissionStatus(t *testing.T) {
	var createCalls atomic.Int32
	pm := managerTestPremiumize(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/account/info":
			fmt.Fprint(w, `{"status":"success","limit_used":0,"booster_points":100}`)
		case "/api/folder/list":
			fmt.Fprint(w, `{"status":"success","content":[]}`)
		case "/api/folder/create":
			fmt.Fprint(w, `{"status":"success","id":"folder-1"}`)
		case "/api/transfer/create":
			createCalls.Add(1)
			fmt.Fprint(w, `{"status":"error","message":"invalid source"}`)
		case "/api/transfer/list":
			fmt.Fprint(w, `{"status":"success","transfers":[]}`)
		default:
			http.NotFound(w, r)
		}
	})
	manager := newTestManager(t, &pm, t.TempDir())
	if err := manager.AddMagnet(context.Background(), testMagnet, "tv"); err != nil {
		t.Fatal(err)
	}
	manager.PollOnce(context.Background())
	jobs := manager.ListTorrents("tv")
	if len(jobs) != 1 || jobs[0].State != "failed" || !strings.Contains(jobs[0].Error, "invalid source") {
		t.Fatalf("jobs after failed submission = %#v, want failed with Premiumize message", jobs)
	}
	if createCalls.Load() != 1 {
		t.Fatalf("transfer/create calls = %d, want 1", createCalls.Load())
	}
	waitQuiescent(t, manager)
}

func TestManagerCompletedJobRemovalLocalDataPolicy(t *testing.T) {
	for _, tc := range []struct {
		name        string
		deleteFiles bool
		wantLocal   bool
		wantFolder  int32
	}{
		{name: "preserve local data", deleteFiles: false, wantLocal: true, wantFolder: 1},
		{name: "delete local data", deleteFiles: true, wantLocal: false, wantFolder: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var transferDeletes, folderDeletes atomic.Int32
			pm := managerTestPremiumize(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/transfer/delete":
					transferDeletes.Add(1)
					fmt.Fprint(w, `{"status":"success"}`)
				case "/api/folder/delete":
					folderDeletes.Add(1)
					fmt.Fprint(w, `{"status":"success"}`)
				default:
					http.NotFound(w, r)
				}
			})
			manager := newTestManager(t, &pm, t.TempDir())
			if err := manager.AddMagnet(context.Background(), testMagnet, "tv"); err != nil {
				t.Fatal(err)
			}
			const jobID = "0123456789abcdef0123456789abcdef01234567"
			manager.mu.Lock()
			job := manager.jobs[jobID]
			job.Phase, job.Progress = "completed", 1
			job.TransferID, job.CloudFolder = "transfer-1", "folder-1"
			if err := manager.saveLocked(); err != nil {
				manager.mu.Unlock()
				t.Fatal(err)
			}
			manager.mu.Unlock()
			localFile := filepath.Join(job.OutputPath, "episode.mkv")
			if err := os.MkdirAll(job.OutputPath, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(localFile, []byte("media"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := manager.RemoveTorrent(jobID, tc.deleteFiles); err != nil {
				t.Fatalf("remove completed job: %v", err)
			}
			if _, err := os.Stat(localFile); (err == nil) != tc.wantLocal {
				t.Fatalf("local data exists = %v, want %v (stat err %v)", err == nil, tc.wantLocal, err)
			}
			if transferDeletes.Load() != 1 {
				t.Errorf("transfer delete calls = %d, want 1", transferDeletes.Load())
			}
			if folderDeletes.Load() != tc.wantFolder {
				t.Errorf("folder delete calls = %d, want %d", folderDeletes.Load(), tc.wantFolder)
			}
			if len(manager.ListTorrents("tv")) != 0 {
				t.Errorf("removed job remains in torrent list")
			}
			waitQuiescent(t, manager)
		})
	}
}

func TestManagerRestartMarksInterruptedSubmissionUnknown(t *testing.T) {
	pm := premiumizeme.NewPremiumizemeClient("test-key")
	configDir := t.TempDir()
	manager := newTestManager(t, &pm, configDir)
	if err := manager.AddMagnet(context.Background(), testMagnet, "tv"); err != nil {
		t.Fatal(err)
	}
	const jobID = "0123456789abcdef0123456789abcdef01234567"
	manager.mu.Lock()
	manager.jobs[jobID].Phase = "submitting"
	if err := manager.saveLocked(); err != nil {
		manager.mu.Unlock()
		t.Fatal(err)
	}
	manager.mu.Unlock()

	restarted, err := NewManager(&pm, &config.Config{TransferDirectory: "arrDownloads"}, configDir)
	if err != nil {
		t.Fatalf("restart manager: %v", err)
	}
	jobs := restarted.ListTorrents("tv")
	if len(jobs) != 1 || jobs[0].State != "failed" || !strings.Contains(jobs[0].Error, "outcome unknown") {
		t.Fatalf("job after interrupted submission restart = %#v, want failed with unknown outcome", jobs)
	}
	waitQuiescent(t, manager)
	waitQuiescent(t, restarted)
}

const testMagnet = "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567&dn=Example.Release"

func managerTestPremiumize(t *testing.T, handler http.HandlerFunc) premiumizeme.Premiumizeme {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	pm := premiumizeme.NewPremiumizemeClient("test-key")
	pm.APIBaseURL = server.URL + "/api/"
	pm.HTTPClient = server.Client()
	return pm
}

func newTestManager(t *testing.T, pm *premiumizeme.Premiumizeme, configDir string) *Manager {
	t.Helper()
	cfg := &config.Config{TransferDirectory: "arrDownloads", DownloadsDirectory: filepath.Join(t.TempDir(), "downloads"), SimultaneousDownloads: 1}
	if err := os.MkdirAll(cfg.DownloadsDirectory, 0755); err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(pm, cfg, configDir)
	if err != nil {
		t.Fatal(err)
	}
	return manager
}

// waitQuiescent fails the test if a manager goroutine (download work
// spawned by PollOnce) is still running when the deadline expires. A
// download goroutine performs its last state save before it releases its
// active slot, so an empty active map also means no manager write can
// still land in the test's TempDir after the test returns; without the
// wait, the t.TempDir cleanup races a lingering saveLocked into
// "directory not empty" failures on slow runners (CI).
func waitQuiescent(t *testing.T, m *Manager) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		m.mu.RLock()
		n := len(m.active)
		m.mu.RUnlock()
		if n == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("manager goroutines did not quiesce (active = %d)", n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestManagerRemovalRetriesFailedRemoteCleanup is the regression test for
// the finding that removal deleted the durable row before the remote
// deletes, so a failed DeleteTransfer orphaned the Premiumize object with
// nothing left to retry it. The row must survive the failed removal and be
// gone once the retried cleanup succeeds.
func TestManagerRemovalRetriesFailedRemoteCleanup(t *testing.T) {
	var transferDeletes, folderDeletes atomic.Int32
	pm := managerTestPremiumize(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/transfer/delete":
			if transferDeletes.Add(1) == 1 {
				http.Error(w, "premiumize unavailable", http.StatusServiceUnavailable)
				return
			}
			fmt.Fprint(w, `{"status":"success"}`)
		case "/api/folder/delete":
			folderDeletes.Add(1)
			fmt.Fprint(w, `{"status":"success"}`)
		default:
			http.NotFound(w, r)
		}
	})
	manager := newTestManager(t, &pm, t.TempDir())
	if err := manager.AddMagnet(context.Background(), testMagnet, "tv"); err != nil {
		t.Fatal(err)
	}
	const jobID = "0123456789abcdef0123456789abcdef01234567"
	manager.mu.Lock()
	job := manager.jobs[jobID]
	job.Phase, job.Progress = "completed", 1
	job.TransferID, job.CloudFolder = "transfer-1", "folder-1"
	if err := manager.saveLocked(); err != nil {
		manager.mu.Unlock()
		t.Fatal(err)
	}
	manager.mu.Unlock()

	if err := manager.RemoveTorrent(jobID, true); err == nil {
		t.Fatal("removal succeeded although the remote transfer delete failed")
	}
	jobs := manager.ListTorrents("tv")
	if len(jobs) != 1 || jobs[0].State != "failed" {
		t.Fatalf("job after failed remote cleanup = %#v, want failed (the row must survive)", jobs)
	}
	if !strings.Contains(jobs[0].Error, "cleanup pending") {
		t.Fatalf("job error after failed cleanup = %q, want the retry flag", jobs[0].Error)
	}

	if err := manager.RemoveTorrent(jobID, true); err != nil {
		t.Fatalf("retry removal failed: %v", err)
	}
	if got := transferDeletes.Load(); got != 2 {
		t.Fatalf("transfer delete calls after retry = %d, want 2", got)
	}
	if got := folderDeletes.Load(); got != 1 {
		t.Fatalf("folder delete calls after retry = %d, want 1", got)
	}
	if got := manager.ListTorrents("tv"); len(got) != 0 {
		t.Fatalf("job row remains after the successful retry: %#v", got)
	}
	waitQuiescent(t, manager)
}

// TestManagerRemovalDeletesStagingSibling is the regression test for the
// finding that deleteFiles removed only the published OutputPath, leaving
// the whole <path>.partial staging tree of an interrupted download behind.
func TestManagerRemovalDeletesStagingSibling(t *testing.T) {
	pm := managerTestPremiumize(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/transfer/delete", "/api/folder/delete":
			fmt.Fprint(w, `{"status":"success"}`)
		default:
			http.NotFound(w, r)
		}
	})
	manager := newTestManager(t, &pm, t.TempDir())
	if err := manager.AddMagnet(context.Background(), testMagnet, "tv"); err != nil {
		t.Fatal(err)
	}
	const jobID = "0123456789abcdef0123456789abcdef01234567"
	manager.mu.Lock()
	job := manager.jobs[jobID]
	job.Phase, job.Progress = "completed", 1
	job.TransferID, job.CloudFolder = "transfer-1", "folder-1"
	if err := manager.saveLocked(); err != nil {
		manager.mu.Unlock()
		t.Fatal(err)
	}
	manager.mu.Unlock()

	if err := os.MkdirAll(job.OutputPath, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(job.OutputPath, "episode.mkv"), []byte("media"), 0600); err != nil {
		t.Fatal(err)
	}
	staging := job.OutputPath + ".partial"
	if err := os.MkdirAll(staging, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, "episode.mkv"), []byte("in-flight bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	// A foreign sibling inside the direct namespace must survive.
	foreign := filepath.Join(filepath.Dir(job.OutputPath), "other-job")
	if err := os.MkdirAll(foreign, 0755); err != nil {
		t.Fatal(err)
	}

	if err := manager.RemoveTorrent(jobID, true); err != nil {
		t.Fatalf("remove completed job: %v", err)
	}
	if _, err := os.Stat(job.OutputPath); !os.IsNotExist(err) {
		t.Fatalf("published directory survives removal: %v", err)
	}
	if _, err := os.Stat(staging); !os.IsNotExist(err) {
		t.Fatalf("staging sibling .partial survives removal: %v", err)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Fatalf("foreign sibling directory was deleted: %v", err)
	}
	waitQuiescent(t, manager)
}

// TestManagerQuotaExhaustionKeepsCloudJobsPolling is the regression test for
// the P1 finding: the quota gate returned out of PollOnce, so the
// GetTransfers pass never ran and submitted (cloud) jobs froze forever on
// an exhausted account. The gate must condition queued submissions only.
func TestManagerQuotaExhaustionKeepsCloudJobsPolling(t *testing.T) {
	var createCalls atomic.Int32
	detailsParked := make(chan struct{})
	releaseDetails := make(chan struct{})
	pm := managerTestPremiumize(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/account/info":
			fmt.Fprint(w, `{"status":"success","limit_used":1,"booster_points":0}`)
		case "/api/folder/list":
			if r.URL.Query().Get("id") == "folder-1" {
				fmt.Fprint(w, `{"status":"success","content":[{"id":"file-1","name":"episode.mkv","type":"file"}]}`)
			} else {
				fmt.Fprint(w, `{"status":"success","content":[]}`)
			}
		case "/api/transfer/create":
			createCalls.Add(1)
			fmt.Fprint(w, `{"status":"success","id":"unexpected"}`)
		case "/api/transfer/list":
			fmt.Fprint(w, `{"status":"success","transfers":[{"id":"transfer-1","name":"Existing.Release","status":"finished","progress":1}]}`)
		case "/api/item/details":
			// Hold the link request so the download goroutine parks in a
			// deterministic phase ("local") for the assertion. The handler
			// must still terminate on the test's release: closing an
			// httptest server waits for in-flight handlers, and a bare
			// block here would deadlock the test's cleanup.
			detailsParked <- struct{}{}
			select {
			case <-releaseDetails:
			case <-r.Context().Done():
			}
		default:
			http.NotFound(w, r)
		}
	})
	manager := newTestManager(t, &pm, t.TempDir())
	if err := manager.AddMagnet(context.Background(), testMagnet, "tv"); err != nil {
		t.Fatal(err)
	}
	const jobID = "0123456789abcdef0123456789abcdef01234567"
	manager.mu.Lock()
	job := manager.jobs[jobID]
	job.Phase, job.TransferID, job.CloudFolder = "cloud", "transfer-1", "folder-1"
	if err := manager.saveLocked(); err != nil {
		manager.mu.Unlock()
		t.Fatal(err)
	}
	manager.mu.Unlock()

	manager.PollOnce(context.Background())

	if got := manager.ListTorrents("tv"); len(got) != 1 || got[0].State != "downloading" {
		t.Fatalf("cloud job after exhausted quota = %#v, want still moving toward a download", got)
	}
	manager.mu.RLock()
	active, phase := manager.active[jobID] != nil, manager.jobs[jobID].Phase
	manager.mu.RUnlock()
	if !active || phase != "local" {
		t.Fatalf("submitting-adjacent cloud job: active slot = %v, phase = %q; want the download started despite the exhausted quota", active, phase)
	}
	if got := createCalls.Load(); got != 0 {
		t.Fatalf("transfer/create calls = %d, want 0 (queued jobs must not submit while exhausted)", got)
	}
	// Unpark the download before the test ends so the server close cannot
	// wait on the parked handler; the aborted request lets the download
	// goroutine unwind on its own.
	<-detailsParked
	releaseDetails <- struct{}{}
	waitQuiescent(t, manager)
}

// TestManagerRootFolderFollowsTransferDirectory is the regression test for
// the finding that the direct root folder ID was memoized once forever: a
// runtime TransferDirectory change kept landing new jobs in the old
// <dir>-direct folder. The memo must be (directory, ID) and re-resolve on
// change.
func TestManagerRootFolderFollowsTransferDirectory(t *testing.T) {
	var rootCreates, jobParents []string
	pm := managerTestPremiumize(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/account/info":
			fmt.Fprint(w, `{"status":"success","limit_used":0,"booster_points":0}`)
		case "/api/folder/list":
			fmt.Fprint(w, `{"status":"success","content":[]}`)
		case "/api/folder/create":
			name := r.URL.Query().Get("name")
			if name == "first-direct" || name == "second-direct" {
				rootCreates = append(rootCreates, name)
				id := "root-" + strings.TrimSuffix(name, "-direct")
				fmt.Fprintf(w, `{"status":"success","id":%q}`, id)
			} else {
				jobParents = append(jobParents, r.URL.Query().Get("parent_id"))
				fmt.Fprintf(w, `{"status":"success","id":"job-folder"}`)
			}
		case "/api/transfer/create":
			fmt.Fprint(w, `{"status":"success","id":"transfer-1"}`)
		default:
			http.NotFound(w, r)
		}
	})
	manager := newTestManager(t, &pm, t.TempDir())
	manager.config.TransferDirectory = "first"
	if err := manager.AddMagnet(context.Background(), testMagnet, "tv"); err != nil {
		t.Fatal(err)
	}
	manager.PollOnce(context.Background())
	if len(rootCreates) != 1 || rootCreates[0] != "first-direct" {
		t.Fatalf("root folder resolutions = %v, want [first-direct]", rootCreates)
	}
	if len(jobParents) != 1 || jobParents[0] != "root-first" {
		t.Fatalf("first job folder parents = %v, want [root-first]", jobParents)
	}

	// The setting is runtime-mutable: the next job must resolve a fresh
	// <dir>-direct root instead of reusing the old folder.
	manager.config.TransferDirectory = "second"
	const secondHash = "fedcba9876543210fedcba9876543210fedcba98"
	if err := manager.AddMagnet(context.Background(), "magnet:?xt=urn:btih:"+secondHash, "tv"); err != nil {
		t.Fatal(err)
	}
	manager.PollOnce(context.Background())
	if len(rootCreates) != 2 || rootCreates[1] != "second-direct" {
		t.Fatalf("root folder resolutions after setting change = %v, want a fresh second-direct resolution", rootCreates)
	}
	if len(jobParents) != 2 || jobParents[1] != "root-second" {
		t.Fatalf("second job folder parents = %v, want [root-first root-second]", jobParents)
	}
	waitQuiescent(t, manager)
}

// TestManagerSubmittingJobRemovalLeavesNoOrphan is the regression test for
// the P1 finding that a job in the submitting phase was invisible to
// removal: the row was deleted and the in-flight submission then completed,
// leaving Premiumize holding a transfer nobody owns. Two interleavings:
// the removal lands before the submission registers (no transfer may be
// created; the job folder must be cleaned), and while the request is in
// flight (the transfer was never created client-side, or is deleted).
func TestManagerSubmittingJobRemovalLeavesNoOrphan(t *testing.T) {
	t.Run("removal before submission registers", func(t *testing.T) {
		// The root-folder and job-folder creations each park on arrival so
		// the test can delete the (still-queued) job while the submission
		// is in flight; the removal must take the direct-removal branch
		// and the in-flight work must not create a transfer.
		rootParked := make(chan struct{})
		releaseRoot := make(chan struct{})
		jobParked := make(chan struct{})
		releaseJob := make(chan struct{})
		var folderDeletes atomic.Int32
		pm := managerTestPremiumize(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/api/account/info":
				fmt.Fprint(w, `{"status":"success","limit_used":0,"booster_points":0}`)
			case "/api/folder/list":
				fmt.Fprint(w, `{"status":"success","content":[]}`)
			case "/api/folder/create":
				if r.URL.Query().Get("name") == "arrDownloads-direct" {
					rootParked <- struct{}{}
					select {
					case <-releaseRoot:
					case <-r.Context().Done():
						return
					}
					fmt.Fprint(w, `{"status":"success","id":"root-folder"}`)
					return
				}
				jobParked <- struct{}{}
				select {
				case <-releaseJob:
				case <-r.Context().Done():
					return
				}
				fmt.Fprint(w, `{"status":"success","id":"job-folder"}`)
			case "/api/folder/delete":
				folderDeletes.Add(1)
				fmt.Fprint(w, `{"status":"success"}`)
			case "/api/transfer/create":
				t.Error("transfer created for a concurrently removed job")
			default:
				http.NotFound(w, r)
			}
		})
		manager := newTestManager(t, &pm, t.TempDir())
		if err := manager.AddMagnet(context.Background(), testMagnet, "tv"); err != nil {
			t.Fatal(err)
		}
		const jobID = "0123456789abcdef0123456789abcdef01234567"
		polled := make(chan struct{})
		go func() {
			manager.PollOnce(context.Background())
			close(polled)
		}()
		<-rootParked
		if err := manager.RemoveTorrent(jobID, true); err != nil {
			t.Fatalf("remove queued job: %v", err)
		}
		if got := manager.ListTorrents("tv"); len(got) != 0 {
			t.Fatalf("row remains after removal: %#v", got)
		}
		releaseRoot <- struct{}{}
		<-jobParked
		releaseJob <- struct{}{}
		<-polled
		if got := manager.ListTorrents("tv"); len(got) != 0 {
			t.Fatalf("job row remains: %#v", got)
		}
		if got := folderDeletes.Load(); got != 1 {
			t.Fatalf("folder delete calls = %d, want 1 (the job folder must not outlive the removal)", got)
		}
		waitQuiescent(t, manager)
	})
	t.Run("removal while the request is in flight", func(t *testing.T) {
		releaseTransfer := make(chan struct{}, 1)
		var transferDeletes, folderDeletes atomic.Int32
		pm := managerTestPremiumize(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/api/account/info":
				fmt.Fprint(w, `{"status":"success","limit_used":0,"booster_points":0}`)
			case "/api/folder/list":
				fmt.Fprint(w, `{"status":"success","content":[]}`)
			case "/api/folder/create":
				fmt.Fprint(w, `{"status":"success","id":"job-folder"}`)
			case "/api/transfer/create":
				// Hold the submission; abort without a response when the
				// client cancels. The post-release context check closes the
				// race in which the release and the cancel both arrive.
				select {
				case <-releaseTransfer:
				case <-r.Context().Done():
					return
				}
				if r.Context().Err() != nil {
					return
				}
				fmt.Fprint(w, `{"status":"success","id":"transfer-1"}`)
			case "/api/transfer/delete":
				transferDeletes.Add(1)
				fmt.Fprint(w, `{"status":"success"}`)
			case "/api/folder/delete":
				folderDeletes.Add(1)
				fmt.Fprint(w, `{"status":"success"}`)
			default:
				http.NotFound(w, r)
			}
		})
		manager := newTestManager(t, &pm, t.TempDir())
		if err := manager.AddMagnet(context.Background(), testMagnet, "tv"); err != nil {
			t.Fatal(err)
		}
		const jobID = "0123456789abcdef0123456789abcdef01234567"
		polled := make(chan struct{})
		go func() {
			manager.PollOnce(context.Background())
			close(polled)
		}()
		deadline := time.Now().Add(10 * time.Second)
		for {
			manager.mu.RLock()
			phase := ""
			if j := manager.jobs[jobID]; j != nil {
				phase = j.Phase
			}
			manager.mu.RUnlock()
			if phase == "submitting" {
				break
			}
			select {
			case <-polled:
				t.Fatal("poll finished before the submission reached the in-flight phase")
			case <-time.After(5 * time.Millisecond):
			}
			if time.Now().After(deadline) {
				t.Fatal("job never reached the submitting phase")
			}
		}
		// remove() takes the active-cancel branch for a submitting job,
		// cancelling the in-flight request; then let the fake complete
		// whichever side of the release/cancel race it lands on.
		if err := manager.RemoveTorrent(jobID, true); err != nil {
			t.Fatalf("remove submitting job: %v", err)
		}
		releaseTransfer <- struct{}{}
		<-polled
		// Two deterministic interleavings: if the client stored the
		// transfer ID (its response won the cancel race), the first
		// poll's success path already deleted transfer and folder and
		// dropped the row; otherwise the row survived marked failed and
		// DeleteRequested for the second poll's deletion pass (a transfer
		// the client never saw a response for is undecidable from here —
		// the fail message documents it and is out of scope for this test).
		manager.mu.RLock()
		rowAfterFirstPoll := manager.jobs[jobID] != nil
		manager.mu.RUnlock()
		manager.PollOnce(context.Background())
		if got := manager.ListTorrents("tv"); len(got) != 0 {
			t.Fatalf("job row remains after the deletion pass: %#v", got)
		}
		if rowAfterFirstPoll {
			if got := transferDeletes.Load(); got != 0 {
				t.Fatalf("transfer delete calls = %d, want 0 (the client never stored an ID)", got)
			}
		} else {
			if got := transferDeletes.Load(); got != 1 {
				t.Fatalf("transfer delete calls = %d, want 1 (the stored transfer must be deleted)", got)
			}
		}
		if got := folderDeletes.Load(); got != 1 {
			t.Fatalf("folder delete calls = %d, want 1 (the job folder must not outlive the removal)", got)
		}
		waitQuiescent(t, manager)
	})
}

func TestJobSourcePathKeepsJobInsideStateDir(t *testing.T) {
	m := &Manager{stateDir: t.TempDir()}

	for _, id := range []string{"", "../escape", "a/b", `a\b`, "a..b"} {
		if _, err := m.jobSourcePath(id); err == nil {
			t.Fatalf("jobSourcePath(%q) succeeded, want rejection", id)
		}
	}

	const validID = "0123456789abcdef0123456789abcdef"
	path, err := m.jobSourcePath(validID)
	if err != nil {
		t.Fatalf("jobSourcePath(%q) = %v, want success", validID, err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatalf("Abs(%q) = %v", path, err)
	}
	base, err := filepath.Abs(m.stateDir)
	if err != nil {
		t.Fatalf("Abs(%q) = %v", m.stateDir, err)
	}
	if !strings.HasPrefix(abs, base) {
		t.Fatalf("source path %q escapes the state directory %q", abs, base)
	}
	if !strings.HasSuffix(abs, ".source") {
		t.Fatalf("source path %q does not end in .source", abs)
	}
}
