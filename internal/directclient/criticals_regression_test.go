package directclient

// Regression tests for the blocking criticals of the round-1 AO semantic
// review of PR #104 at head acc2e019. Each test is named after the finding
// it guards; the "witness" probes in the review record established the
// failing-first behaviour against the unmodified head.

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ensingerphilipp/premiumizearr-nova/internal/config"
)

// TestFailedJobReservedFolderIsDeletedByPoll is the regression test for
// R1-3: a terminally failed job kept its reserved Premiumize folder forever
// because no path set the cleanup-retry flag, so the folder outlived the
// job with nothing left to delete it.
func TestFailedJobReservedFolderIsDeletedByPoll(t *testing.T) {
	var folderDeletes atomic.Int32
	pm := managerTestPremiumize(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/account/info":
			fmt.Fprint(w, `{"status":"success","limit_used":0}`)
		case "/api/folder/list":
			fmt.Fprint(w, `{"status":"success","content":[]}`)
		case "/api/folder/create":
			fmt.Fprint(w, `{"status":"success","id":"folder-1"}`)
		case "/api/transfer/create":
			fmt.Fprint(w, `{"status":"success","id":"transfer-1"}`)
		case "/api/transfer/list":
			fmt.Fprint(w, `{"status":"success","transfers":[{"id":"transfer-1","status":"error","message":"invalid source"}]}`)
		case "/api/folder/delete":
			if folderDeletes.Add(1) == 1 {
				http.Error(w, "transient outage", http.StatusServiceUnavailable)
				return
			}
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
	m.PollOnce(context.Background())
	m.mu.RLock()
	j := *m.jobs[id]
	m.mu.RUnlock()
	if j.Phase != "failed" || !j.CleanupPending {
		t.Fatalf("failed job = %#v, want failed with the reserved folder handed to the deletion pass", j)
	}
	// The first cleanup attempt fails transiently and must keep the flag.
	m.PollOnce(context.Background())
	m.mu.RLock()
	j = *m.jobs[id]
	m.mu.RUnlock()
	if !j.CleanupPending || j.CloudFolder == "" {
		t.Fatalf("after the transient cleanup failure = %#v, want still cleanup-pending", j)
	}
	if folderDeletes.Load() != 1 {
		t.Fatalf("folder delete attempts = %d, want 1", folderDeletes.Load())
	}
	// The retry succeeds and clears the reference WITHOUT deleting the row.
	m.PollOnce(context.Background())
	m.mu.RLock()
	j = *m.jobs[id]
	m.mu.RUnlock()
	if j.Phase != "failed" || j.CloudFolder != "" || j.CleanupPending {
		t.Fatalf("after the cleanup retry = %#v, want failed with the folder reference cleared", j)
	}
	if folderDeletes.Load() != 2 {
		t.Fatalf("folder delete attempts = %d, want 2", folderDeletes.Load())
	}
	if jobs := m.ListTorrents("tv"); len(jobs) != 1 || jobs[0].State != "failed" {
		t.Fatalf("job row = %#v, want the failed row to survive the folder cleanup", jobs)
	}
}

// TestAddMagnetRequeuesFailedJob is the regression test for the prior-round
// critical: the dedup path returned the existing terminally failed job as if
// nothing changed, so a *arr re-grab of the same release had no retry path.
func TestAddMagnetRequeuesFailedJob(t *testing.T) {
	pm := managerTestPremiumize(t, func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	m := newTestManager(t, &pm, t.TempDir())
	if err := m.AddMagnet(context.Background(), testMagnet, "tv"); err != nil {
		t.Fatal(err)
	}
	const id = "0123456789abcdef0123456789abcdef01234567"
	m.mu.Lock()
	m.jobs[id].Phase = "failed"
	m.jobs[id].Error = "submission failed"
	m.jobs[id].Progress = 0.5
	m.jobs[id].Downloaded = 42
	m.jobs[id].TotalBytes = 100
	m.mu.Unlock()
	// A *arr re-issues the grab through this endpoint to retry.
	if err := m.AddMagnet(context.Background(), testMagnet, "tv"); err != nil {
		t.Fatalf("re-grab of a failed release errored: %v", err)
	}
	m.mu.RLock()
	j := *m.jobs[id]
	m.mu.RUnlock()
	if j.Phase != "queued" || j.Error != "" || j.Progress != 0 || j.Downloaded != 0 || j.TotalBytes != 0 {
		t.Fatalf("failed job after re-grab = %#v, want a reset queued row", j)
	}
	// A different category is still rejected for an existing hash.
	if err := m.AddMagnet(context.Background(), testMagnet, "movies"); err == nil {
		t.Fatal("a different category was accepted for an existing hash")
	}
}

// TestFailureReportPollsAreCapped is the regression test for R1-5: the
// failure reporter was called for an unacknowledged job every poll forever,
// forcing a full *arr history refetch against every configured *arr each
// time. Passes are now bounded.
func TestFailureReportPollsAreCapped(t *testing.T) {
	var reporterCalls atomic.Int32
	pm := managerTestPremiumize(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/account/info":
			fmt.Fprint(w, `{"status":"success","limit_used":0}`)
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
	m.mu.Lock()
	m.jobs[id].Phase = "failed"
	m.jobs[id].Error = "submission failed"
	m.mu.Unlock()
	m.SetTorrentFailureReporter(func(job Job) ([]string, error) {
		reporterCalls.Add(1)
		return nil, nil // No *arr ever tracks this grab.
	})
	// Two polls beyond the bound: the reporter must stop being called.
	for i := 0; i < failedReportPollCap+2; i++ {
		m.PollOnce(context.Background())
	}
	if got := reporterCalls.Load(); got != failedReportPollCap {
		t.Fatalf("failure reporter calls = %d, want the %d-poll bound (an unacknowledged job must not pin the *arr stack with a refetch every poll)", got, failedReportPollCap)
	}
	m.mu.RLock()
	j := *m.jobs[id]
	m.mu.RUnlock()
	if j.Phase != "failed" {
		t.Fatalf("dead-lettered job = %#v, want still failed (it stays *arr-visible until removed)", j)
	}
}

// TestPollUnlocksBeforeFailureReportRuns is the regression test for R1-6:
// the deferred failure report ran its *arr network calls inside the pollMu
// critical section, so an unreachable *arr held the lock for the whole
// report and stalled every queued submission, in-flight download, and
// cleanup pass behind it. The lock must be released before the report runs.
func TestPollUnlocksBeforeFailureReportRuns(t *testing.T) {
	reporterEntered := make(chan struct{})
	release := make(chan struct{})
	pm := managerTestPremiumize(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/account/info":
			fmt.Fprint(w, `{"status":"success","limit_used":0}`)
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
	m.mu.Lock()
	m.jobs[id].Phase = "failed"
	m.jobs[id].Error = "submission failed"
	m.mu.Unlock()
	m.SetTorrentFailureReporter(func(job Job) ([]string, error) {
		close(reporterEntered)
		<-release
		return nil, nil
	})
	done := make(chan struct{})
	go func() {
		m.PollOnce(context.Background())
		close(done)
	}()
	select {
	case <-reporterEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("the failure reporter was never reached")
	}
	// While the reporter is blocked, the poll lock must already be free or
	// the next poll (and everything it advances) waits on the *arr call.
	if !m.pollMu.TryLock() {
		t.Fatal("pollMu is still held while the failure reporter runs")
	}
	m.pollMu.Unlock()
	close(release)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("PollOnce did not return after the reporter was released")
	}
}

// TestUnusableDownloadsDirectoryFallsBackForOutputRoot is the regression
// test for the prior-round critical: the direct output root bypassed the
// GetDownloadsBaseLocation validation, so an invalid configured directory
// (the filesystem root, a non-writeable path) was trusted into the import
// path the *arrs read. The validated fallback location must be used instead.
func TestUnusableDownloadsDirectoryFallsBackForOutputRoot(t *testing.T) {
	pm := managerTestPremiumize(t, func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	cases := []struct {
		name      string
		directory string
	}{
		{"filesystem root", "/"},
		{"nonexistent (non-writeable) path", filepath.Join(os.TempDir(), "definitely-not-a-real-dir-3f9c", "below")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{TransferDirectory: "arrDownloads", DownloadsDirectory: tc.directory}
			m, err := NewManager(&pm, cfg, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			want := filepath.Join(os.TempDir(), "premiumizearrd", "direct")
			if got := m.QBitOutputRoot(); got != want {
				t.Fatalf("QBitOutputRoot() = %q, want the validated fallback %q", got, want)
			}
			if got := m.SABOutputRoot(); got != want {
				t.Fatalf("SABOutputRoot() = %q, want the validated fallback %q", got, want)
			}
		})
	}
}

// TestCreatedCategoriesSurviveRestart is the regression test for the
// prior-round critical: CreateCategory in the emulated qBittorrent API kept
// categories in memory only, so every *arr category save after a restart
// re-conflicted against the lost list.
func TestCreatedCategoriesSurviveRestart(t *testing.T) {
	pm := managerTestPremiumize(t, func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	configDir := t.TempDir()
	m, err := NewManager(&pm, &config.Config{TransferDirectory: "arrDownloads", DownloadsDirectory: filepath.Join(t.TempDir(), "downloads")}, configDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.CreateCategory("custom-x"); err != nil {
		t.Fatal(err)
	}
	if err := m.CreateCategory("custom-x"); err != nil {
		t.Fatalf("re-creating the category errored: %v", err)
	}
	restarted, err := NewManager(&pm, &config.Config{TransferDirectory: "arrDownloads", DownloadsDirectory: filepath.Join(t.TempDir(), "downloads")}, configDir)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range restarted.ListCategories() {
		if c == "custom-x" {
			found = true
		}
	}
	if !found {
		t.Fatalf("category registry after restart = %v, want the created category to be listed", restarted.ListCategories())
	}
	// The same registry must also carry a category a job acquired through
	// add, and a re-created category must not duplicate the list.
	if err := m.AddMagnet(context.Background(), testMagnet, "jobcat"); err != nil {
		t.Fatal(err)
	}
	restarted2, err := NewManager(&pm, &config.Config{TransferDirectory: "arrDownloads", DownloadsDirectory: filepath.Join(t.TempDir(), "downloads")}, configDir)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, c := range restarted2.ListCategories() {
		counts[c]++
	}
	if counts["jobcat"] != 1 || counts["custom-x"] != 1 {
		t.Fatalf("category counts after restart = %v, want exactly one entry each for jobcat and custom-x", counts)
	}
}

// TestZeroSimultaneousDownloadsDoesNotStall is the regression test for the
// prior-round critical: SimultaneousDownloads 0 (a value the web route
// accepts) passed the load gate but bound the slot limit to zero, stalling
// every direct download at the cloud phase forever. Non-positive means
// unlimited here.
func TestZeroSimultaneousDownloadsDoesNotStall(t *testing.T) {
	pm := managerTestPremiumize(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/account/info":
			fmt.Fprint(w, `{"status":"success","limit_used":0}`)
		case "/api/folder/list":
			fmt.Fprint(w, `{"status":"success","content":[]}`)
		case "/api/transfer/list":
			fmt.Fprint(w, `{"status":"success","transfers":[{"id":"transfer-1","status":"finished"}]}`)
		default:
			http.NotFound(w, r)
		}
	})
	downloads := filepath.Join(t.TempDir(), "downloads")
	if err := os.MkdirAll(downloads, 0755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{TransferDirectory: "arrDownloads", DownloadsDirectory: downloads, SimultaneousDownloads: 0}
	m, err := NewManager(&pm, cfg, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := m.AddMagnet(context.Background(), testMagnet, "tv"); err != nil {
		t.Fatal(err)
	}
	const id = "0123456789abcdef0123456789abcdef01234567"
	m.mu.Lock()
	m.jobs[id].Phase = "cloud"
	m.jobs[id].TransferID = "transfer-1"
	m.jobs[id].CloudFolder = "folder-1"
	m.mu.Unlock()
	m.PollOnce(context.Background())
	waitQuiescent(t, m)
	m.mu.RLock()
	j := *m.jobs[id]
	m.mu.RUnlock()
	// The finished transfer must have ENTERED the download (the old zero
	// slot limit bailed before that, parking the job in the cloud phase
	// forever). The fake folder is empty, so the attempt fails locally —
	// the point is that it was attempted at all.
	if j.Phase == "cloud" {
		t.Fatalf("job stuck in the cloud phase with a zero download limit: %#v", j)
	}
	if j.Phase != "failed" || !strings.Contains(j.Error, "Local download failed") {
		t.Fatalf("job after the zero-limit download attempt = %#v, want failed with a local download error", j)
	}
}

// TestConfigUpdateIsRaceFreeForManagerReaders is the regression test for
// R1-4: the manager read fields of the shared App config struct while the
// config route rewrote it in place from an HTTP goroutine (data race, torn
// string reads). The manager now keeps its own value snapshot swapped by
// ConfigUpdatedCallback, so concurrent updates and reads are clean under
// -race.
func TestConfigUpdateIsRaceFreeForManagerReaders(t *testing.T) {
	pm := managerTestPremiumize(t, func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	m := newTestManager(t, &pm, t.TempDir())
	long := strings.Repeat("x", 512)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(4)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			newCfg := m.configSnapshot()
			newCfg.PremiumizemeAPIKey = long + fmt.Sprintf("%d", i)
			newCfg.DownloadsDirectory = long + fmt.Sprintf("/%d", i)
			newCfg.Arrs = []config.ArrConfig{{Name: long + fmt.Sprintf("%d", i), URL: "http://example.invalid", APIKey: "k", Type: config.Sonarr}}
			m.ConfigUpdatedCallback(m.configSnapshot(), newCfg)
		}
	}()
	for r := 0; r < 3; r++ {
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = m.QBitOutputRoot()
				_ = m.SABOutputRoot()
				_ = m.ListCategories()
				_ = m.ListTorrents("")
			}
		}()
	}
	time.Sleep(250 * time.Millisecond)
	close(stop)
	wg.Wait()
}
