package config

// Round-2 regression tests for the finding that UpdateConfig fired the
// app fan-out (the web server's in-handler Close()+Start() that switches
// which key the /qbit and /sab endpoints accept) BEFORE persisting the new
// config, and discarded Save()'s error: a failed or interrupted save left
// the file on the old DirectClientAPIKey while the running server (and the
// UI) presented the new one, and the route still answered succeeded:true.
// UpdateConfig now persists first, rolls the in-memory config back on
// failure, returns the save error, and fires the fan-out only for a change
// that is on disk.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestUpdateConfigSaveFailureRollsBackAndReportsError drives the
// save-failure branch: with the save's temp file landing under a path
// whose parent is a regular file (ENOTDIR — the same error path a
// read-only or full config directory takes at CreateTemp), UpdateConfig
// must return the error, roll memory back to the on-disk value, leave the
// file untouched, and not fire the fan-out; a later save to a repaired
// location must still converge, with the fan-out firing exactly once.
func TestUpdateConfigSaveFailureRollsBackAndReportsError(t *testing.T) {
	dir := t.TempDir()
	var callbackCalls int
	cfg, err := LoadOrCreateConfig(dir, func(oldConfig, newConfig Config) {
		callbackCalls++
	})
	if err != nil {
		t.Fatalf("LoadOrCreateConfig: %v", err)
	}
	originalKey := cfg.DirectClientAPIKey
	if originalKey == "" {
		t.Fatal("expected LoadOrCreateConfig to generate a DirectClientAPIKey")
	}

	// Break persistence while the original config.yaml stays on disk.
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg.altConfigLocation = filepath.Join(blocker, "inner")

	newCfg := cfg
	newCfg.DirectClientAPIKey = "rotated-direct-key-2"
	err = cfg.UpdateConfig(newCfg)
	if err == nil {
		t.Fatal("UpdateConfig with an unwritable config location must return the save error")
	}
	if cfg.DirectClientAPIKey != originalKey {
		t.Fatalf("failed update left the new key in memory: got %q, want the original on-disk key %q", cfg.DirectClientAPIKey, originalKey)
	}
	if callbackCalls != 0 {
		t.Fatalf("failed update fired the app fan-out %d times; an unpersisted change must not restart or re-key live services", callbackCalls)
	}
	data, err := os.ReadFile(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatalf("read config.yaml: %v", err)
	}
	if !strings.Contains(string(data), originalKey) || strings.Contains(string(data), "rotated-direct-key-2") {
		t.Fatalf("config.yaml after the failed update:\n%s", data)
	}

	// The recovery path: a save to a repaired location still converges,
	// with the fan-out firing for the persisted change only.
	cfg.altConfigLocation = dir
	newCfg = cfg
	newCfg.DirectClientAPIKey = "rotated-direct-key-3"
	if err := cfg.UpdateConfig(newCfg); err != nil {
		t.Fatalf("UpdateConfig after repairing the config location: %v", err)
	}
	if callbackCalls != 1 {
		t.Fatalf("fan-out calls = %d, want exactly 1 (only a persisted update fires it)", callbackCalls)
	}
	if cfg.DirectClientAPIKey != "rotated-direct-key-3" {
		t.Fatalf("config key = %q after the successful update, want rotated-direct-key-3", cfg.DirectClientAPIKey)
	}
	data, err = os.ReadFile(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatalf("read config.yaml: %v", err)
	}
	if !strings.Contains(string(data), "rotated-direct-key-3") {
		t.Fatalf("persisted config.yaml lacks the rotated key:\n%s", data)
	}
}
