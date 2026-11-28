package service

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/ensingerphilipp/premiumizearr-nova/internal/config"
	"github.com/ensingerphilipp/premiumizearr-nova/pkg/premiumizeme"
)

// gatedPmeServer is a premiumize.me stub whose folder/list requests wait
// on gate while it is open, so a test can commit a config change or a
// service-state switch exactly while a GetOrCreateSubfolderID round trip
// is in its pme window. listEntered is closed when the first list request
// arrives - proof that the caller's pre-flight config snapshot has run -
// and the call log is released before the gate wait, so the test can
// observe the in-flight request from another goroutine (pmeStub's single
// mutex is held across its gate wait and cannot be read there).
type gatedPmeServer struct {
	server        *httptest.Server
	mu            sync.Mutex
	calls         []pmeCall
	table         map[string][]premiumizeme.Item
	gate          chan struct{}
	listEntered   chan struct{}
	listAnnounced bool
	nextID        int
}

func newGatedPmeServer(t *testing.T) *gatedPmeServer {
	t.Helper()
	s := &gatedPmeServer{
		table:       map[string][]premiumizeme.Item{},
		gate:        make(chan struct{}),
		listEntered: make(chan struct{}),
		nextID:      1,
	}
	s.server = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.server.Close)
	return s
}

// openGate releases every list request held on the gate.
func (s *gatedPmeServer) openGate() {
	close(s.gate)
}

func (s *gatedPmeServer) handle(w http.ResponseWriter, r *http.Request) {
	call := pmeCall{Method: r.Method, Path: r.URL.Path, Query: map[string]string{}}
	for k, v := range r.URL.Query() {
		call.Query[k] = v[0]
	}
	s.mu.Lock()
	s.calls = append(s.calls, call)
	if !s.listAnnounced && r.URL.Path == "/api/folder/list" {
		s.listAnnounced = true
		s.mu.Unlock()
		close(s.listEntered)
	} else {
		s.mu.Unlock()
	}

	switch {
	case r.URL.Path == "/api/folder/list":
		<-s.gate
		s.mu.Lock()
		items := s.table[call.Query["id"]]
		s.mu.Unlock()
		writeJSON(w, map[string]any{"status": "success", "content": items})
	case r.URL.Path == "/api/folder/create":
		id := fmt.Sprintf("created-%d", s.nextID)
		s.mu.Lock()
		s.nextID++
		s.table[call.Query["parent_id"]] = append(s.table[call.Query["parent_id"]], premiumizeme.Item{ID: id, Name: call.Query["name"], Type: "folder"})
		s.mu.Unlock()
		writeJSON(w, map[string]any{"status": "success", "id": id})
	default:
		w.WriteHeader(http.StatusNotImplemented)
		_, _ = w.Write([]byte(`{"status":"error","message":"endpoint not implemented by the test stub"}`))
	}
}

func (s *gatedPmeServer) anyCall(pred func(pmeCall) bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.calls {
		if pred(c) {
			return true
		}
	}
	return false
}

// callsSnapshot returns a copy of the call log for failure messages.
func (s *gatedPmeServer) callsSnapshot() []pmeCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]pmeCall(nil), s.calls...)
}

// newRetryGuardTestService builds the transfer manager in the exact state
// a poll loop leaves it when an Arr's pme subfolder is still unresolved
// (tracked with an empty ID): the config is loaded through the product
// path (a real config file, so a whole-struct swap persists like a web
// save), the swap mutex is registered the way DirectoryWatcherService.
// Start does in the daemon, and the service holds an in-memory resolved
// downloads folder. The callback registered with the config is empty: the
// tests drive the guard's state transition directly, so the asynchronous
// reconfiguration worker cannot re-resolve the slug behind the test's
// back.
func newRetryGuardTestService(t *testing.T, srv *gatedPmeServer, arrs []config.ArrConfig, arrFolders map[string]string) (*TransferManagerService, *config.Config, *sync.RWMutex) {
	t.Helper()
	var mu sync.RWMutex
	config.SetUpdateMu(&mu)
	m := TransferManagerService{}.New()
	client := premiumizeme.NewPremiumizemeClient("test-key")
	client.APIBaseURL = srv.server.URL + "/api/"
	client.HTTPClient = srv.server.Client()
	m.premiumizemeClient = &client
	cfg, err := config.LoadOrCreateConfig(t.TempDir(), func(oldConfig, newConfig config.Config) {})
	if err != nil {
		t.Fatalf("LoadOrCreateConfig: %v", err)
	}
	cfg.DownloadsDirectory = t.TempDir()
	cfg.TransferDirectory = "arrDownloads"
	cfg.EnableArrSubfolders = true
	cfg.Arrs = arrs
	cfg.SimultaneousDownloads = 5
	m.config = &cfg
	m.downloadsFolderID = "main-id"
	m.arrFoldersMutex.Lock()
	m.arrFolders = arrFolders
	m.arrFoldersMutex.Unlock()
	return &m, &cfg, &mu
}

// runGatedRetry starts retryUnresolvedArrFolders in a goroutine against
// the gated server and waits until the round trip is provably in its pme
// window: its folder/list call has arrived and is held on the gate, so
// the pre-flight config snapshot is done and the post-round-trip
// re-validation cannot have run yet. It returns a channel closed when
// the retry run has returned.
func runGatedRetry(t *testing.T, m *TransferManagerService, srv *gatedPmeServer) <-chan struct{} {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.retryUnresolvedArrFolders()
	}()
	select {
	case <-srv.listEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("the in-flight round trip never reached its pme folder/list call")
	}
	return done
}

// TestManagerRetryDoesNotUpgradeSlugWhenConfigChangesInFlight is the
// regression test for finding R1-1: a slug whose GetOrCreateSubfolderID
// round trip is in flight must not be upgraded to the pme folder the
// round trip just created when the web ConfigHandler (whole-struct swap
// through UpdateConfig) disables EnableArrSubfolders or renames/removes
// the Arr in the meantime - upgrading would resurrect an abandoned Arr:
// pme folder re-created, slug routed there, content downloaded, pme
// container deleted. The swap goes through the registered swap mutex, so
// the guard's re-validation read cannot interleave with it.
func TestManagerRetryDoesNotUpgradeSlugWhenConfigChangesInFlight(t *testing.T) {
	sonarr := config.ArrConfig{Name: "sonarr", URL: "http://127.0.0.1:8989", APIKey: "k", Type: config.Sonarr}
	sonarr2 := config.ArrConfig{Name: "sonarr-2", URL: "http://127.0.0.1:8989", APIKey: "k", Type: config.Sonarr}

	t.Run("feature toggled off while the round trip is in flight", func(t *testing.T) {
		srv := newGatedPmeServer(t)
		m, cfg, _ := newRetryGuardTestService(t, srv, []config.ArrConfig{sonarr}, map[string]string{"sonarr": ""})

		done := runGatedRetry(t, m, srv)

		// Web-save shape: whole-struct swap through UpdateConfig, which
		// takes the registered swap mutex - the guard's re-validation
		// read cannot interleave with the swap.
		newCfg := *cfg
		newCfg.EnableArrSubfolders = false
		if err := cfg.UpdateConfig(newCfg); err != nil {
			t.Fatalf("UpdateConfig: %v", err)
		}

		srv.openGate()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("retryUnresolvedArrFolders did not return after the round trip completed")
		}
		config.WaitReconfigIdle()

		// The round trip ran to completion (its pme folder was created
		// under the old parent) - the test is not passing vacuously.
		if !srv.anyCall(func(c pmeCall) bool {
			return c.Path == "/api/folder/create" && c.Query["name"] == "sonarr" && c.Query["parent_id"] == "main-id"
		}) {
			t.Fatalf("the round trip did not create the pme folder: calls = %v", srv.callsSnapshot())
		}

		m.arrFoldersMutex.Lock()
		id, ok := m.arrFolders["sonarr"]
		m.arrFoldersMutex.Unlock()
		if !ok || id != "" {
			t.Fatalf("arrFolders[sonarr] = %q (present=%v); the slug must keep its empty-ID placeholder after the feature was toggled off in flight", id, ok)
		}
	})

	t.Run("arr renamed away while the round trip is in flight", func(t *testing.T) {
		srv := newGatedPmeServer(t)
		m, cfg, _ := newRetryGuardTestService(t, srv, []config.ArrConfig{sonarr}, map[string]string{"sonarr": ""})

		done := runGatedRetry(t, m, srv)

		// Web-save shape: whole-struct swap through UpdateConfig.
		newCfg := *cfg
		newCfg.Arrs = []config.ArrConfig{sonarr2}
		if err := cfg.UpdateConfig(newCfg); err != nil {
			t.Fatalf("UpdateConfig: %v", err)
		}

		srv.openGate()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("retryUnresolvedArrFolders did not return after the round trip completed")
		}
		config.WaitReconfigIdle()

		if !srv.anyCall(func(c pmeCall) bool {
			return c.Path == "/api/folder/create" && c.Query["name"] == "sonarr" && c.Query["parent_id"] == "main-id"
		}) {
			t.Fatalf("the round trip did not create the pme folder: calls = %v", srv.callsSnapshot())
		}

		m.arrFoldersMutex.Lock()
		id, ok := m.arrFolders["sonarr"]
		m.arrFoldersMutex.Unlock()
		if !ok || id != "" {
			t.Fatalf("arrFolders[sonarr] = %q (present=%v); a slug renamed away in flight must keep its empty-ID placeholder", id, ok)
		}
	})
}

// TestManagerRetryDoesNotAdoptFolderFromStaleParentAfterTransferDirectorySwitch
// is the regression test for finding R1-2: a slug whose
// GetOrCreateSubfolderID round trip is in flight must not be upgraded to
// a pme folder created under the old parent when the user switches
// TransferDirectory in the meantime (the config callback re-resolves the
// main downloads folder). The in-flight write-back must observe the
// switched downloadsFolderID and keep the empty-ID placeholder, so the
// next poll re-resolves the slug under the NEW parent instead of routing
// the Arr's files to the old parent's folder forever (where the pme
// container would be deleted after its content was downloaded).
func TestManagerRetryDoesNotAdoptFolderFromStaleParentAfterTransferDirectorySwitch(t *testing.T) {
	sonarr := config.ArrConfig{Name: "sonarr", URL: "http://127.0.0.1:8989", APIKey: "k", Type: config.Sonarr}
	srv := newGatedPmeServer(t)
	m, cfg, mu := newRetryGuardTestService(t, srv, []config.ArrConfig{sonarr}, map[string]string{"sonarr": ""})

	done := runGatedRetry(t, m, srv)

	// The state a TransferDirectory switch commits through the config
	// callback: a new transfers directory, and the re-resolved main
	// folder for it. Both are committed directly - the config field under
	// the swap mutex, the main folder under the service-state mutex - to
	// keep the guard under test isolated from the callback's own
	// re-resolution of the slug.
	mu.Lock()
	cfg.TransferDirectory = "arrDownloads2"
	mu.Unlock()
	m.arrFoldersMutex.Lock()
	m.downloadsFolderID = "new-main-id"
	m.arrFoldersMutex.Unlock()

	srv.openGate()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("retryUnresolvedArrFolders did not return after the round trip completed")
	}

	// The round trip ran to completion against the OLD parent - the test
	// is not passing vacuously.
	if !srv.anyCall(func(c pmeCall) bool {
		return c.Path == "/api/folder/create" && c.Query["name"] == "sonarr" && c.Query["parent_id"] == "main-id"
	}) {
		t.Fatalf("the round trip did not create the pme folder under the old parent: calls = %v", srv.callsSnapshot())
	}

	m.arrFoldersMutex.Lock()
	id, ok := m.arrFolders["sonarr"]
	m.arrFoldersMutex.Unlock()
	if !ok || id != "" {
		t.Fatalf("arrFolders[sonarr] = %q (present=%v); a folder created under the old parent must not be adopted after a TransferDirectory switch", id, ok)
	}

	// The next poll re-resolves the slug under the new parent (the guard
	// only rejects the stale in-flight result), and the pme folder the
	// aborted round trip left under the old parent is neither adopted,
	// downloaded, nor deleted.
	m.TaskCheckPremiumizeDownloadsFolder()

	srv.mu.Lock()
	var staleID, healedParentID string
	for _, item := range srv.table["main-id"] {
		if item.Name == "sonarr" {
			staleID = item.ID
		}
	}
	for _, item := range srv.table["new-main-id"] {
		if item.Name == "sonarr" {
			healedParentID = item.ID
		}
	}
	srv.mu.Unlock()
	if healedParentID == "" {
		srv.mu.Lock()
		table := srv.table
		srv.mu.Unlock()
		t.Fatalf("the next poll did not re-resolve the slug under the new parent: table = %v", table)
	}

	m.arrFoldersMutex.Lock()
	healedID, ok := m.arrFolders["sonarr"]
	m.arrFoldersMutex.Unlock()
	if !ok || healedID != healedParentID {
		t.Fatalf("arrFolders[sonarr] = %q (present=%v) after the next poll; want the ID resolved under the new transfers directory %q", healedID, ok, healedParentID)
	}
	if healedID == staleID {
		t.Fatalf("arrFolders[sonarr] = %q was adopted from the old parent", healedID)
	}
	if srv.anyCall(func(c pmeCall) bool {
		return c.Query["id"] == staleID && (c.Path == "/api/folder/list" || c.Path == "/api/folder/delete")
	}) {
		t.Fatalf("stale in-flight folder under the old parent was listed or deleted: calls = %v", srv.callsSnapshot())
	}
}
