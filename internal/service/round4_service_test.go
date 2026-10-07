package service

// Round-4 regression tests for the service-side high-confidence criticals
// of the semantic rereview of c1624868 (PR #104): R2-3, R2-5, R3-36,
// R3-4, N2.

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ensingerphilipp/premiumizearr-nova/internal/config"
	"github.com/ensingerphilipp/premiumizearr-nova/internal/directclient"
	"github.com/ensingerphilipp/premiumizearr-nova/pkg/premiumizeme"
	"github.com/ensingerphilipp/premiumizearr-nova/pkg/stringqueue"
)

// TestRound4ArrsManagerStrikeIsolatesFailingTarget is the regression test
// for R2-3: failureTargets had no per-target fault isolation — a
// permanently erroring *arr made EVERY report pass return an error, so the
// err-gated failureReportPolls increment never ran and the manager's cap
// dead-letter was unreachable, while every pass still forced a full
// history refetch against every healthy *arr. A target that errors on two
// consecutive passes is skipped for the job's remaining passes; a target
// that queries cleanly again has its strike reset. (The pinned round-2
// manager test still requires outage PASSES to keep not counting — this
// isolates per TARGET, not per pass.)
func TestRound4ArrsManagerStrikeIsolatesFailingTarget(t *testing.T) {
	healthy := &round4FakeArr{name: "sonarr"}
	broken := &round4FakeArr{name: "radarr", permanentError: true}
	a := ArrsManagerService{}
	a.mu = &sync.RWMutex{}
	a.failureTargetStrikes = make(map[string]int)
	a.failureTargets = []directFailureTarget{{key: "sonarr", client: healthy}, {key: "radarr", client: broken}}

	job := directclient.Job{ID: "job-1"}
	for pass := 1; pass <= 12; pass++ {
		acked, err := a.ReportDirectTorrentFailure(job)
		switch pass {
		case 1:
			if err == nil {
				t.Fatalf("pass 1: err = nil, want the failing target's error")
			}
			if len(acked) != 1 || acked[0] != "sonarr" {
				t.Fatalf("pass 1: acknowledgements = %#v, want the healthy target only", acked)
			}
		case 2:
			if err == nil {
				t.Fatalf("pass 2: err = nil, want the failing target's second error")
			}
		default:
			if err != nil {
				t.Fatalf("pass %d: err = %v, want none (a struck-out target must not pin the pass's error set, or the manager's no-error-pass cap is unreachable)", pass, err)
			}
		}
		// The manager appends the acknowledged targets to the row's ack
		// list; from pass 2 on, the healthy target is already reported.
		for _, k := range acked {
			if !round4ContainsReport(job.ReportedFailures, k) {
				job.ReportedFailures = append(job.ReportedFailures, k)
			}
		}
	}
	if got := broken.queryCalls.Load(); got != 2 {
		t.Fatalf("failing target query calls = %d, want 2 (two consecutive errors strike the target out of the job's remaining passes)", got)
	}
	if got := healthy.queryCalls.Load(); got != 1 {
		t.Fatalf("healthy target query calls = %d, want 1 (acknowledged once, then skipped by the row's ack list)", got)
	}
	if got := healthy.markCalls.Load(); got != 1 {
		t.Fatalf("healthy target mark calls = %d, want 1", got)
	}
}

type round4FakeArr struct {
	name           string
	permanentError bool
	queryCalls     atomic.Int32
	markCalls      atomic.Int32
}

func (f *round4FakeArr) HistoryContains(name string) (int64, bool, error) {
	if f.permanentError {
		return 0, false, errors.New("permanent 401")
	}
	return -1, false, nil
}
func (f *round4FakeArr) HistoryContainsFresh(name string) (int64, bool, error) {
	if f.permanentError {
		return 0, false, errors.New("permanent 401")
	}
	return -1, false, nil
}
func (f *round4FakeArr) HistoryContainsDownloadIDFresh(downloadID string) (int64, bool, error) {
	f.queryCalls.Add(1)
	if f.permanentError {
		return 0, false, errors.New("permanent 401")
	}
	return 7, true, nil
}
func (f *round4FakeArr) MarkHistoryItemAsFailed(id int64) error {
	f.markCalls.Add(1)
	if f.permanentError {
		return errors.New("permanent 401")
	}
	return nil
}
func (f *round4FakeArr) HandleErrorTransfer(_ *premiumizeme.Transfer, _ int64, _ *premiumizeme.Premiumizeme) error {
	return nil
}
func (f *round4FakeArr) GetArrName() string { return f.name }

func round4ContainsReport(reported []string, target string) bool {
	for _, previous := range reported {
		if previous == target {
			return true
		}
	}
	return false
}

func round4DropMagnet(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("magnet:?xt=urn:btih:round4"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func round4WaitQueueLen(t *testing.T, s *DirectoryWatcherService, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if got := s.Queue.Len(); got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("queue length = %d, want %d", s.Queue.Len(), want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func round4WatcherBootService(t *testing.T, blackholeDir string) (*DirectoryWatcherService, *config.Config) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/folder/list":
			fmt.Fprint(w, `{"status":"success","content":[]}`)
		case "/api/folder/create":
			fmt.Fprint(w, `{"status":"success","id":"downloads-root"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	pm := premiumizeme.NewPremiumizemeClient("test-secret")
	pm.APIBaseURL = server.URL + "/api/"
	pm.HTTPClient = server.Client()

	cfg := &config.Config{
		TransferDirectory:      "arrDownloads",
		BlackholeDirectory:     blackholeDir,
		PollBlackholeDirectory: false,
	}
	svc := NewDirectoryWatcherService()
	s := &svc
	s.premiumizemeClient = &pm
	s.config = cfg
	// Start creates its own queue and starts the uploads processor; the
	// processor's first cycle sees an empty queue and sleeps ten seconds,
	// so the assertions below observe the queue before any upload cycle.
	s.Start()
	return s, cfg
}

// TestRound4WatcherStopsWhenBlackholeDirectoryDisappears is the regression
// test for R2-5: changing the blackhole path to a not-yet-existing
// directory used to leave the inotify watcher running on the PREVIOUS
// directory (the pre-PR unconditioned UpdatePath removed it) — it kept
// consuming create events and uploading into the queue while the new
// path was never watched. The missing-directory branch stops the existing
// watcher before logging.
func TestRound4WatcherStopsWhenBlackholeDirectoryDisappears(t *testing.T) {
	oldDir := t.TempDir()
	missingDir := filepath.Join(t.TempDir(), "not-yet")
	s, cfg := round4WatcherBootService(t, oldDir)

	s.mu.RLock()
	watcher := s.watchDirectory
	s.mu.RUnlock()
	if watcher == nil {
		t.Fatal("watcher did not start for the existing boot directory")
	}
	t.Cleanup(func() {
		s.mu.RLock()
		w := s.watchDirectory
		s.mu.RUnlock()
		if w != nil {
			_ = w.Stop()
		}
	})

	// Positive control: the live watcher consumes the old directory.
	round4DropMagnet(t, oldDir, "first.magnet")
	round4WaitQueueLen(t, s, 1)

	// The path changes to a directory that does not exist yet.
	oldCfg := *cfg
	cfg.BlackholeDirectory = missingDir
	s.ConfigUpdatedCallback(oldCfg, *cfg)

	s.mu.RLock()
	watcher = s.watchDirectory
	s.mu.RUnlock()
	if watcher != nil {
		t.Fatal("watcher still armed on the removed directory after the path changed to a missing one")
	}

	// ... so files dropped in the old directory no longer reach the queue.
	round4DropMagnet(t, oldDir, "second.magnet")
	time.Sleep(750 * time.Millisecond)
	if got := s.Queue.Len(); got != 1 {
		t.Fatalf("queue length = %d, want 1 (the removed directory must no longer be consumed)", got)
	}

	// The directory starts through the same path once a later update
	// points at an existing one.
	newDir := t.TempDir()
	round4DropMagnet(t, newDir, "third.magnet")
	oldCfg = *cfg
	cfg.BlackholeDirectory = newDir
	s.ConfigUpdatedCallback(oldCfg, *cfg)
	round4WaitQueueLen(t, s, 2)
}

// TestRound4WatcherRearmsWhenConfiguredDirectoryAppears is the regression
// test for R3-36: a boot with the configured directory missing left the
// watcher nil, and the only re-arm paths were Start() and a
// BlackholeDirectory CHANGE — a directory that appears at the
// ALREADY-CONFIGURED path (the callback triggered by any other config
// field) was never watched. The callback now re-arms a nil watcher whose
// configured directory exists.
func TestRound4WatcherRearmsWhenConfiguredDirectoryAppears(t *testing.T) {
	missingDir := filepath.Join(t.TempDir(), "appears-later")
	s, cfg := round4WatcherBootService(t, missingDir)

	s.mu.RLock()
	watcher := s.watchDirectory
	s.mu.RUnlock()
	if watcher != nil {
		t.Fatal("watcher started for a missing boot directory")
	}
	t.Cleanup(func() {
		s.mu.RLock()
		w := s.watchDirectory
		s.mu.RUnlock()
		if w != nil {
			_ = w.Stop()
		}
	})

	// The configured directory appears, and the triggering config save
	// changes an UNRELATED field: the path-comparison arm above cannot
	// fire, so only the appeared-directory arm can re-arm.
	if err := os.MkdirAll(missingDir, 0755); err != nil {
		t.Fatal(err)
	}
	round4DropMagnet(t, missingDir, "dropped.magnet")
	oldCfg := *cfg
	cfg.PollBlackholeIntervalMinutes = 17
	s.ConfigUpdatedCallback(oldCfg, *cfg)

	s.mu.RLock()
	watcher = s.watchDirectory
	s.mu.RUnlock()
	if watcher == nil {
		t.Fatal("watcher did not arm for the configured directory that appeared at the unchanged path")
	}
	// The initial scan picks up the already-dropped file.
	round4WaitQueueLen(t, s, 1)
}

// TestRound4PollLoopReadsConfigSnapshot is the regression test for R3-4:
// the poll-mode loop read the shared App config struct unsynchronized
// while the web route rewrites it in place (*c = _newConfig) — the exact
// race the PR fixed for the directclient manager, left in the sibling
// consumer. The long-lived loop now reads a value snapshot refreshed
// under the service mutex by Init and the config callback. Under `go
// test -race`, a loop that still reads the struct is reported here.
func TestRound4PollLoopReadsConfigSnapshot(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{
		BlackholeDirectory:           dir,
		PollBlackholeDirectory:       true,
		PollBlackholeIntervalMinutes: 0, // spin the loop for maximum overlap
	}
	svc := NewDirectoryWatcherService()
	s := &svc
	s.Init(nil, cfg)
	s.Queue = stringqueue.NewStringQueue()
	s.startBlackholeWatch(dir)

	s.mu.RLock()
	polling := s.polling
	s.mu.RUnlock()
	if !polling {
		t.Fatal("poller did not start")
	}

	// The web route rewrites the shared struct in place before the
	// callback copies it; mirror that rewrite loop against the running
	// poller.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 200; i++ {
			current := *cfg
			next := current
			next.DownloadSpeedLimit = i
			*cfg = next
			s.ConfigUpdatedCallback(current, next)
		}
		close(done)
	}()
	<-done

	// Stop the poller the way a config save would.
	current := *cfg
	next := current
	next.PollBlackholeDirectory = false
	*cfg = next
	s.ConfigUpdatedCallback(current, next)

	deadline := time.Now().Add(5 * time.Second)
	for {
		s.mu.RLock()
		stillPolling := s.polling
		speed := s.configSnapshot.DownloadSpeedLimit
		s.mu.RUnlock()
		if !stillPolling {
			if speed != 199 {
				t.Errorf("snapshot DownloadSpeedLimit = %d, want the last route rewrite (199)", speed)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("poller did not stop after the poll flag was cleared")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestRound4ZeroSimultaneousDownloadsGateUnlimited is the regression test
// for N2's transfer-gate half: a config that carries 0 through the web
// save and the load path must mean "no limit" at the legacy completed-item
// gate the same way it already does at the direct slot gate — the old
// predicate (count < 0) made a 0 a hard zero-cap that stalled every
// completed transfer forever.
func TestRound4ZeroSimultaneousDownloadsGateUnlimited(t *testing.T) {
	var createCalls, pasteCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/folder/list":
			fmt.Fprint(w, `{"status":"success","content":[{"id":"item-1","name":"Release.mkv","type":"file"}]}`)
		case "/api/folder/create":
			createCalls.Add(1)
			fmt.Fprint(w, `{"status":"success","id":"single-file-folder"}`)
		case "/api/folder/paste":
			pasteCalls.Add(1)
			fmt.Fprint(w, `{"status":"success"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	pm := premiumizeme.NewPremiumizemeClient("test-secret")
	pm.APIBaseURL = server.URL + "/api/"
	pm.HTTPClient = server.Client()

	m := TransferManagerService{}.New()
	m.premiumizemeClient = &pm
	m.downloadsFolderID = "downloads-root"
	m.config = &config.Config{SimultaneousDownloads: 0, DownloadsDirectory: t.TempDir()}
	// Three active top-level downloads: a positive cap would be
	// exhausted by them, so passing the gate proves the non-positive
	// limit is unlimited rather than merely "more than three".
	for _, name := range []string{"Show.A", "Show.B", "Show.C"} {
		m.addDownload(&premiumizeme.Item{Name: name}, true)
	}

	m.TaskCheckPremiumizeDownloadsFolder()

	if got := createCalls.Load(); got != 1 {
		t.Fatalf("single-file folder creations = %d, want 1 (a zero cap must not stall completed items)", got)
	}
	if got := pasteCalls.Load(); got != 1 {
		t.Fatalf("folder pastes = %d, want 1 (the completed item must be admitted to the download pipeline)", got)
	}
}
