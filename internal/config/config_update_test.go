package config

import (
	"errors"
	"os"
	"path"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ensingerphilipp/premiumizearr-nova/internal/utils"

	"gopkg.in/yaml.v2"
)

// TestUpdateConfigConcurrentSavesPersistCompleteConfigs is the regression
// test for finding C-17: two concurrent web saves call UpdateConfig on the
// same Config; the private-field carry-over reads, the swapped-in copy
// handed to the callback, and the field-wise YAML marshal in Save must all
// run under the registered swap lock, or a swap interleaved with the
// marshal persists a config that mixes fields of the two updates. Run with
// -race; the pre-fix tree reports data races on the carry-over reads and
// the marshal.
func TestUpdateConfigConcurrentSavesPersistCompleteConfigs(t *testing.T) {
	loc := t.TempDir()
	cfg, err := LoadOrCreateConfig(loc, func(oldConfig, newConfig Config) {})
	if err != nil {
		t.Fatalf("LoadOrCreateConfig: %v", err)
	}
	var mu sync.RWMutex
	SetUpdateMu(&mu)

	a := cfg
	a.BlackholeDirectory = "blackhole-a"
	a.Arrs = []ArrConfig{{Name: "alpha"}}
	b := cfg
	b.BlackholeDirectory = "blackhole-b"
	b.Arrs = []ArrConfig{{Name: "beta"}}

	const iterations = 50
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < iterations; i++ {
			(&cfg).UpdateConfig(a)
		}
	}()
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < iterations; i++ {
			(&cfg).UpdateConfig(b)
		}
	}()
	close(start)
	wg.Wait()

	data, err := os.ReadFile(path.Join(loc, "config.yaml"))
	if err != nil {
		t.Fatalf("read persisted config: %v", err)
	}
	var persisted Config
	if err := yaml.Unmarshal(data, &persisted); err != nil {
		t.Fatalf("persisted config does not parse: %v", err)
	}

	// The persisted file must be one complete update, never a field-wise
	// mixture of the two.
	isA := persisted.BlackholeDirectory == "blackhole-a" &&
		len(persisted.Arrs) == 1 && persisted.Arrs[0].Name == "alpha"
	isB := persisted.BlackholeDirectory == "blackhole-b" &&
		len(persisted.Arrs) == 1 && persisted.Arrs[0].Name == "beta"
	if !isA && !isB {
		t.Fatalf("persisted config is a torn mixture of the two updates: BlackholeDirectory=%q Arrs=%+v", persisted.BlackholeDirectory, persisted.Arrs)
	}
}

// TestUpdateConfigPersistsBeforeCallbackRuns is the regression test for
// finding C-4 (a) and (f): a blocked service callback (premiumize.me down)
// must not indefinitely block the save, and the validated config must be
// safely stored before the reconfiguration runs. The callback's operational
// failure is logged and retried by the services; it never undoes the saved
// config.
func TestUpdateConfigPersistsBeforeCallbackRuns(t *testing.T) {
	loc := t.TempDir()
	// Channel-based handshakes instead of a captured sync.WaitGroup: the
	// reconfiguration worker is process-wide and may already run from an
	// earlier test, so the closure's captures must be shared with the test
	// goroutine through channels alone (a value-type capture's heap storage
	// has no happens-before edge to a worker created before this test).
	releaseCallback := make(chan struct{})
	callbackDone := make(chan struct{})
	cfg, err := LoadOrCreateConfig(loc, func(oldConfig, newConfig Config) {
		// Blocked like a service callback waiting on a premiumize.me
		// round trip that never answers.
		<-releaseCallback
		close(callbackDone)
	})
	if err != nil {
		t.Fatalf("LoadOrCreateConfig: %v", err)
	}
	var mu sync.RWMutex
	SetUpdateMu(&mu)

	newCfg := cfg
	newCfg.BlackholeDirectory = "blackhole-new"

	// The callback is blocked for the whole test: a synchronous
	// reconfiguration (the pre-C-4 shape) would hold UpdateConfig here
	// until the test's final release, well past this bound.
	start := time.Now()
	if err := (&cfg).UpdateConfig(newCfg); err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("UpdateConfig waited %s on the blocked callback", elapsed)
	}

	// The validated config is safely stored at this point, callback or not.
	data, err := os.ReadFile(path.Join(loc, "config.yaml"))
	if err != nil {
		t.Fatalf("read persisted config: %v", err)
	}
	var persisted Config
	if err := yaml.Unmarshal(data, &persisted); err != nil {
		t.Fatalf("persisted config does not parse: %v", err)
	}
	if persisted.BlackholeDirectory != "blackhole-new" {
		t.Fatalf("persisted BlackholeDirectory = %q, want the new value while the callback is still blocked", persisted.BlackholeDirectory)
	}

	// Unblock the callback, let the worker drain, and verify the blocked
	// reconfiguration did not roll the saved state back.
	close(releaseCallback)
	<-callbackDone
	WaitReconfigIdle()
	if cfg.BlackholeDirectory != "blackhole-new" {
		t.Fatalf("in-memory BlackholeDirectory = %q after the blocked callback drained, want the saved value", cfg.BlackholeDirectory)
	}
}

// TestUpdateConfigReconfigurationIsSerializedLatestState is the regression
// test for finding C-4 (c) and S-28: reconfiguration work runs on a single
// serialized worker, and an invocation applies the newest committed state
// - a slow, stale callback can never restore an older state on top of a
// newer save. Coalesced swaps pass the oldest missed state as the old
// argument so change detection stays correct.
func TestUpdateConfigReconfigurationIsSerializedLatestState(t *testing.T) {
	loc := t.TempDir()
	gate := make(chan struct{})
	var cbMu sync.Mutex
	type callRec struct {
		oldBlackhole string
		newBlackhole string
	}
	var calls []callRec
	cfg, err := LoadOrCreateConfig(loc, func(oldConfig, newConfig Config) {
		cbMu.Lock()
		first := len(calls) == 0
		cbMu.Unlock()
		if first {
			// The first (slowest) callback blocks while the test commits
			// two more saves on top of it.
			<-gate
		}
		cbMu.Lock()
		calls = append(calls, callRec{oldConfig.BlackholeDirectory, newConfig.BlackholeDirectory})
		cbMu.Unlock()
	})
	if err != nil {
		t.Fatalf("LoadOrCreateConfig: %v", err)
	}
	var mu sync.RWMutex
	SetUpdateMu(&mu)

	a := cfg
	a.BlackholeDirectory = "blackhole-a"
	b := cfg
	b.BlackholeDirectory = "blackhole-b"
	c := cfg
	c.BlackholeDirectory = "blackhole-c"

	if err := (&cfg).UpdateConfig(a); err != nil {
		t.Fatalf("UpdateConfig(a): %v", err)
	}
	if err := (&cfg).UpdateConfig(b); err != nil {
		t.Fatalf("UpdateConfig(b): %v", err)
	}
	if err := (&cfg).UpdateConfig(c); err != nil {
		t.Fatalf("UpdateConfig(c): %v", err)
	}

	// All three saves returned (and persisted) while the first callback
	// is still gated.
	committed := map[string]bool{
		"blackhole-a": true,
		"blackhole-b": true,
		"blackhole-c": true,
	}
	close(gate)
	WaitReconfigIdle()

	cbMu.Lock()
	defer cbMu.Unlock()
	if len(calls) == 0 {
		t.Fatal("no reconfiguration callback ran")
	}
	for i, call := range calls {
		if !committed[call.newBlackhole] {
			t.Fatalf("callback %d applied a state that was never committed: new=%q", i, call.newBlackhole)
		}
	}
	// The last invocation applied the newest committed state: a stale
	// callback that applied an older state last would leave it in memory.
	last := calls[len(calls)-1]
	if last.newBlackhole != "blackhole-c" {
		t.Fatalf("last callback applied new=%q, want the newest committed state blackhole-c (old=%q)", last.newBlackhole, last.oldBlackhole)
	}
	// Coalescing must keep the oldest missed state for change detection.
	if last.oldBlackhole != "" && last.oldBlackhole != "blackhole-a" && last.oldBlackhole != "blackhole-b" {
		t.Fatalf("coalesced callback old=%q does not match any committed state", last.oldBlackhole)
	}

	// The in-memory config is the newest committed state, untouched by the
	// stale callback.
	if cfg.BlackholeDirectory != "blackhole-c" {
		t.Fatalf("in-memory BlackholeDirectory = %q, want blackhole-c", cfg.BlackholeDirectory)
	}
	data, err := os.ReadFile(path.Join(loc, "config.yaml"))
	if err != nil {
		t.Fatalf("read persisted config: %v", err)
	}
	var persisted Config
	if err := yaml.Unmarshal(data, &persisted); err != nil {
		t.Fatalf("persisted config does not parse: %v", err)
	}
	if persisted.BlackholeDirectory != "blackhole-c" {
		t.Fatalf("persisted BlackholeDirectory = %q, want blackhole-c", persisted.BlackholeDirectory)
	}
}

// TestUpdateConfigSaveFailurePropagates is the regression test for finding
// C-4 (f): a config save failure is returned to the caller (the web
// endpoint reports it) instead of claiming success; the in-memory swap is
// kept because the new state is valid and already committed.
func TestUpdateConfigSaveFailurePropagates(t *testing.T) {
	loc := t.TempDir()
	cfg, err := LoadOrCreateConfig(loc, func(oldConfig, newConfig Config) {})
	if err != nil {
		t.Fatalf("LoadOrCreateConfig: %v", err)
	}
	var mu sync.RWMutex
	SetUpdateMu(&mu)
	cfg.BlackholeDirectory = "blackhole-before"
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Point the save at a location that cannot be written.
	cfg.altConfigLocation = filepath.Join(loc, "does-not-exist")
	newCfg := cfg
	newCfg.BlackholeDirectory = "blackhole-after"

	err = (&cfg).UpdateConfig(newCfg)
	if err == nil {
		t.Fatal("UpdateConfig returned nil error for an unwritable save location")
	}
	// The in-memory swap is kept: the new state is valid and committed.
	if cfg.BlackholeDirectory != "blackhole-after" {
		t.Fatalf("in-memory BlackholeDirectory = %q, want blackhole-after", cfg.BlackholeDirectory)
	}
	// The previous file was not overwritten by the failed save.
	data, err := os.ReadFile(path.Join(loc, "config.yaml"))
	if err != nil {
		t.Fatalf("read persisted config: %v", err)
	}
	var persisted Config
	if err := yaml.Unmarshal(data, &persisted); err != nil {
		t.Fatalf("persisted config does not parse: %v", err)
	}
	if persisted.BlackholeDirectory != "blackhole-before" {
		t.Fatalf("failed save still wrote the new value: BlackholeDirectory=%q", persisted.BlackholeDirectory)
	}
	WaitReconfigIdle()
}

// TestLoadOrCreateConfigRejectsEmptyBlackholeWithFeatureOn is the
// regression test for findings S-19/S-29: a startup config with the
// feature on and an empty BlackholeDirectory is rejected with a clear
// error before any service starts, instead of silently running with
// relative "." subfolder paths. Docker installs are backfilled before the
// gate, so they are unaffected.
func TestLoadOrCreateConfigRejectsEmptyBlackholeWithFeatureOn(t *testing.T) {
	if utils.IsRunningInDockerContainer() {
		t.Skip("docker backfill overrides the blank blackhole directory before the gate")
	}
	for _, tc := range []struct {
		name      string
		enabled   bool
		blackhole string
		wantErr   bool
	}{
		{"feature on with empty blackhole is rejected", true, "", true},
		{"feature off with empty blackhole is accepted", false, "", false},
		{"feature on with a blackhole is accepted", true, "/blackhole", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			loc := t.TempDir()
			seed := Config{
				PremiumizemeAPIKey:                      "key",
				BlackholeDirectory:                      tc.blackhole,
				TransferDirectory:                       "arrDownloads",
				BindIP:                                  "0.0.0.0",
				BindPort:                                "8182",
				SimultaneousDownloads:                   5,
				DownloadSpeedLimit:                      100,
				ArrHistoryUpdateIntervalSeconds:         20,
				ErroredTransferDeleteGracePeriodSeconds: 300,
				EnableArrSubfolders:                     tc.enabled,
				Arrs:                                    []ArrConfig{{Name: "sonarr", URL: "http://127.0.0.1:8989", APIKey: "k", Type: Sonarr}},
			}
			data, err := yaml.Marshal(seed)
			if err != nil {
				t.Fatalf("marshal seed config: %v", err)
			}
			if err := os.WriteFile(filepath.Join(loc, "config.yaml"), data, 0o600); err != nil {
				t.Fatalf("write seed config: %v", err)
			}

			_, err = LoadOrCreateConfig(loc, func(oldConfig, newConfig Config) {})
			if tc.wantErr {
				if !errors.Is(err, ErrEmptyBlackholeDirectory) {
					t.Fatalf("LoadOrCreateConfig error = %v, want ErrEmptyBlackholeDirectory", err)
				}
			} else if err != nil {
				t.Fatalf("LoadOrCreateConfig error = %v, want nil", err)
			}
		})
	}
}
