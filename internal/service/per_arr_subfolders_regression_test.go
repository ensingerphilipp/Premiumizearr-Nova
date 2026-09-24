package service

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ensingerphilipp/premiumizearr-nova/internal/config"
	"github.com/ensingerphilipp/premiumizearr-nova/internal/directory_watcher"
	"github.com/ensingerphilipp/premiumizearr-nova/pkg/premiumizeme"
	"github.com/ensingerphilipp/premiumizearr-nova/pkg/stringqueue"
	"gopkg.in/yaml.v2"
)

// pmeCall records one request the test pme stub received.
type pmeCall struct {
	Method string
	Path   string
	Query  map[string]string
}

// pmeStub serves the premiumize.me endpoints the service code under test
// uses, records every call, and keeps a mutable folder table so
// CreateFolder behaves like the real API (the new folder appears in the
// parent's listing). failAll makes every endpoint return 500; gateList
// (a channel that is closed to "open") makes folder/list wait while it is
// still open, and failListFor makes one parent's listing fail while all
// other endpoints keep working.
type pmeStub struct {
	server          *httptest.Server
	mu              sync.Mutex
	calls           []pmeCall
	table           map[string][]premiumizeme.Item
	failAll         bool
	gateList        chan struct{}
	failListFor     map[string]bool
	nextID          int
	transferTargets []string
	createdFolders  []string
	transfers       []premiumizeme.Transfer
}

func newPmeStub(t *testing.T, failAll bool) *pmeStub {
	t.Helper()
	s := &pmeStub{
		table:   map[string][]premiumizeme.Item{"": {{ID: "main-id", Name: "arrDownloads", Type: "folder"}}},
		failAll: failAll,
		nextID:  1,
	}
	s.server = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.server.Close)
	return s
}

// setGateList makes every folder/list request wait while gate is open
// (unclosed); closing it releases all of them at once.
func (s *pmeStub) setGateList(gate chan struct{}) {
	s.mu.Lock()
	s.gateList = gate
	s.mu.Unlock()
}

// setListFail makes the folder/list of one specific parent ID return 500
// while every other endpoint and parent keeps working.
func (s *pmeStub) setListFail(id string, fail bool) {
	s.mu.Lock()
	if s.failListFor == nil {
		s.failListFor = map[string]bool{}
	}
	s.failListFor[id] = fail
	s.mu.Unlock()
}

func (s *pmeStub) handle(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	call := pmeCall{Method: r.Method, Path: r.URL.Path, Query: map[string]string{}}
	for k, v := range r.URL.Query() {
		call.Query[k] = v[0]
	}
	s.calls = append(s.calls, call)

	if s.failAll {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"status":"error","message":"pme down"}`))
		return
	}

	switch {
	case r.URL.Path == "/api/account/info":
		_, _ = w.Write([]byte(`{"status":"success","limit_used":0.25,"booster_points":0}`))
	case r.URL.Path == "/api/folder/list":
		if s.gateList != nil {
			<-s.gateList
		}
		if s.failListFor[call.Query["id"]] {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"status":"error","message":"pme down"}`))
			return
		}
		items := s.table[call.Query["id"]]
		writeJSON(w, map[string]any{"status": "success", "content": items})
	case r.URL.Path == "/api/folder/create":
		id := fmt.Sprintf("created-%d", s.nextID)
		s.nextID++
		s.table[call.Query["parent_id"]] = append(s.table[call.Query["parent_id"]], premiumizeme.Item{ID: id, Name: call.Query["name"], Type: "folder"})
		s.createdFolders = append(s.createdFolders, call.Query["name"])
		writeJSON(w, map[string]any{"status": "success", "id": id})
	case r.URL.Path == "/api/transfer/create":
		_ = r.ParseMultipartForm(1 << 20)
		s.transferTargets = append(s.transferTargets, r.FormValue("folder_id"))
		_, _ = w.Write([]byte(`{"status":"success","id":"transfer-1"}`))
	case r.URL.Path == "/api/transfer/list":
		writeJSON(w, map[string]any{"status": "success", "transfers": s.transfers})
	case r.URL.Path == "/api/folder/paste":
		_, _ = w.Write([]byte(`{"status":"success"}`))
	case r.URL.Path == "/api/folder/delete":
		_, _ = w.Write([]byte(`{"status":"success"}`))
	case r.URL.Path == "/api/item/details":
		writeJSON(w, map[string]any{"status": "success", "type": "file", "link": "http://example.invalid/f"})
	default:
		http.NotFound(w, r)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	_ = json.NewEncoder(w).Encode(v)
}

func (s *pmeStub) anyCall(pred func(pmeCall) bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.calls {
		if pred(c) {
			return true
		}
	}
	return false
}

// assertNoCallWithin fails (immediately) as soon as a matching call is
// observed, and keeps watching until timeout: an async goroutine that
// triggers the forbidden call after the first poll must still be caught.
func assertNoCallWithin(t *testing.T, s *pmeStub, timeout time.Duration, what string, pred func(pmeCall) bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if s.anyCall(pred) {
			t.Fatalf("%s: %v", what, s.calls)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// waitForCall fails if no matching call is observed within timeout; unlike
// assertNoCallWithin it succeeds on the first observation.
func waitForCall(t *testing.T, s *pmeStub, timeout time.Duration, what string, pred func(pmeCall) bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if s.anyCall(pred) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s; calls = %v", what, s.calls)
}

func newPmeTestClient(t *testing.T, s *pmeStub) *premiumizeme.Premiumizeme {
	t.Helper()
	client := premiumizeme.NewPremiumizemeClient("test-key")
	client.APIBaseURL = s.server.URL + "/api/"
	client.HTTPClient = s.server.Client()
	return &client
}

// newArrSubfolderTestService builds a directory watcher service pointed at a
// stub pme, the way Start() would leave it after the downloads folder was
// resolved: downloadsFolderID set, no watcher (watch mode off).
func newArrSubfolderTestService(t *testing.T, stub *pmeStub, blackhole string, enabled bool, arrs []config.ArrConfig) *DirectoryWatcherService {
	t.Helper()
	dw := NewDirectoryWatcherService()
	dw.Init(newPmeTestClient(t, stub), &config.Config{
		BlackholeDirectory:  blackhole,
		TransferDirectory:   "arrDownloads",
		EnableArrSubfolders: enabled,
		Arrs:                arrs,
	})
	dw.Queue = stringqueue.NewStringQueue()
	dw.downloadsFolderID = "main-id"
	return &dw
}

// TestUpdateConfigRaceAgainstWatcherReaders is the regression test for
// findings R1-3, R1-21 and R1-24: UpdateConfig's in-place struct swap must
// be protected against the service's goroutines (fsnotify readers, the
// upload processor, the config callback) by the one registered mutex, and
// every new read/write site of the shared config and service state must take
// it. Run with -race; the pre-fix tree reports data races on the Arvs slice
// header, BlackholeDirectory string header, the arrFolders map, and
// downloadsFolderID.
func TestUpdateConfigRaceAgainstWatcherReaders(t *testing.T) {
	bh1 := t.TempDir()
	bh2 := t.TempDir()
	sonarrDir1 := filepath.Join(bh1, "sonarr")
	if err := os.MkdirAll(sonarrDir1, 0o755); err != nil {
		t.Fatal(err)
	}
	fileA := filepath.Join(bh1, "root.magnet")
	fileB := filepath.Join(sonarrDir1, "episode.magnet")
	if err := os.WriteFile(fileA, []byte("magnet:?xt=urn:btih:a"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fileB, []byte("magnet:?xt=urn:btih:b"), 0o600); err != nil {
		t.Fatal(err)
	}

	sonarr := config.ArrConfig{Name: "sonarr", URL: "http://127.0.0.1:8989", APIKey: "k", Type: config.Sonarr}
	radarr := config.ArrConfig{Name: "radarr", URL: "http://127.0.0.1:7878", APIKey: "k", Type: config.Radarr}

	dw := NewDirectoryWatcherService()
	// The callback is installed through the product load path so the
	// unexported appCallback/altConfigLocation fields are set the way
	// cmd/premiumizearrd sets them.
	cfg, err := config.LoadOrCreateConfig(t.TempDir(), func(oldConfig, newConfig config.Config) {
		dw.ConfigUpdatedCallback(oldConfig, newConfig)
	})
	if err != nil {
		t.Fatalf("LoadOrCreateConfig: %v", err)
	}
	cfgArrs := []config.ArrConfig{sonarr}
	cfg.BlackholeDirectory = bh1
	cfg.DownloadsDirectory = t.TempDir()
	cfg.TransferDirectory = "arrDownloads"
	cfg.EnableArrSubfolders = true
	cfg.Arrs = cfgArrs

	stub := newPmeStub(t, false)
	dw.Init(newPmeTestClient(t, stub), &cfg)
	dw.Queue = stringqueue.NewStringQueue()
	dw.downloadsFolderID = "main-id"

	start := make(chan struct{})
	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Web-save shape: whole-struct swap through UpdateConfig, which runs the
	// config callback (map snapshot loop + resolveArrFolders) afterwards.
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			newCfg := config.Config{
				PremiumizemeAPIKey:                      "test-key",
				BlackholeDirectory:                      bh1,
				DownloadsDirectory:                      cfg.DownloadsDirectory,
				TransferDirectory:                       "arrDownloads",
				PollBlackholeIntervalMinutes:            10,
				SimultaneousDownloads:                   5,
				DownloadSpeedLimit:                      100,
				ArrHistoryUpdateIntervalSeconds:         20,
				ErroredTransferDeleteGracePeriodSeconds: 300,
			}
			// Vary slice length, toggle and blackhole target on every
			// swap so the torn-header windows are exercised, like a user
			// editing the config in the web UI.
			if i%2 == 0 {
				newCfg.EnableArrSubfolders = true
				newCfg.Arrs = []config.ArrConfig{sonarr, radarr}
			} else {
				newCfg.EnableArrSubfolders = false
				newCfg.Arrs = []config.ArrConfig{sonarr}
				newCfg.BlackholeDirectory = bh2
			}
			cfg.UpdateConfig(newCfg)
		}
	}()

	// processUploads goroutine shape: the upload processor resolves and
	// processes files, rewrites downloadsFolderID on transfer-directory
	// changes, and writes resolved slugs into the map.
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if i%2 == 0 {
				dw.processUpload(fileA)
			} else {
				dw.processUpload(fileB)
			}
			// Re-create whatever processUpload consumed so the loop keeps
			// exercising the same read sites.
			if err := os.WriteFile(fileA, []byte("magnet:?xt=urn:btih:a"), 0o600); err != nil {
				t.Error(err)
				return
			}
			if err := os.MkdirAll(sonarrDir1, 0o755); err != nil {
				t.Error(err)
				return
			}
			if err := os.WriteFile(fileB, []byte("magnet:?xt=urn:btih:b"), 0o600); err != nil {
				t.Error(err)
				return
			}
			if i%10 == 0 {
				dw.resolveSingleArrFolder("sonarr", "main-id")
			}
			if i%15 == 0 {
				dw.setTransferDirectory("arrDownloads")
			}
		}
	}()

	// fsnotify reader shape: the event loop's checkFile and the poller's
	// root directoryScan read the config on every iteration.
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for {
			select {
			case <-stop:
				return
			default:
			}
			dw.checkFile(fileA)
			dw.checkFile(sonarrDir1)
			dw.isConfiguredArrSlug("sonarr")
			dw.isConfiguredArrSlug("radarr")
		}
	}()

	close(start)
	time.Sleep(2500 * time.Millisecond)
	close(stop)
	wg.Wait()
	// The reconfiguration worker is process-wide and asynchronous: drain
	// the saves this test enqueued before the temp dirs go.
	config.WaitReconfigIdle()
	// Let callback-spawned directory scans drain before the temp dirs go.
	time.Sleep(200 * time.Millisecond)
}

// TestProcessUploadSubfolderGates is the regression test for finding R1-2:
// processUpload must route only files inside the CURRENT blackhole whose
// parent basename is a configured Arr slug into per-Arr resolution.
// Everything else keeps the pre-feature main-folder destination, and no pme
// folder is created for an arbitrary name.
func TestProcessUploadSubfolderGates(t *testing.T) {
	sonarr := config.ArrConfig{Name: "sonarr", URL: "http://127.0.0.1:8989", APIKey: "k", Type: config.Sonarr}

	t.Run("feature off keeps main folder destination", func(t *testing.T) {
		stub := newPmeStub(t, false)
		bh := t.TempDir()
		sub := filepath.Join(bh, "sonarr")
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		file := filepath.Join(sub, "x.magnet")
		if err := os.WriteFile(file, []byte("magnet:?xt=urn:btih:x"), 0o600); err != nil {
			t.Fatal(err)
		}

		dw := newArrSubfolderTestService(t, stub, bh, false, nil)
		dw.processUpload(file)

		if got := stub.transferTargets; len(got) != 1 || got[0] != "main-id" {
			t.Fatalf("transfer targets = %v, want [main-id]", got)
		}
		if len(stub.createdFolders) != 0 {
			t.Fatalf("pme folders created = %v, want none", stub.createdFolders)
		}
		if _, err := os.Stat(file); !os.IsNotExist(err) {
			t.Fatalf("source file still exists after successful transfer: %v", err)
		}
	})

	t.Run("feature on unconfigured subfolder keeps main folder destination", func(t *testing.T) {
		stub := newPmeStub(t, false)
		bh := t.TempDir()
		sub := filepath.Join(bh, "not-an-arr")
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		file := filepath.Join(sub, "y.magnet")
		if err := os.WriteFile(file, []byte("magnet:?xt=urn:btih:y"), 0o600); err != nil {
			t.Fatal(err)
		}

		dw := newArrSubfolderTestService(t, stub, bh, true, []config.ArrConfig{sonarr})
		dw.processUpload(file)

		if got := stub.transferTargets; len(got) != 1 || got[0] != "main-id" {
			t.Fatalf("transfer targets = %v, want [main-id]", got)
		}
		if len(stub.createdFolders) != 0 {
			t.Fatalf("pme folders created = %v, want none for an unconfigured name", stub.createdFolders)
		}
		if _, err := os.Stat(file); !os.IsNotExist(err) {
			t.Fatalf("source file still exists after successful transfer: %v", err)
		}
	})

	t.Run("old blackhole leftover goes to main folder", func(t *testing.T) {
		stub := newPmeStub(t, false)
		oldBh := t.TempDir()
		oldSub := filepath.Join(oldBh, "sonarr")
		if err := os.MkdirAll(oldSub, 0o755); err != nil {
			t.Fatal(err)
		}
		file := filepath.Join(oldSub, "z.magnet")
		if err := os.WriteFile(file, []byte("magnet:?xt=urn:btih:z"), 0o600); err != nil {
			t.Fatal(err)
		}

		dw := newArrSubfolderTestService(t, stub, t.TempDir(), true, []config.ArrConfig{sonarr})
		dw.processUpload(file)

		if got := stub.transferTargets; len(got) != 1 || got[0] != "main-id" {
			t.Fatalf("transfer targets = %v, want [main-id] for a previous blackhole location", got)
		}
		if len(stub.createdFolders) != 0 {
			t.Fatalf("pme folders created = %v, want none for a parent outside the blackhole", stub.createdFolders)
		}
		if _, err := os.Stat(file); !os.IsNotExist(err) {
			t.Fatalf("source file still exists after successful transfer: %v", err)
		}
	})

	t.Run("configured slug still resolves its subfolder", func(t *testing.T) {
		stub := newPmeStub(t, false)
		bh := t.TempDir()
		sub := filepath.Join(bh, "sonarr")
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		file := filepath.Join(sub, "w.nzb")
		if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}

		dw := newArrSubfolderTestService(t, stub, bh, true, []config.ArrConfig{sonarr})
		dw.processUpload(file)

		if got := stub.transferTargets; len(got) != 1 || got[0] == "" || got[0] == "main-id" {
			t.Fatalf("transfer targets = %v, want the resolved Arr subfolder ID", got)
		}
		if len(stub.createdFolders) != 1 || stub.createdFolders[0] != "sonarr" {
			t.Fatalf("pme folders created = %v, want [sonarr]", stub.createdFolders)
		}
		dw.mu.RLock()
		id, ok := dw.arrFolders["sonarr"]
		dw.mu.RUnlock()
		if !ok || id != stub.transferTargets[0] {
			t.Fatalf("arrFolders[sonarr] = %q, %v; want the ID the transfer used (%q)", id, ok, stub.transferTargets[0])
		}
	})
}

// TestResolveArrFoldersCarriesOverOnPmeFailure is the regression test for
// finding R1-4: a transient premiumize.me failure during re-resolution must
// not discard a previously resolved subfolder ID, and the local subfolder
// must still be created, watched and scanned so the upload can retry once
// premiumize.me recovers.
func TestResolveArrFoldersCarriesOverOnPmeFailure(t *testing.T) {
	sonarr := config.ArrConfig{Name: "sonarr", URL: "http://127.0.0.1:8989", APIKey: "k", Type: config.Sonarr}

	stub := newPmeStub(t, true)
	bh := t.TempDir()
	dw := newArrSubfolderTestService(t, stub, bh, true, []config.ArrConfig{sonarr})
	dw.mu.Lock()
	dw.arrFolders = map[string]string{"sonarr": "existing-id"}
	dw.mu.Unlock()

	dw.resolveArrFolders()

	dw.mu.RLock()
	id, present := dw.arrFolders["sonarr"]
	dw.mu.RUnlock()
	if !present || id != "existing-id" {
		t.Fatalf("arrFolders[sonarr] = %q, present=%v; the previously resolved ID must survive a pme failure", id, present)
	}
	if _, err := os.Stat(filepath.Join(bh, "sonarr")); err != nil {
		t.Fatalf("blackhole subfolder not created during the pme outage: %v", err)
	}
}

// TestResolveArrFoldersNoPanicOnUninitializedWatcher is the regression test
// for finding R1-22: a watcher whose Watch() failed leaves Watcher nil, and
// every AddWatchPath/RemoveWatchPath/UpdatePath call site must degrade to a
// logged no-op instead of nil-dereferencing the fsnotify Watcher.
func TestResolveArrFoldersNoPanicOnUninitializedWatcher(t *testing.T) {
	sonarr := config.ArrConfig{Name: "sonarr", URL: "http://127.0.0.1:8989", APIKey: "k", Type: config.Sonarr}

	stub := newPmeStub(t, false)
	bh := t.TempDir()
	dw := newArrSubfolderTestService(t, stub, bh, true, []config.ArrConfig{sonarr})
	// Exactly the state a failed Watch() leaves behind.
	dw.watchDirectory = &directory_watcher.WatchDirectory{}

	dw.resolveArrFolders()

	dw.mu.RLock()
	id, ok := dw.arrFolders["sonarr"]
	dw.mu.RUnlock()
	if !ok || id == "" {
		t.Fatalf("arrFolders[sonarr] = %q, %v; want the resolved ID despite the unavailable watcher", id, ok)
	}
	if _, err := os.Stat(filepath.Join(bh, "sonarr")); err != nil {
		t.Fatalf("blackhole subfolder not created: %v", err)
	}

	// The guards themselves: the zero-value watcher reports "not
	// initialized" instead of panicking; Stop degrades to a no-op.
	wd := &directory_watcher.WatchDirectory{}
	for name, fn := range map[string]func() error{
		"AddWatchPath":    func() error { return wd.AddWatchPath("/does/not/matter") },
		"RemoveWatchPath": func() error { return wd.RemoveWatchPath("/does/not/matter") },
		"UpdatePath":      func() error { return wd.UpdatePath("/does/not/matter") },
	} {
		if err := fn(); err == nil {
			t.Errorf("%s on an uninitialized watcher: err = nil, want an error", name)
		}
	}
	if err := wd.Stop(); err != nil {
		t.Errorf("Stop on an uninitialized watcher: err = %v, want nil", err)
	}
}

func newManagerTestService(t *testing.T, stub *pmeStub, cfg *config.Config, arrFolders map[string]string) *TransferManagerService {
	t.Helper()
	m := TransferManagerService{}.New()
	m.premiumizemeClient = newPmeTestClient(t, stub)
	m.config = cfg
	m.downloadsFolderID = "main-id"
	m.arrFoldersMutex.Lock()
	m.arrFolders = arrFolders
	m.arrFoldersMutex.Unlock()
	return &m
}

// TestManagerRenamedArrStaysExcludedFromRootScan is the regression test for
// finding R1-5: an Arr that is renamed away (or that fails re-resolution)
// must stay tracked in arrFolders with an empty ID, so its pme folder keeps
// being excluded from the root scan's download-and-delete path for the
// lifetime of the process; empty IDs are skipped by the per-slug pass.
func TestManagerRenamedArrStaysExcludedFromRootScan(t *testing.T) {
	renamed := config.ArrConfig{Name: "sonarr-2", URL: "http://127.0.0.1:8989", APIKey: "k", Type: config.Sonarr}

	stub := newPmeStub(t, false)
	stub.mu.Lock()
	// The pme root still holds the folder of the Arr under its old name.
	stub.table["main-id"] = []premiumizeme.Item{{ID: "old-sonarr-id", Name: "sonarr", Type: "folder"}}
	stub.mu.Unlock()

	m := newManagerTestService(t, stub, &config.Config{
		EnableArrSubfolders:   true,
		Arrs:                  []config.ArrConfig{renamed},
		DownloadsDirectory:    t.TempDir(),
		TransferDirectory:     "arrDownloads",
		SimultaneousDownloads: 5,
		DownloadSpeedLimit:    100,
	}, map[string]string{"sonarr": "old-sonarr-id"})

	m.resolveArrFolders()

	m.arrFoldersMutex.Lock()
	oldID, oldPresent := m.arrFolders["sonarr"]
	newID, newPresent := m.arrFolders["sonarr-2"]
	m.arrFoldersMutex.Unlock()
	if !oldPresent || oldID != "" {
		t.Fatalf("arrFolders[sonarr] = %q, present=%v; a renamed-away Arr must stay tracked with an empty ID", oldID, oldPresent)
	}
	if !newPresent || newID == "" {
		t.Fatalf("arrFolders[sonarr-2] = %q, present=%v; want the newly resolved ID", newID, newPresent)
	}

	m.TaskCheckPremiumizeDownloadsFolder()

	// The old folder must not be handed to HandleFinishedItem (listing,
	// child downloads, or deletion) by either the root scan or the
	// per-slug pass.
	assertNoCallWithin(t, stub, time.Second, "renamed-away Arr folder was handed to the download path", func(c pmeCall) bool {
		return c.Query["id"] == "old-sonarr-id" && (c.Path == "/api/folder/list" || c.Path == "/api/folder/delete")
	})
}

// TestCheckFolderSkipsOnlyFolderItemsNamedLikeSlugs is the regression test
// for finding R1-6: the root-scan exclusion applies to folders only - a
// completed FILE named like a tracked Arr slug must be processed, not
// skipped on every poll forever. The name layer applies to slugs without
// a resolved pme folder (the only signal left is the name).
func TestCheckFolderSkipsOnlyFolderItemsNamedLikeSlugs(t *testing.T) {
	stub := newPmeStub(t, false)
	stub.mu.Lock()
	stub.table["main-id"] = []premiumizeme.Item{
		{ID: "arr-folder-id", Name: "sonarr", Type: "folder"},
		{ID: "sonarr-file-id", Name: "sonarr", Type: "file"},
		{ID: "other-file-id", Name: "other", Type: "file"},
	}
	stub.mu.Unlock()

	m := newManagerTestService(t, stub, &config.Config{
		EnableArrSubfolders:   true,
		Arrs:                  []config.ArrConfig{{Name: "sonarr", URL: "http://127.0.0.1:8989", APIKey: "k", Type: config.Sonarr}},
		DownloadsDirectory:    t.TempDir(),
		TransferDirectory:     "arrDownloads",
		SimultaneousDownloads: 5,
		DownloadSpeedLimit:    100,
	}, map[string]string{"sonarr": ""}) // unresolved slug: name-only exclusion

	if !m.checkFolder("main-id", m.config.DownloadsDirectory, rootExclusions{byName: map[string]bool{"sonarr": true}}) {
		t.Fatalf("checkFolder returned false, want true (cap not reached)")
	}

	// The file named like the slug must be processed: its single-file
	// wrapper folder is created in the main downloads folder.
	if !stub.anyCall(func(c pmeCall) bool {
		return c.Path == "/api/folder/create" && c.Query["name"] == "sonarr.folder" && c.Query["parent_id"] == "main-id"
	}) {
		t.Fatalf("file item named like a tracked slug was not processed: calls = %v", stub.calls)
	}

	// The folder container must stay excluded from the root scan's
	// download-and-delete path.
	assertNoCallWithin(t, stub, time.Second, "Arr container folder was handed to the root scan", func(c pmeCall) bool {
		return c.Query["id"] == "arr-folder-id" && (c.Path == "/api/folder/list" || c.Path == "/api/folder/delete")
	})
}

// waitForQueuedPath pops the queue until want is observed or the deadline
// passes; other queued paths are discarded as the loop runs.
func waitForQueuedPath(t *testing.T, q *stringqueue.StringQueue, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for {
			isFile, p := q.PopTopOfQueue()
			if !isFile {
				break
			}
			if p == want {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("path %s was never queued within %s; queue: %v", want, timeout, q.GetQueue())
}

// TestCheckFileReestablishesWatchOnRecreatedSubfolder is the regression
// test for finding R1-20: a configured Arr subfolder that lost its watch
// (pme outage at resolution, delete+recreate, failed AddWatchPath) must be
// re-watched and rescanned when it (re)appears in the blackhole, so files
// dropped into it are queued in default watch mode instead of being
// silently ignored forever.
func TestCheckFileReestablishesWatchOnRecreatedSubfolder(t *testing.T) {
	sonarr := config.ArrConfig{Name: "sonarr", URL: "http://127.0.0.1:8989", APIKey: "k", Type: config.Sonarr}
	bh := t.TempDir()
	sonarrDir := filepath.Join(bh, "sonarr")
	if err := os.MkdirAll(sonarrDir, 0o755); err != nil {
		t.Fatal(err)
	}

	dw := NewDirectoryWatcherService()
	dw.config = &config.Config{
		BlackholeDirectory:  bh,
		EnableArrSubfolders: true,
		Arrs:                []config.ArrConfig{sonarr},
	}
	dw.Queue = stringqueue.NewStringQueue()
	dw.watchDirectory = directory_watcher.NewDirectoryWatcher(bh, true, dw.checkFile, dw.addFileToQueue)
	if err := dw.watchDirectory.Watch(); err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer dw.watchDirectory.Stop()
	if err := dw.watchDirectory.AddWatchPath(sonarrDir); err != nil {
		t.Fatalf("AddWatchPath: %v", err)
	}

	// Control: the harness must deliver a file created in the watched root.
	control := filepath.Join(bh, "control.magnet")
	if err := os.WriteFile(control, []byte("magnet:?xt=urn:btih:control"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitForQueuedPath(t, dw.Queue, control, 5*time.Second)

	// A file in the still-watched subfolder is queued (pre-existing path).
	inside := filepath.Join(sonarrDir, "inside.torrent")
	if err := os.WriteFile(inside, []byte("bt"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitForQueuedPath(t, dw.Queue, inside, 5*time.Second)

	// Delete and recreate the subfolder: its watch dies with the deleted
	// directory and must be re-established by the Create event.
	if err := os.RemoveAll(sonarrDir); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(sonarrDir, 0o755); err != nil {
		t.Fatal(err)
	}
	episode := filepath.Join(sonarrDir, "episode.torrent")
	if err := os.WriteFile(episode, []byte("bt"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitForQueuedPath(t, dw.Queue, episode, 5*time.Second)
}

// TestManagerKeepsTransferDestinationFoldersAcrossRestart is the regression
// test for finding R1-5 (complete): after a daemon restart the arrFolders
// map is empty, so a renamed-away or stale Arr routing folder in the pme
// downloads root can no longer be recognized by name. The identity layer
// closes that gap: the transfer/list endpoint names the destination folder
// of every non-error transfer, so a root-level folder whose own ID is a
// transfer destination is kept, never downloaded and deleted, regardless
// of its current name.
func TestManagerKeepsTransferDestinationFoldersAcrossRestart(t *testing.T) {
	stub := newPmeStub(t, false)
	stub.mu.Lock()
	stub.table["main-id"] = []premiumizeme.Item{
		{ID: "old-sonarr-id", Name: "sonarr", Type: "folder"}, // stale Arr routing folder (renamed away)
		{ID: "show-id", Name: "Show.S01", Type: "folder"},     // transferred content
	}
	stub.table["show-id"] = []premiumizeme.Item{}
	stub.transfers = []premiumizeme.Transfer{
		{ID: "t1", Name: "sonarr feed", Status: "finished", FolderID: "old-sonarr-id"},
		{ID: "t2", Name: "show feed", Status: "finished", FolderID: "main-id"},
	}
	stub.mu.Unlock()

	m := newManagerTestService(t, stub, &config.Config{
		EnableArrSubfolders:   true,
		Arrs:                  []config.ArrConfig{{Name: "sonarr-2", URL: "http://127.0.0.1:8989", APIKey: "k", Type: config.Sonarr}},
		DownloadsDirectory:    t.TempDir(),
		TransferDirectory:     "arrDownloads",
		SimultaneousDownloads: 5,
		DownloadSpeedLimit:    100,
	}, map[string]string{}) // fresh process: nothing resolved yet

	// Simulate Run()'s TaskUpdateTransfersList having populated the cache
	// in the same goroutine before this task ran.
	m.transfers = append([]premiumizeme.Transfer(nil), stub.transfers...)

	m.TaskCheckPremiumizeDownloadsFolder()

	// The stale routing folder (name no longer matches any configured Arr)
	// must be kept by the identity layer: no listing, no deletion.
	assertNoCallWithin(t, stub, time.Second, "transfer-destination folder was handed to the download path", func(c pmeCall) bool {
		return c.Query["id"] == "old-sonarr-id" && (c.Path == "/api/folder/list" || c.Path == "/api/folder/delete")
	})

	// Regular transferred content is unaffected and still processed end to
	// end: listed, downloaded (empty here) and deleted.
	waitForCall(t, stub, 5*time.Second, "transferred content folder was not deleted", func(c pmeCall) bool {
		return c.Path == "/api/folder/delete" && c.Query["id"] == "show-id"
	})
}

// TestManagerTransferDestinationsIgnoredWhenFeatureOff proves a fresh
// feature-off install keeps the exact pre-feature behavior: with the
// feature off and nothing ever tracked (empty arrFolders map), the
// exclusion layers stay off and a folder that is a transfer destination
// is still processed (downloaded and deleted). The layers only turn on
// once the feature has been on and tracked folders exist.
func TestManagerTransferDestinationsIgnoredWhenFeatureOff(t *testing.T) {
	stub := newPmeStub(t, false)
	stub.mu.Lock()
	stub.table["main-id"] = []premiumizeme.Item{{ID: "dest-id", Name: "anything", Type: "folder"}}
	stub.table["dest-id"] = []premiumizeme.Item{}
	stub.transfers = []premiumizeme.Transfer{{ID: "t1", Name: "feed", Status: "finished", FolderID: "dest-id"}}
	stub.mu.Unlock()

	m := newManagerTestService(t, stub, &config.Config{
		EnableArrSubfolders:   false,
		DownloadsDirectory:    t.TempDir(),
		TransferDirectory:     "arrDownloads",
		SimultaneousDownloads: 5,
		DownloadSpeedLimit:    100,
	}, nil)

	m.TaskCheckPremiumizeDownloadsFolder()

	// Feature off: the destination folder is ordinary content and must be
	// downloaded and deleted.
	waitForCall(t, stub, 5*time.Second, "transfer-destination folder was not processed with the feature off", func(c pmeCall) bool {
		return c.Path == "/api/folder/delete" && c.Query["id"] == "dest-id"
	})
}

// TestDownloadDedupScopedByFolderID is the unit regression test for
// finding R1-8: download and cooldown tracking is keyed by premiumize.me
// folder ID plus item name, so same-named items admitted from different
// folders never block, overwrite, or shadow each other.
func TestDownloadDedupScopedByFolderID(t *testing.T) {
	m := TransferManagerService{}.New()

	m.addDownload(&premiumizeme.Item{Name: "Show"}, "f1", true)
	m.addDownload(&premiumizeme.Item{Name: "Show"}, "f2", true)
	if got := m.countDownloads(); got != 2 {
		t.Fatalf("two same-named jobs from different folders: countDownloads() = %d, want 2", got)
	}
	if !m.downloadExists("f1", "Show") || !m.downloadExists("f2", "Show") {
		t.Fatal("downloadExists = false for a folder+name pair that was admitted")
	}
	if m.downloadExists("f3", "Show") {
		t.Fatal("downloadExists = true for a folder that never admitted the item")
	}

	// Removing one job leaves the same-named job in the other folder.
	m.removeDownload("f1", "Show")
	if got := m.countDownloads(); got != 1 {
		t.Fatalf("after removing one job: countDownloads() = %d, want 1", got)
	}
	if !m.downloadExists("f2", "Show") {
		t.Fatal("removing one job leaked into the same-named job in another folder")
	}

	// Cooldown is scoped the same way: a failure in f1 keeps the
	// same-named item in f2 retryable.
	m.markDownloadFailed("f1", "Show")
	if !m.isDownloadInCooldown("f1", "Show") {
		t.Fatal("isDownloadInCooldown = false right after markDownloadFailed")
	}
	if m.isDownloadInCooldown("f2", "Show") {
		t.Fatal("failure in one folder put a same-named item in another folder into cooldown")
	}
}

// TestCheckFolderSameNameInDifferentArrFolders is the integration
// regression test for finding R1-8: two Arr subfolders that each contain
// a folder with the same name must both be admitted and fully processed -
// with bare-name keying the second admission is skipped and its folder is
// never listed or deleted.
func TestCheckFolderSameNameInDifferentArrFolders(t *testing.T) {
	stub := newPmeStub(t, false)
	stub.mu.Lock()
	stub.table["main-id"] = []premiumizeme.Item{
		{ID: "sonarr-id", Name: "sonarr", Type: "folder"},
		{ID: "radarr-id", Name: "radarr", Type: "folder"},
	}
	stub.table["sonarr-id"] = []premiumizeme.Item{{ID: "show-sonarr-id", Name: "Show", Type: "folder"}}
	stub.table["radarr-id"] = []premiumizeme.Item{{ID: "show-radarr-id", Name: "Show", Type: "folder"}}
	stub.table["show-sonarr-id"] = []premiumizeme.Item{}
	stub.table["show-radarr-id"] = []premiumizeme.Item{}
	stub.mu.Unlock()

	m := newManagerTestService(t, stub, &config.Config{
		EnableArrSubfolders: true,
		Arrs: []config.ArrConfig{
			{Name: "sonarr", URL: "http://127.0.0.1:8989", APIKey: "k", Type: config.Sonarr},
			{Name: "radarr", URL: "http://127.0.0.1:7878", APIKey: "k", Type: config.Radarr},
		},
		DownloadsDirectory:    t.TempDir(),
		TransferDirectory:     "arrDownloads",
		SimultaneousDownloads: 5,
		DownloadSpeedLimit:    100,
	}, map[string]string{"sonarr": "sonarr-id", "radarr": "radarr-id"})

	m.TaskCheckPremiumizeDownloadsFolder()

	for _, id := range []string{"show-sonarr-id", "show-radarr-id"} {
		waitForCall(t, stub, 5*time.Second, "same-named folder in an Arr subfolder was not fully processed", func(id string) func(pmeCall) bool {
			return func(c pmeCall) bool {
				return c.Path == "/api/folder/delete" && c.Query["id"] == id
			}
		}(id))
	}
}

// TestProcessUploadUnresolvedTargetRequeues is the regression test for
// findings C-2 and C-8: a file with no resolved target yet (the transfers
// folder never resolved, or the Arr subfolder still unresolved) must be
// re-queued for a later retry instead of being dropped, and an empty main
// folder ID must not be treated as a valid target (which would submit the
// file to the account root). Once the target resolves, the file uploads.
func TestProcessUploadUnresolvedTargetRequeues(t *testing.T) {
	sonarr := config.ArrConfig{Name: "sonarr", URL: "http://127.0.0.1:8989", APIKey: "k", Type: config.Sonarr}

	t.Run("root file with unresolved main folder re-queues instead of dropping", func(t *testing.T) {
		stub := newPmeStub(t, false)
		bh := t.TempDir()
		file := filepath.Join(bh, "root.magnet")
		if err := os.WriteFile(file, []byte("magnet:?xt=urn:btih:a"), 0o600); err != nil {
			t.Fatal(err)
		}

		dw := newArrSubfolderTestService(t, stub, bh, true, []config.ArrConfig{sonarr})
		// pme was down at startup: the transfers folder was never resolved.
		dw.downloadsFolderID = ""
		dw.Queue.Add(file)

		if n := dw.processUploadCycle(); n != 0 {
			t.Fatalf("cycle processed %d files, want 0 (unresolved target re-queued)", n)
		}
		if _, err := os.Stat(file); err != nil {
			t.Fatalf("source file removed although no target resolved: %v", err)
		}
		if got := stub.transferTargets; len(got) != 0 {
			t.Fatalf("transfer submitted without a resolved target: %v", got)
		}
		if q := dw.Queue.GetQueue(); len(q) != 1 || q[0] != file {
			t.Fatalf("file not re-queued for retry: %v", q)
		}

		// The transfers folder resolves (pme recovered).
		dw.mu.Lock()
		dw.downloadsFolderID = "main-id"
		dw.mu.Unlock()
		if n := dw.processUploadCycle(); n != 1 {
			t.Fatalf("recovery cycle processed %d files, want 1", n)
		}
		if got := stub.transferTargets; len(got) != 1 || got[0] != "main-id" {
			t.Fatalf("recovery transfer targets = %v, want [main-id]", got)
		}
		if _, err := os.Stat(file); !os.IsNotExist(err) {
			t.Fatalf("source file not removed after the successful transfer")
		}
	})

	t.Run("subfolder file with unresolved main folder re-queues until the slug resolves", func(t *testing.T) {
		stub := newPmeStub(t, false)
		bh := t.TempDir()
		sub := filepath.Join(bh, "sonarr")
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		file := filepath.Join(sub, "episode.nzb")
		if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}

		dw := newArrSubfolderTestService(t, stub, bh, true, []config.ArrConfig{sonarr})
		dw.downloadsFolderID = ""
		dw.Queue.Add(file)

		if n := dw.processUploadCycle(); n != 0 {
			t.Fatalf("cycle processed %d files, want 0", n)
		}
		// No pme traffic at all: the empty parent ID must not list or
		// create at the account root.
		if stub.anyCall(func(c pmeCall) bool {
			return c.Path == "/api/folder/list" || c.Path == "/api/folder/create"
		}) {
			t.Fatalf("pme traffic with an empty main folder ID: %v", stub.calls)
		}
		if _, err := os.Stat(file); err != nil {
			t.Fatalf("source file removed although no target resolved: %v", err)
		}

		dw.mu.Lock()
		dw.downloadsFolderID = "main-id"
		dw.mu.Unlock()
		if n := dw.processUploadCycle(); n != 1 {
			t.Fatalf("recovery cycle processed %d files, want 1", n)
		}
		if got := stub.transferTargets; len(got) != 1 || got[0] == "" || got[0] == "main-id" {
			t.Fatalf("recovery transfer targets = %v, want the resolved Arr subfolder", got)
		}
		if len(stub.createdFolders) != 1 || stub.createdFolders[0] != "sonarr" {
			t.Fatalf("pme folders created = %v, want [sonarr]", stub.createdFolders)
		}
	})
}

// TestResolveArrFoldersEmptyMainFolderIDMakesNoPmeTraffic is the
// regression test for finding C-9: with the transfers folder unresolved
// (empty main folder ID), the bulk resolve must not list or create at the
// account root; it keeps the previous mapping (or an empty-ID placeholder)
// and retries on the next resolve.
func TestResolveArrFoldersEmptyMainFolderIDMakesNoPmeTraffic(t *testing.T) {
	sonarr := config.ArrConfig{Name: "sonarr", URL: "http://127.0.0.1:8989", APIKey: "k", Type: config.Sonarr}

	stub := newPmeStub(t, false)
	bh := t.TempDir()
	dw := newArrSubfolderTestService(t, stub, bh, true, []config.ArrConfig{sonarr})
	dw.downloadsFolderID = ""

	dw.resolveArrFolders()

	if stub.anyCall(func(c pmeCall) bool {
		return c.Path == "/api/folder/list" || c.Path == "/api/folder/create"
	}) {
		t.Fatalf("pme traffic with an empty main folder ID: %v", stub.calls)
	}
	dw.mu.RLock()
	id, present := dw.arrFolders["sonarr"]
	dw.mu.RUnlock()
	if !present || id != "" {
		t.Fatalf("arrFolders[sonarr] = %q, present=%v; want an empty-ID placeholder", id, present)
	}
	if _, err := os.Stat(filepath.Join(bh, "sonarr")); err != nil {
		t.Fatalf("blackhole subfolder not created: %v", err)
	}
}

// TestResolveSingleArrFolderConfigChangeMidFlight is the regression test
// for finding C-5: the on-demand resolve checks the feature flag and the
// slug's presence under a read lock, then does the pme round trip, then
// writes the map under the write lock. A config swap in that window
// (feature toggled off, slug removed) must be seen by a re-validation
// under the write lock, or the stale write resurrects an entry a
// concurrent bulk resolve just dropped - and its watch leaks.
func TestResolveSingleArrFolderConfigChangeMidFlight(t *testing.T) {
	sonarr := config.ArrConfig{Name: "sonarr", URL: "http://127.0.0.1:8989", APIKey: "k", Type: config.Sonarr}
	bh := t.TempDir()
	dw := NewDirectoryWatcherService()
	cfg := &config.Config{
		BlackholeDirectory:  bh,
		TransferDirectory:   "arrDownloads",
		EnableArrSubfolders: true,
		Arrs:                []config.ArrConfig{sonarr},
	}

	var mu sync.Mutex
	created := false
	gate := make(chan struct{})
	gated := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/folder/list":
			close(gated)
			<-gate
			writeJSON(w, map[string]any{"status": "success", "content": []premiumizeme.Item{}})
		case r.URL.Path == "/api/folder/create":
			mu.Lock()
			created = true
			mu.Unlock()
			writeJSON(w, map[string]any{"status": "success", "id": "created-1"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := premiumizeme.NewPremiumizemeClient("test-key")
	client.APIBaseURL = server.URL + "/api/"
	client.HTTPClient = server.Client()
	dw.Init(&client, cfg)
	dw.Queue = stringqueue.NewStringQueue()
	dw.downloadsFolderID = "main-id"

	done := make(chan struct{})
	go func() {
		dw.resolveSingleArrFolder("sonarr", "main-id")
		close(done)
	}()

	<-gated // the pme round trip is in flight
	// Toggle the feature off (and remove the slug) while it is in flight.
	cfg.EnableArrSubfolders = false
	cfg.Arrs = nil
	close(gate)
	<-done

	mu.Lock()
	roundTripCompleted := created
	mu.Unlock()
	if !roundTripCompleted {
		t.Fatal("the pme round trip did not complete; the test cannot prove the write was skipped")
	}
	dw.mu.RLock()
	id, present := dw.arrFolders["sonarr"]
	dw.mu.RUnlock()
	if present {
		t.Fatalf("arrFolders[sonarr] = %q; the stale resolve must not resurrect an entry the config change dropped", id)
	}
}

// TestResolveArrFoldersPmeFailureTracksUnresolvedSlug is the regression
// test for finding C-6: a slug whose pme resolution fails is tracked with
// an empty-ID entry, so a watch registered for its local subfolder is
// removed by the same map-keyed loops as every other watch instead of
// leaking when the blackhole changes, the feature toggles off, or the
// slug is removed.
func TestResolveArrFoldersPmeFailureTracksUnresolvedSlug(t *testing.T) {
	sonarr := config.ArrConfig{Name: "sonarr", URL: "http://127.0.0.1:8989", APIKey: "k", Type: config.Sonarr}

	stub := newPmeStub(t, true)
	bh := t.TempDir()
	dw := newArrSubfolderTestService(t, stub, bh, true, []config.ArrConfig{sonarr})

	dw.resolveArrFolders()

	dw.mu.RLock()
	id, present := dw.arrFolders["sonarr"]
	dw.mu.RUnlock()
	if !present {
		t.Fatal("a slug that failed resolution is not tracked: its subfolder watch can never be removed")
	}
	if id != "" {
		t.Fatalf("arrFolders[sonarr] = %q, want an empty-ID placeholder", id)
	}
	if _, err := os.Stat(filepath.Join(bh, "sonarr")); err != nil {
		t.Fatalf("blackhole subfolder not created during the pme outage: %v", err)
	}
}

// TestResolveArrFoldersConcurrentRunsSerialize is the regression test for
// finding C-7: the startup resolve and a config-callback resolve can run
// concurrently, each ending with a full map replacement; serialized runs
// must end in a map consistent with the last run's config, not a torn
// commit where one run's replacement erases the other run's entry.
func TestResolveArrFoldersConcurrentRunsSerialize(t *testing.T) {
	sonarr := config.ArrConfig{Name: "sonarr", URL: "http://127.0.0.1:8989", APIKey: "k", Type: config.Sonarr}
	radarr := config.ArrConfig{Name: "radarr", URL: "http://127.0.0.1:7878", APIKey: "k", Type: config.Radarr}
	bh := t.TempDir()

	// A pme whose create is slow enough that the second resolve's config
	// change lands inside the first run's network window.
	var tableMu sync.Mutex
	table := map[string][]premiumizeme.Item{"main-id": {}}
	nextID := 0
	firstListed := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/folder/list":
			id := r.URL.Query().Get("id")
			tableMu.Lock()
			items := table[id]
			tableMu.Unlock()
			select {
			case <-firstListed:
			default:
				close(firstListed)
			}
			writeJSON(w, map[string]any{"status": "success", "content": items})
		case r.URL.Path == "/api/folder/create":
			name := r.URL.Query().Get("name")
			parent := r.URL.Query().Get("parent_id")
			time.Sleep(150 * time.Millisecond)
			tableMu.Lock()
			nextID++
			id := fmt.Sprintf("created-%d", nextID)
			table[parent] = append(table[parent], premiumizeme.Item{ID: id, Name: name, Type: "folder"})
			tableMu.Unlock()
			writeJSON(w, map[string]any{"status": "success", "id": id})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := premiumizeme.NewPremiumizemeClient("test-key")
	client.APIBaseURL = server.URL + "/api/"
	client.HTTPClient = server.Client()

	cfg := &config.Config{
		BlackholeDirectory:  bh,
		TransferDirectory:   "arrDownloads",
		EnableArrSubfolders: true,
		Arrs:                []config.ArrConfig{sonarr},
	}
	dw := NewDirectoryWatcherService()
	dw.Init(&client, cfg)
	dw.Queue = stringqueue.NewStringQueue()
	dw.downloadsFolderID = "main-id"

	aDone := make(chan struct{})
	go func() {
		dw.resolveArrFolders()
		close(aDone)
	}()

	<-firstListed // the first resolve is in its pme window
	// Second resolve (config-callback shape) while the first is in flight:
	// the config now carries a different Arr set.
	cfg.Arrs = []config.ArrConfig{radarr}
	dw.resolveArrFolders()
	<-aDone

	dw.mu.RLock()
	sonarrID, sonarrOK := dw.arrFolders["sonarr"]
	radarrID, radarrOK := dw.arrFolders["radarr"]
	total := len(dw.arrFolders)
	dw.mu.RUnlock()
	// The config at the end is [radarr], and both runs serialize
	// snapshot→pme→commit under the same lock, so the final map must be
	// exactly the newer run's snapshot result: radarr resolved; no sonarr
	// (an older snapshot's commit landing after the newer run's is the
	// C-7 torn commit, leaving the map inconsistent with the config);
	// no mixture of the two snapshots in one map.
	if !radarrOK || radarrID == "" {
		t.Fatalf("final map missing the last run's resolved slug: sonarr=%q(%v) radarr=%q(%v)", sonarrID, sonarrOK, radarrID, radarrOK)
	}
	if sonarrOK {
		t.Fatalf("an older snapshot's commit landed after the newer run's: final map = stale state (the C-7 torn commit): sonarr=%q radarr=%q", sonarrID, radarrID)
	}
	if total != 1 {
		t.Fatalf("final map mixes slugs from two runs' snapshots: %d entries", total)
	}
}

// TestWatcherHandleRaceBetweenStartAndCallback is the regression test for
// finding C-12: the Start() goroutine and the web config callback can both
// create, replace, or nil the watcher handle; every read and write of the
// handle must take the service lock, or the pair is torn (and -race flags
// the unsynchronized pointer access).
func TestWatcherHandleRaceBetweenStartAndCallback(t *testing.T) {
	bh1 := t.TempDir()
	bh2 := t.TempDir()
	sonarr := config.ArrConfig{Name: "sonarr", URL: "http://127.0.0.1:8989", APIKey: "k", Type: config.Sonarr}

	dw := NewDirectoryWatcherService()
	// The callback is installed through the product load path so the
	// unexported appCallback/altConfigLocation fields are set the way
	// cmd/premiumizearrd sets them.
	cfg, err := config.LoadOrCreateConfig(t.TempDir(), func(oldConfig, newConfig config.Config) {
		dw.ConfigUpdatedCallback(oldConfig, newConfig)
	})
	if err != nil {
		t.Fatalf("LoadOrCreateConfig: %v", err)
	}
	cfg.BlackholeDirectory = bh1
	cfg.DownloadsDirectory = t.TempDir()
	cfg.TransferDirectory = "arrDownloads"
	cfg.EnableArrSubfolders = true
	cfg.Arrs = []config.ArrConfig{sonarr}

	stub := newPmeStub(t, false)
	dw.Init(newPmeTestClient(t, stub), &cfg)
	dw.Queue = stringqueue.NewStringQueue()
	dw.downloadsFolderID = "main-id"

	start := make(chan struct{})
	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Web-save shape: whole-struct swaps alternating the blackhole
	// target, which is what drives the callback's watcher restart.
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			newCfg := config.Config{
				PremiumizemeAPIKey:                      "test-key",
				BlackholeDirectory:                      bh1,
				DownloadsDirectory:                      cfg.DownloadsDirectory,
				TransferDirectory:                       "arrDownloads",
				PollBlackholeIntervalMinutes:            10,
				SimultaneousDownloads:                   5,
				DownloadSpeedLimit:                      100,
				ArrHistoryUpdateIntervalSeconds:         20,
				ErroredTransferDeleteGracePeriodSeconds: 300,
			}
			if i%2 == 0 {
				newCfg.EnableArrSubfolders = true
				newCfg.Arrs = []config.ArrConfig{sonarr}
			} else {
				newCfg.EnableArrSubfolders = false
				newCfg.Arrs = []config.ArrConfig{}
				newCfg.BlackholeDirectory = bh2
			}
			cfg.UpdateConfig(newCfg)
		}
	}()

	// Start() shape: the one-shot watcher setup racing the callback's
	// handle replacements.
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		dw.Start()
	}()

	close(start)
	time.Sleep(2000 * time.Millisecond)
	close(stop)
	wg.Wait()
	// The reconfiguration worker is process-wide and asynchronous: drain
	// the saves this test enqueued before the temp dirs go.
	config.WaitReconfigIdle()
	// Let callback-spawned directory scans drain before the temp dirs go.
	time.Sleep(200 * time.Millisecond)
}

// TestManagerToggleOffKeepsArrContainersExcluded is the regression test
// for finding C-13: toggling the feature off must not clear the tracked
// folder IDs - the pme containers this client created stay excluded from
// the root scan's download-and-delete path (with their unprocessed
// remainder still downloaded into the local subfolder), otherwise the next
// scan downloads the container's content into the main downloads directory
// and deletes the container from premiumize.me.
func TestManagerToggleOffKeepsArrContainersExcluded(t *testing.T) {
	sonarr := config.ArrConfig{Name: "sonarr", URL: "http://127.0.0.1:8989", APIKey: "k", Type: config.Sonarr}

	stub := newPmeStub(t, false)
	stub.mu.Lock()
	stub.table["main-id"] = []premiumizeme.Item{
		{ID: "sonarr-id", Name: "sonarr", Type: "folder"}, // container this client created
		{ID: "show-id", Name: "Show.S01", Type: "folder"}, // transferred content
	}
	stub.table["sonarr-id"] = []premiumizeme.Item{}
	stub.table["show-id"] = []premiumizeme.Item{}
	stub.mu.Unlock()

	cfg := &config.Config{
		EnableArrSubfolders:   true,
		Arrs:                  []config.ArrConfig{sonarr},
		DownloadsDirectory:    t.TempDir(),
		TransferDirectory:     "arrDownloads",
		SimultaneousDownloads: 5,
		DownloadSpeedLimit:    100,
	}
	m := newManagerTestService(t, stub, cfg, nil)

	m.resolveArrFolders()
	m.arrFoldersMutex.Lock()
	containerID := m.arrFolders["sonarr"]
	m.arrFoldersMutex.Unlock()
	if containerID == "" {
		t.Fatalf("the Arr container was not resolved: %v", m.arrFolders)
	}

	// The user toggles the feature off (web save -> callback -> resolve).
	newCfg := *cfg
	newCfg.EnableArrSubfolders = false
	m.ConfigUpdatedCallback(*cfg, newCfg)

	m.arrFoldersMutex.Lock()
	kept, present := m.arrFolders["sonarr"]
	m.arrFoldersMutex.Unlock()
	if !present || kept == "" {
		t.Fatalf("toggle-off cleared the tracked container: present=%v id=%q", present, kept)
	}

	m.TaskCheckPremiumizeDownloadsFolder()

	// The retained container must not be handed to the download path
	// (listing, child downloads, or deletion).
	assertNoCallWithin(t, stub, time.Second, "retained Arr container was handed to the download path with the feature off", func(c pmeCall) bool {
		return c.Query["id"] == "sonarr-id" && c.Path == "/api/folder/delete"
	})

	// Regular transferred content is unaffected and still processed end
	// to end: listed, downloaded (empty here) and deleted.
	waitForCall(t, stub, 5*time.Second, "content folder was not processed with the feature off", func(c pmeCall) bool {
		return c.Path == "/api/folder/delete" && c.Query["id"] == "show-id"
	})
}

// TestCheckFolderContentFolderNamedLikeResolvedSlugIsProcessed is the
// regression test for finding C-10: a slug whose pme folder IS resolved
// is excluded by folder ID, not by name - so a legitimate content folder
// that happens to be named like the slug is still processed by the root
// scan.
func TestCheckFolderContentFolderNamedLikeResolvedSlugIsProcessed(t *testing.T) {
	sonarr := config.ArrConfig{Name: "sonarr", URL: "http://127.0.0.1:8989", APIKey: "k", Type: config.Sonarr}

	stub := newPmeStub(t, false)
	stub.mu.Lock()
	stub.table["main-id"] = []premiumizeme.Item{
		{ID: "sonarr-id", Name: "sonarr-container", Type: "folder"}, // resolved container
		{ID: "content-sonarr-id", Name: "sonarr", Type: "folder"},   // content named like the slug
	}
	stub.table["sonarr-id"] = []premiumizeme.Item{}
	stub.table["content-sonarr-id"] = []premiumizeme.Item{}
	stub.mu.Unlock()

	m := newManagerTestService(t, stub, &config.Config{
		EnableArrSubfolders:   true,
		Arrs:                  []config.ArrConfig{sonarr},
		DownloadsDirectory:    t.TempDir(),
		TransferDirectory:     "arrDownloads",
		SimultaneousDownloads: 5,
		DownloadSpeedLimit:    100,
	}, map[string]string{"sonarr": "sonarr-id"})

	m.TaskCheckPremiumizeDownloadsFolder()

	// The resolved container is excluded by its ID: never admitted, never
	// deleted (the per-slug pass lists it to process its content, but the
	// stubbed container is empty, so it is never admitted either).
	assertNoCallWithin(t, stub, time.Second, "resolved Arr container was deleted by the root scan", func(c pmeCall) bool {
		return c.Query["id"] == "sonarr-id" && c.Path == "/api/folder/delete"
	})

	// A content folder named like the resolved slug is still processed.
	waitForCall(t, stub, 5*time.Second, "content folder named like a resolved slug was not processed", func(c pmeCall) bool {
		return c.Path == "/api/folder/delete" && c.Query["id"] == "content-sonarr-id"
	})
}

// TestCheckFolderTrackedRenamedFolderKeptByID is the regression test for
// finding C-15: a tracked pme folder that the user renames on pme keeps
// its ID; the ID-based exclusion layer recognizes it even though the name
// no longer matches any tracked slug and no transfer in the cache points
// at it anymore.
func TestCheckFolderTrackedRenamedFolderKeptByID(t *testing.T) {
	sonarr := config.ArrConfig{Name: "sonarr", URL: "http://127.0.0.1:8989", APIKey: "k", Type: config.Sonarr}

	stub := newPmeStub(t, false)
	stub.mu.Lock()
	stub.table["main-id"] = []premiumizeme.Item{
		{ID: "sonarr-id", Name: "renamed-by-user", Type: "folder"}, // tracked container, renamed on pme
	}
	stub.table["sonarr-id"] = []premiumizeme.Item{}
	stub.mu.Unlock()

	m := newManagerTestService(t, stub, &config.Config{
		EnableArrSubfolders:   true,
		Arrs:                  []config.ArrConfig{sonarr},
		DownloadsDirectory:    t.TempDir(),
		TransferDirectory:     "arrDownloads",
		SimultaneousDownloads: 5,
		DownloadSpeedLimit:    100,
	}, map[string]string{"sonarr": "sonarr-id"})

	m.TaskCheckPremiumizeDownloadsFolder()

	// The renamed tracked folder must not be downloaded and deleted:
	// only its ID can still recognize it.
	assertNoCallWithin(t, stub, time.Second, "tracked folder renamed on pme was deleted by the root scan", func(c pmeCall) bool {
		return c.Query["id"] == "sonarr-id" && c.Path == "/api/folder/delete"
	})
}

// TestNestedTransferDestinationFolderKeptWithParent is the regression
// test for finding C-14: a client-managed routing folder NESTED inside a
// transfer content folder must not be downloaded and deleted with its
// parent - the parent job fails instead, keeping the whole subtree on
// premiumize.me.
func TestNestedTransferDestinationFolderKeptWithParent(t *testing.T) {
	sonarr := config.ArrConfig{Name: "sonarr", URL: "http://127.0.0.1:8989", APIKey: "k", Type: config.Sonarr}

	stub := newPmeStub(t, false)
	stub.mu.Lock()
	stub.table["main-id"] = []premiumizeme.Item{
		{ID: "show-id", Name: "Show.S01", Type: "folder"},
	}
	stub.table["show-id"] = []premiumizeme.Item{
		{ID: "dest-nested-id", Name: "nested-arr-folder", Type: "folder"},
	}
	stub.table["dest-nested-id"] = []premiumizeme.Item{}
	stub.transfers = []premiumizeme.Transfer{
		{ID: "t1", Name: "feed", Status: "finished", FolderID: "dest-nested-id"},
	}
	stub.mu.Unlock()

	m := newManagerTestService(t, stub, &config.Config{
		EnableArrSubfolders:   true,
		Arrs:                  []config.ArrConfig{sonarr},
		DownloadsDirectory:    t.TempDir(),
		TransferDirectory:     "arrDownloads",
		SimultaneousDownloads: 5,
		DownloadSpeedLimit:    100,
	}, nil)

	// Simulate Run()'s TaskUpdateTransfersList having populated the cache
	// in the same goroutine before this task ran.
	m.transfers = append([]premiumizeme.Transfer(nil), stub.transfers...)

	m.TaskCheckPremiumizeDownloadsFolder()

	// The nested routing folder is kept, and the parent job fails instead
	// of deleting the whole subtree.
	assertNoCallWithin(t, stub, time.Second, "nested routing folder was deleted with its parent", func(c pmeCall) bool {
		return c.Path == "/api/folder/delete" && c.Query["id"] == "dest-nested-id"
	})
	assertNoCallWithin(t, stub, time.Second, "parent of a kept nested routing folder was deleted", func(c pmeCall) bool {
		return c.Path == "/api/folder/delete" && c.Query["id"] == "show-id"
	})
}

// TestUpdateConfigRaceAgainstManagerAndWebReaders is the regression test
// for finding C-1: the transfer manager's poll-loop config reads and the
// web BlackholeHandler's BlackholeDirectory read must take the registered
// config swap lock's read lock, like the watcher's readers already do.
// Run with -race; the pre-fix tree reports data races on the unlocked
// manager/web config field reads against the locked in-place swap.
func TestUpdateConfigRaceAgainstManagerAndWebReaders(t *testing.T) {
	bh1 := t.TempDir()
	bh2 := t.TempDir()
	sonarr := config.ArrConfig{Name: "sonarr", URL: "http://127.0.0.1:8989", APIKey: "k", Type: config.Sonarr}
	radarr := config.ArrConfig{Name: "radarr", URL: "http://127.0.0.1:7878", APIKey: "k", Type: config.Radarr}

	cfg, err := config.LoadOrCreateConfig(t.TempDir(), func(oldConfig, newConfig config.Config) {})
	if err != nil {
		t.Fatalf("LoadOrCreateConfig: %v", err)
	}
	cfg.BlackholeDirectory = bh1
	cfg.DownloadsDirectory = t.TempDir()
	cfg.TransferDirectory = "arrDownloads"
	cfg.EnableArrSubfolders = true
	cfg.Arrs = []config.ArrConfig{sonarr}
	var mu sync.RWMutex
	config.SetUpdateMu(&mu)

	stub := newPmeStub(t, false)
	m := TransferManagerService{}.New()
	m.premiumizemeClient = newPmeTestClient(t, stub)
	m.config = &cfg
	m.downloadsFolderID = "main-id"
	m.transfers = []premiumizeme.Transfer{{ID: "t1", Name: "feed", Status: "finished", FolderID: "dest-x"}}

	// The web reader shape: BlackholeHandler reads BlackholeDirectory on
	// every request.
	dw := NewDirectoryWatcherService()
	dw.config = &cfg
	dw.Queue = stringqueue.NewStringQueue()
	dw.Queue.Add(filepath.Join(bh1, "a.magnet"))
	web := &WebServerService{directoryWatcherService: &dw, config: &cfg}

	start := make(chan struct{})
	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Web-save shape: whole-struct swap through UpdateConfig.
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			newCfg := config.Config{
				PremiumizemeAPIKey:                      "test-key",
				BlackholeDirectory:                      bh1,
				DownloadsDirectory:                      cfg.DownloadsDirectory,
				TransferDirectory:                       "arrDownloads",
				PollBlackholeIntervalMinutes:            10,
				SimultaneousDownloads:                   5,
				DownloadSpeedLimit:                      100,
				ArrHistoryUpdateIntervalSeconds:         20,
				ErroredTransferDeleteGracePeriodSeconds: 300,
			}
			// Vary slice length, toggle and blackhole target on every
			// swap so the torn-header windows are exercised, like a user
			// editing the config in the web UI.
			if i%2 == 0 {
				newCfg.EnableArrSubfolders = true
				newCfg.Arrs = []config.ArrConfig{sonarr, radarr}
			} else {
				newCfg.EnableArrSubfolders = false
				newCfg.Arrs = []config.ArrConfig{sonarr}
				newCfg.BlackholeDirectory = bh2
			}
			m.config.UpdateConfig(newCfg)
		}
	}()

	// Manager poll-loop shape: the downloads-folder check reads
	// EnableArrSubfolders, DownloadsDirectory and SimultaneousDownloads
	// and the tracked map on every cycle.
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for {
			select {
			case <-stop:
				return
			default:
			}
			m.TaskCheckPremiumizeDownloadsFolder()
		}
	}()

	// Web handler shape.
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for {
			select {
			case <-stop:
				return
			default:
			}
			rec := httptest.NewRecorder()
			web.BlackholeHandler(rec, httptest.NewRequest(http.MethodGet, "/blackhole", nil))
		}
	}()

	close(start)
	time.Sleep(2000 * time.Millisecond)
	close(stop)
	wg.Wait()
	// The reconfiguration worker is process-wide and asynchronous: drain
	// the saves this test enqueued.
	config.WaitReconfigIdle()
}

// TestDownloadFolderRecursivelyProgressReadsRaceFree is the regression
// test for finding S-13: the downloadList progress-counter read inside
// downloadFolderRecursively must run under the download list lock, and a
// -race test must actually execute that read against concurrent locked
// writes from other top-level download goroutines - otherwise deleting
// the lock pair would stay invisible to the race gate.
func TestDownloadFolderRecursivelyProgressReadsRaceFree(t *testing.T) {
	// A pme stub whose item details point at a download server that
	// always fails, so every file child takes the post-read error path.
	downloadSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer downloadSrv.Close()

	var tableMu sync.Mutex
	table := map[string][]premiumizeme.Item{
		"folder-a": {{ID: "file-a1", Name: "a1.bin", Type: "file"}},
		"folder-b": {{ID: "file-b1", Name: "b1.bin", Type: "file"}},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/folder/list":
			id := r.URL.Query().Get("id")
			tableMu.Lock()
			items := table[id]
			tableMu.Unlock()
			writeJSON(w, map[string]any{"status": "success", "content": items})
		case r.URL.Path == "/api/item/details":
			writeJSON(w, map[string]any{"status": "success", "type": "file", "link": downloadSrv.URL + "/f"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := premiumizeme.NewPremiumizemeClient("test-key")
	client.APIBaseURL = server.URL + "/api/"
	client.HTTPClient = server.Client()

	downloads := t.TempDir()

	itemA := premiumizeme.Item{ID: "folder-a", Name: "A", Type: "folder"}
	itemB := premiumizeme.Item{ID: "folder-b", Name: "B", Type: "folder"}

	for i := 0; i < 4; i++ {
		// A fresh manager per round: a failed child stays in the
		// downloadList/cooldown state, so a second run of the same folder
		// on the same manager would be deduplicated away and never reach
		// the progress read. Two goroutines share one manager's
		// downloadList, so A's locked progress read runs against B's
		// locked addDownload/removeDownload/markDownloadFailed writes.
		m := TransferManagerService{}.New()
		m.premiumizemeClient = &client
		m.config = &config.Config{EnableTlsCheck: true, DownloadSpeedLimit: 0}

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			if err := m.downloadFolderRecursively(itemA, downloads, rootExclusions{}); err == nil {
				t.Error("downloadFolderRecursively returned nil for a failing file child")
			}
		}()
		go func() {
			defer wg.Done()
			if err := m.downloadFolderRecursively(itemB, downloads, rootExclusions{}); err == nil {
				t.Error("downloadFolderRecursively returned nil for a failing file child")
			}
		}()
		wg.Wait()
	}
}

// TestDownloadFolderRecursivelyCreatesMissingParents is the regression
// test for finding S-17: the MkdirAll save-path change must be covered -
// a download into a directory whose parents do not exist (deleted at
// runtime, or left missing after a failed MkdirAll at resolution) creates
// the chain instead of failing the job into the 30-minute cooldown loop.
func TestDownloadFolderRecursivelyCreatesMissingParents(t *testing.T) {
	stub := newPmeStub(t, false)
	stub.mu.Lock()
	stub.table["show-id"] = []premiumizeme.Item{}
	stub.mu.Unlock()

	m := newManagerTestService(t, stub, &config.Config{
		EnableArrSubfolders:   true,
		DownloadsDirectory:    t.TempDir(),
		TransferDirectory:     "arrDownloads",
		SimultaneousDownloads: 5,
		DownloadSpeedLimit:    100,
	}, nil)

	missing := filepath.Join(t.TempDir(), "does", "not", "exist")
	err := m.downloadFolderRecursively(premiumizeme.Item{ID: "show-id", Name: "Show", Type: "folder"}, missing, rootExclusions{})
	if err != nil {
		t.Fatalf("download into a missing parent chain failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(missing, "Show")); err != nil {
		t.Fatalf("save path was not created: %v", err)
	}
}

// TestConfigChangeAppliesNewestCommittedState is the regression test for
// findings C-4 (a)/(b) and S-28: a save whose reconfiguration is blocked on
// premiumize.me must not block the save (the endpoint returns once the
// validated config is safely stored), and a slow, stale reconfiguration
// that finally completes after a NEWER save has committed must apply the
// newest committed state - it must never move the live watcher (or, in the
// pre-async-persistence shape, the persisted file) back to the older
// blackhole of the save it was called for.
func TestConfigChangeAppliesNewestCommittedState(t *testing.T) {
	bh1 := t.TempDir()
	bh2 := t.TempDir()
	bh3 := t.TempDir()

	sonarr := config.ArrConfig{Name: "sonarr", URL: "http://127.0.0.1:8989", APIKey: "k", Type: config.Sonarr}

	dw := NewDirectoryWatcherService()
	loc := t.TempDir()
	// Seed the initial state the way a local install's config.yaml would
	// carry it: the feature on, one configured Arr, the blackhole at bh1.
	seed := config.Config{
		PremiumizemeAPIKey:                      "key",
		BlackholeDirectory:                      bh1,
		DownloadsDirectory:                      t.TempDir(),
		TransferDirectory:                       "arrDownloads",
		BindIP:                                  "0.0.0.0",
		BindPort:                                "8182",
		SimultaneousDownloads:                   5,
		DownloadSpeedLimit:                      100,
		ArrHistoryUpdateIntervalSeconds:         20,
		ErroredTransferDeleteGracePeriodSeconds: 300,
		EnableArrSubfolders:                     true,
		Arrs:                                    []config.ArrConfig{sonarr},
	}
	seedData, err := yaml.Marshal(seed)
	if err != nil {
		t.Fatalf("marshal seed config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(loc, "config.yaml"), seedData, 0o600); err != nil {
		t.Fatalf("write seed config: %v", err)
	}
	// The service callback is installed through the product load path so
	// the unexported appCallback/altConfigLocation fields are set the way
	// cmd/premiumizearrd sets them.
	cfg, err := config.LoadOrCreateConfig(loc, func(oldConfig, newConfig config.Config) {
		dw.ConfigUpdatedCallback(oldConfig, newConfig)
	})
	if err != nil {
		t.Fatalf("LoadOrCreateConfig: %v", err)
	}
	if cfg.BlackholeDirectory != bh1 {
		t.Fatalf("seeded config BlackholeDirectory = %q, want %q", cfg.BlackholeDirectory, bh1)
	}

	stub := newPmeStub(t, false)
	dw.Init(newPmeTestClient(t, stub), &cfg)
	dw.Queue = stringqueue.NewStringQueue()
	dw.downloadsFolderID = "main-id"

	// The startup watcher: bh1 as root plus the resolved, watched sonarr
	// subfolder - the state a running daemon has before the change.
	wd := directory_watcher.NewDirectoryWatcher(bh1, true, dw.checkFile, dw.addFileToQueue)
	if err := wd.Watch(); err != nil {
		t.Fatalf("Watch: %v", err)
	}
	dw.mu.Lock()
	dw.watchDirectory = wd
	dw.mu.Unlock()
	t.Cleanup(func() { _ = wd.Stop() })

	dw.resolveArrFolders()

	// Gate every pme folder/list: the reconfiguration triggered by the
	// first save will block inside it.
	gate := make(chan struct{})
	stub.setGateList(gate)

	// Save A while pme is blocked. The save must return as soon as the
	// new state is stored, not when the blocked reconfiguration
	// completes: a synchronous reconfiguration (the pre-C-4 shape) would
	// hold this call until the test's final gate release, well past the
	// bound below.
	newA := cfg
	newA.BlackholeDirectory = bh2
	start := time.Now()
	if err := cfg.UpdateConfig(newA); err != nil {
		t.Fatalf("UpdateConfig A: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("UpdateConfig waited %s on the blocked reconfiguration", elapsed)
	}

	// Save B on top of A while A's reconfiguration is still in flight.
	newB := cfg
	newB.BlackholeDirectory = bh3
	if err := cfg.UpdateConfig(newB); err != nil {
		t.Fatalf("UpdateConfig B: %v", err)
	}

	// The newest committed state is already safely stored on disk, even
	// though A's reconfiguration has not completed.
	data, err := os.ReadFile(filepath.Join(loc, "config.yaml"))
	if err != nil {
		t.Fatalf("read persisted config: %v", err)
	}
	var persisted config.Config
	if err := yaml.Unmarshal(data, &persisted); err != nil {
		t.Fatalf("persisted config does not parse: %v", err)
	}
	if persisted.BlackholeDirectory != bh3 {
		t.Fatalf("persisted BlackholeDirectory = %q, want the newest committed state %q (save A's reconfiguration is still in flight)", persisted.BlackholeDirectory, bh3)
	}

	// Release pme and let the serialized worker drain both
	// reconfigurations: A's (queued first, blocked) completes after B's
	// state has already been committed.
	close(gate)
	config.WaitReconfigIdle()

	// The final watcher path is the newest committed state, never the
	// stale location of the reconfiguration that was queued first:
	// A's callback applied the live state it re-read at run time, on top
	// of B's commit.
	dw.mu.RLock()
	finalWD := dw.watchDirectory
	dw.mu.RUnlock()
	if finalWD == nil {
		t.Fatal("watcher handle is nil after the reconfiguration drained")
	}
	if finalWD.Path != bh3 {
		t.Fatalf("final watcher path = %q, want %q (the newest committed blackhole)", finalWD.Path, bh3)
	}
	dw.mu.RLock()
	arrID, arrOK := dw.arrFolders["sonarr"]
	dw.mu.RUnlock()
	if !arrOK || arrID == "" {
		t.Fatalf("arrFolders[sonarr] = %q (present=%v), want the resolved pme ID", arrID, arrOK)
	}

	// The sonarr subfolder under the NEW blackhole is watched: a file
	// dropped into it is queued.
	newFile := filepath.Join(bh3, "sonarr", "queued.magnet")
	if err := os.WriteFile(newFile, []byte("magnet:?xt=urn:btih:queued"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitForQueuedPath(t, dw.Queue, newFile, 5*time.Second)

	// The stale blackhole is no longer watched: a file dropped into its
	// sonarr subfolder stays out of the queue.
	oldFile := filepath.Join(bh2, "sonarr", "stale.magnet")
	if err := os.MkdirAll(filepath.Dir(oldFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(oldFile, []byte("magnet:?xt=urn:btih:stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	for _, queued := range dw.Queue.GetQueue() {
		if queued == oldFile {
			t.Fatalf("file in the stale blackhole %s was queued: the watcher still watches the older location", oldFile)
		}
	}

	// The stale reconfiguration must not have rolled the stored state
	// back either.
	data, err = os.ReadFile(filepath.Join(loc, "config.yaml"))
	if err != nil {
		t.Fatalf("read persisted config: %v", err)
	}
	if err := yaml.Unmarshal(data, &persisted); err != nil {
		t.Fatalf("persisted config does not parse: %v", err)
	}
	if persisted.BlackholeDirectory != bh3 {
		t.Fatalf("persisted BlackholeDirectory = %q after the reconfiguration drained, want %q", persisted.BlackholeDirectory, bh3)
	}
}

// TestManagerRetryHealsTransientlyFailedArrResolution is the regression
// test for finding S-22: a tracked Arr whose pme subfolder resolution
// failed (premiumize.me down) must be re-resolved during the normal poll
// loop - no config edit, restart, or dedicated re-resolution trigger -
// and a slug healed in a poll is excluded by ID and processed by the
// per-slug pass in that same poll.
func TestManagerRetryHealsTransientlyFailedArrResolution(t *testing.T) {
	sonarr := config.ArrConfig{Name: "sonarr", URL: "http://127.0.0.1:8989", APIKey: "k", Type: config.Sonarr}
	stub := newPmeStub(t, false)
	m := newManagerTestService(t, stub, &config.Config{
		EnableArrSubfolders:   true,
		Arrs:                  []config.ArrConfig{sonarr},
		DownloadsDirectory:    t.TempDir(),
		TransferDirectory:     "arrDownloads",
		SimultaneousDownloads: 5,
		DownloadSpeedLimit:    100,
	}, map[string]string{})

	// The pme listing fails transiently: the resolution run tracks the
	// slug as unresolved (empty ID, excluded from the root scan by name).
	stub.setListFail("main-id", true)
	m.resolveArrFolders()
	m.arrFoldersMutex.Lock()
	id, ok := m.arrFolders["sonarr"]
	m.arrFoldersMutex.Unlock()
	if !ok || id != "" {
		t.Fatalf("after the failed resolution run arrFolders[sonarr] = %q (present=%v), want the tracked empty ID", id, ok)
	}

	// premiumize.me recovers: the normal poll heals the slug.
	stub.setListFail("main-id", false)
	m.TaskCheckPremiumizeDownloadsFolder()

	m.arrFoldersMutex.Lock()
	healedID, ok := m.arrFolders["sonarr"]
	m.arrFoldersMutex.Unlock()
	if !ok || healedID == "" {
		t.Fatalf("after the next poll arrFolders[sonarr] = %q (present=%v), want the resolved pme ID", healedID, ok)
	}
	// The healed slug is processed in the same poll: the per-slug pass
	// lists its pme folder.
	if !stub.anyCall(func(c pmeCall) bool {
		return c.Path == "/api/folder/list" && c.Query["id"] == healedID
	}) {
		t.Fatalf("healed Arr folder %s was not processed by the per-slug pass in the same poll: calls = %v", healedID, stub.calls)
	}
	// And the folder the pme created under the transfers directory stays
	// excluded from the root scan's download-and-delete path by ID.
	assertNoCallWithin(t, stub, time.Second, "healed Arr folder was handed to the root scan's delete path", func(c pmeCall) bool {
		return c.Query["id"] == healedID && c.Path == "/api/folder/delete"
	})
}

// TestManagerMkdirAllFailureKeepsResolvedArrFolder is the regression test
// for finding S-4: a local MkdirAll failure after a SUCCESSFUL pme
// resolution must not store "" for the slug - that would make the per-slug
// pass skip the slug forever and the pme folder would never be processed
// again. The resolved ID is kept, and the next resolution run (a config
// change) recreates the local subfolder and reuses the same ID instead of
// creating a second pme folder.
func TestManagerMkdirAllFailureKeepsResolvedArrFolder(t *testing.T) {
	sonarr := config.ArrConfig{Name: "sonarr", URL: "http://127.0.0.1:8989", APIKey: "k", Type: config.Sonarr}
	downloads := t.TempDir()
	// A plain file at the subfolder path blocks MkdirAll on every
	// platform (ENOTDIR: cannot create through a file).
	blocker := filepath.Join(downloads, "sonarr")
	if err := os.WriteFile(blocker, []byte("blocker"), 0o600); err != nil {
		t.Fatal(err)
	}

	stub := newPmeStub(t, false)
	m := newManagerTestService(t, stub, &config.Config{
		EnableArrSubfolders:   true,
		Arrs:                  []config.ArrConfig{sonarr},
		DownloadsDirectory:    downloads,
		TransferDirectory:     "arrDownloads",
		SimultaneousDownloads: 5,
		DownloadSpeedLimit:    100,
	}, map[string]string{})

	m.resolveArrFolders()

	m.arrFoldersMutex.Lock()
	id := m.arrFolders["sonarr"]
	m.arrFoldersMutex.Unlock()
	if id == "" {
		t.Fatal("arrFolders[sonarr] = \"\" after the MkdirAll failure; the successful pme resolution was discarded")
	}

	// Recovery: the blocker is cleared, and the next resolution run (a
	// config change, the product path) recreates the local subfolder. The
	// product's reconfiguration worker swaps the live config before
	// invoking the callback, and the resolution run reads the live
	// config - mirror that swap.
	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	recovered := t.TempDir()
	oldCfg := *m.config
	m.config.DownloadsDirectory = recovered
	newCfg := *m.config
	m.ConfigUpdatedCallback(oldCfg, newCfg)

	m.arrFoldersMutex.Lock()
	idAfter := m.arrFolders["sonarr"]
	m.arrFoldersMutex.Unlock()
	if idAfter != id {
		t.Fatalf("arrFolders[sonarr] = %q after recovery, want the same ID %q (the pme folder must not be re-created)", idAfter, id)
	}
	stub.mu.Lock()
	creates := 0
	for _, c := range stub.calls {
		if c.Path == "/api/folder/create" && c.Query["name"] == "sonarr" {
			creates++
		}
	}
	stub.mu.Unlock()
	if creates != 1 {
		t.Fatalf("folder/create calls for sonarr = %d, want 1 (the pme folder was re-created)", creates)
	}
	if _, err := os.Stat(filepath.Join(recovered, "sonarr")); err != nil {
		t.Fatalf("recovered downloads subfolder is missing: %v", err)
	}
}

// TestManagerRetryNeverResolvesRenamedAwayArrSlug is the regression test
// for finding S-22: the poll-time retry only re-resolves a slug that is
// STILL a configured Arr. A renamed-away (or removed) slug stays tracked
// with its empty ID - excluded from the root scan - and is never
// re-resolved: re-creating the pme folder for a name the user no longer
// configures would resurrect a folder the user abandoned.
func TestManagerRetryNeverResolvesRenamedAwayArrSlug(t *testing.T) {
	sonarr2 := config.ArrConfig{Name: "sonarr-2", URL: "http://127.0.0.1:8989", APIKey: "k", Type: config.Sonarr}
	stub := newPmeStub(t, false)
	stub.mu.Lock()
	// The pme root still holds the folder of the Arr under its old name.
	stub.table["main-id"] = []premiumizeme.Item{{ID: "old-sonarr-id", Name: "sonarr", Type: "folder"}}
	stub.mu.Unlock()

	m := newManagerTestService(t, stub, &config.Config{
		EnableArrSubfolders:   true,
		Arrs:                  []config.ArrConfig{sonarr2},
		DownloadsDirectory:    t.TempDir(),
		TransferDirectory:     "arrDownloads",
		SimultaneousDownloads: 5,
		DownloadSpeedLimit:    100,
	}, map[string]string{"sonarr": "old-sonarr-id"})

	// The resolution run seeds the renamed-away slug as a tracked
	// empty-ID entry and resolves the replacement Arr.
	m.resolveArrFolders()

	// The poll must not touch the renamed-away slug at all: no pme
	// create for its old name, and its tracked empty ID is preserved.
	m.TaskCheckPremiumizeDownloadsFolder()

	assertNoCallWithin(t, stub, time.Second, "renamed-away slug was re-resolved by the poll", func(c pmeCall) bool {
		return c.Path == "/api/folder/create" && c.Query["name"] == "sonarr"
	})
	assertNoCallWithin(t, stub, time.Second, "renamed-away slug's old folder was listed by the poll", func(c pmeCall) bool {
		return c.Query["id"] == "old-sonarr-id" && (c.Path == "/api/folder/list" || c.Path == "/api/folder/delete")
	})
	m.arrFoldersMutex.Lock()
	oldID, ok := m.arrFolders["sonarr"]
	m.arrFoldersMutex.Unlock()
	if !ok || oldID != "" {
		t.Fatalf("arrFolders[sonarr] = %q (present=%v), want the tracked empty ID to stay", oldID, ok)
	}
}

// TestManagerRetryMakesNoPmeCallsWithoutResolvedDownloadsFolder is the
// regression test for finding S-22: the poll-time retry makes no pme
// folder calls while the transfers folder itself is unresolved - an
// empty parent ID would list/create at the account root instead of the
// transfers directory.
func TestManagerRetryMakesNoPmeCallsWithoutResolvedDownloadsFolder(t *testing.T) {
	sonarr := config.ArrConfig{Name: "sonarr", URL: "http://127.0.0.1:8989", APIKey: "k", Type: config.Sonarr}
	stub := newPmeStub(t, false)
	m := newManagerTestService(t, stub, &config.Config{
		EnableArrSubfolders:   true,
		Arrs:                  []config.ArrConfig{sonarr},
		DownloadsDirectory:    t.TempDir(),
		TransferDirectory:     "arrDownloads",
		SimultaneousDownloads: 5,
		DownloadSpeedLimit:    100,
	}, map[string]string{"sonarr": ""})

	m.arrFoldersMutex.Lock()
	m.downloadsFolderID = ""
	m.arrFoldersMutex.Unlock()

	m.TaskCheckPremiumizeDownloadsFolder()

	assertNoCallWithin(t, stub, time.Second, "pme folder call while the downloads folder is unresolved", func(c pmeCall) bool {
		return c.Path == "/api/folder/list" || c.Path == "/api/folder/create"
	})
}
