package config

import (
	"os"
	"path"
	"strings"
	"testing"
)

// writeConfigFile writes raw YAML content to dir/config.yaml.
func writeConfigFile(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(path.Join(dir, "config.yaml"), []byte(content), 0644); err != nil {
		t.Fatalf("write config.yaml: %v", err)
	}
}

// readConfigFile returns the content of dir/config.yaml.
func readConfigFile(t *testing.T, dir string) string {
	t.Helper()
	data, err := os.ReadFile(path.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatalf("read config.yaml: %v", err)
	}
	return string(data)
}

func TestDirectClientKeyGeneratedOnceAndSavedPrivate(t *testing.T) {
	dir := t.TempDir()
	cfg, err := LoadOrCreateConfig(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.DirectClientAPIKey) != 48 {
		t.Fatalf("generated direct client key length = %d, want 48", len(cfg.DirectClientAPIKey))
	}
	info, err := os.Stat(path.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Fatalf("config permissions = %04o, want 0600", got)
	}
	reloaded, err := LoadOrCreateConfig(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.DirectClientAPIKey != cfg.DirectClientAPIKey {
		t.Fatal("direct client key changed after restart")
	}
}

// TestLoadOrCreateConfigTightensLegacyReadableConfig verifies that a legacy
// 0644 config (predating the private-file tightening) ends up as a 0600
// file after the fresh DirectClientAPIKey is generated, with the key stable
// across reloads: the new content must never sit in a world-readable file,
// so it is written to a 0600 temp file and renamed into place.
func TestLoadOrCreateConfigTightensLegacyReadableConfig(t *testing.T) {
	dir := t.TempDir()
	writeConfigFile(t, dir, "PremiumizemeAPIKey: xxxxxxxxx\n") // 0644, empty direct client key

	cfg, err := LoadOrCreateConfig(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.DirectClientAPIKey) != 48 {
		t.Fatalf("generated direct client key length = %d, want 48", len(cfg.DirectClientAPIKey))
	}
	info, err := os.Stat(path.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Fatalf("config permissions = %04o, want 0600 after tightening a legacy 0644 file", got)
	}
	reloaded, err := LoadOrCreateConfig(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.DirectClientAPIKey != cfg.DirectClientAPIKey {
		t.Fatal("direct client key changed after reload")
	}
	if info, err := os.Stat(path.Join(dir, "config.yaml")); err != nil {
		t.Fatal(err)
	} else if got := info.Mode().Perm(); got != 0600 {
		t.Fatalf("config permissions after reload = %04o, want 0600", got)
	}
}

// TestSaveFailureKeepsPreviousConfigAndLeavesNoDebris verifies the failure
// side of the atomic save: when the final rename cannot happen, Save must
// report the error, leave the previous config byte-for-byte intact, and
// not leave a temp file behind.
func TestSaveFailureKeepsPreviousConfigAndLeavesNoDebris(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{PremiumizemeAPIKey: "xxxxxxxxx"}
	cfg.altConfigLocation = dir

	// A directory named config.yaml makes the final rename fail (rename to
	// an existing directory is an error), whatever the user running the
	// test is.
	if err := os.Mkdir(path.Join(dir, "config.yaml"), 0755); err != nil {
		t.Fatal(err)
	}

	if err := cfg.Save(); err == nil {
		t.Fatal("Save() succeeded, want an error for the blocked rename")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() == "config.yaml" {
			continue
		}
		t.Fatalf("leftover file %q after a failed save", e.Name())
	}
	info, err := os.Stat(path.Join(dir, "config.yaml"))
	if err != nil || !info.IsDir() {
		t.Fatalf("previous config target changed by the failed save: %v", err)
	}
}

// TestLoadConfigFromDiskBackfillsGracePeriod verifies that a legacy config
// file without ErroredTransferDeleteGracePeriodSeconds is backfilled with
// the 300 second default on load, and that the backfilled value is written
// back to the file.
func TestLoadConfigFromDiskBackfillsGracePeriod(t *testing.T) {
	dir := t.TempDir()
	writeConfigFile(t, dir, "PremiumizemeAPIKey: xxxxxxxxx\n")

	cfg, err := loadConfigFromDisk(dir)
	if err != nil {
		t.Fatalf("loadConfigFromDisk() error = %v, want nil", err)
	}
	if cfg.ErroredTransferDeleteGracePeriodSeconds != 300 {
		t.Fatalf("ErroredTransferDeleteGracePeriodSeconds = %d, want 300 (backfilled default)", cfg.ErroredTransferDeleteGracePeriodSeconds)
	}

	file := readConfigFile(t, dir)
	if !strings.Contains(file, "ErroredTransferDeleteGracePeriodSeconds: 300") {
		t.Fatalf("config file does not contain the backfilled grace period:\n%s", file)
	}
}

// TestLoadConfigFromDiskKeepsSetGracePeriod verifies that an explicitly set
// ErroredTransferDeleteGracePeriodSeconds is preserved on load instead of
// being overwritten by the default.
func TestLoadConfigFromDiskKeepsSetGracePeriod(t *testing.T) {
	dir := t.TempDir()
	writeConfigFile(t, dir, "PremiumizemeAPIKey: xxxxxxxxx\nErroredTransferDeleteGracePeriodSeconds: 120\n")

	cfg, err := loadConfigFromDisk(dir)
	if err != nil {
		t.Fatalf("loadConfigFromDisk() error = %v, want nil", err)
	}
	if cfg.ErroredTransferDeleteGracePeriodSeconds != 120 {
		t.Fatalf("ErroredTransferDeleteGracePeriodSeconds = %d, want 120 (the explicitly set value)", cfg.ErroredTransferDeleteGracePeriodSeconds)
	}
}

// TestLoadConfigFromDiskBackfillsMissingArrs verifies that a legacy config
// file without an Arrs section is backfilled with an empty (non-nil) slice
// on load, so the config API never serves "Arrs": null for it.
func TestLoadConfigFromDiskBackfillsMissingArrs(t *testing.T) {
	dir := t.TempDir()
	writeConfigFile(t, dir, "PremiumizemeAPIKey: xxxxxxxxx\n")

	cfg, err := loadConfigFromDisk(dir)
	if err != nil {
		t.Fatalf("loadConfigFromDisk() error = %v, want nil", err)
	}
	if cfg.Arrs == nil {
		t.Fatalf("Arrs = nil, want non-nil empty slice (backfilled default)")
	}
	if len(cfg.Arrs) != 0 {
		t.Fatalf("Arrs = %v, want empty slice (no entries in the file)", cfg.Arrs)
	}

	file := readConfigFile(t, dir)
	if !strings.Contains(file, "Arrs: []") {
		t.Fatalf("config file does not contain the backfilled empty Arrs list:\n%s", file)
	}
}

// TestLoadConfigFromDiskKeepsSetArrs verifies that an explicitly configured
// Arrs list is preserved on load instead of being replaced by the backfill.
func TestLoadConfigFromDiskKeepsSetArrs(t *testing.T) {
	dir := t.TempDir()
	writeConfigFile(t, dir, "PremiumizemeAPIKey: xxxxxxxxx\nArrs:\n  - Name: Sonarr\n    URL: http://127.0.0.1:8989\n    APIKey: xxxxxxxxx\n    Type: Sonarr\n")

	cfg, err := loadConfigFromDisk(dir)
	if err != nil {
		t.Fatalf("loadConfigFromDisk() error = %v, want nil", err)
	}
	if len(cfg.Arrs) != 1 {
		t.Fatalf("Arrs = %v, want the single entry from the file", cfg.Arrs)
	}
	if cfg.Arrs[0].Name != "Sonarr" || cfg.Arrs[0].URL != "http://127.0.0.1:8989" || cfg.Arrs[0].Type != Sonarr {
		t.Fatalf("Arrs[0] = %+v, want the entry from the file", cfg.Arrs[0])
	}
}

// TestLoadConfigFromDiskNormalizesZeroSimultaneousDownloads verifies that a
// hand-edited 0 (or negative) SimultaneousDownloads is normalized to the
// 5 default on load and written back: a 0 would otherwise pass the
// load gate and then bind every direct download to a zero slot limit,
// stalling them silently forever.
func TestLoadConfigFromDiskNormalizesZeroSimultaneousDownloads(t *testing.T) {
	for _, input := range []string{"SimultaneousDownloads: 0", "SimultaneousDownloads: -2"} {
		t.Run(input, func(t *testing.T) {
			dir := t.TempDir()
			writeConfigFile(t, dir, "PremiumizemeAPIKey: xxxxxxxxx\n"+input+"\n")

			cfg, err := loadConfigFromDisk(dir)
			if err != nil {
				t.Fatalf("loadConfigFromDisk() error = %v, want nil", err)
			}
			if cfg.SimultaneousDownloads != 5 {
				t.Fatalf("SimultaneousDownloads = %d, want 5 (normalized on load)", cfg.SimultaneousDownloads)
			}
			file := readConfigFile(t, dir)
			if !strings.Contains(file, "SimultaneousDownloads: 5") {
				t.Fatalf("config file does not contain the normalized limit:\n%s", file)
			}
		})
	}
}

// TestLoadConfigFromDiskKeepsSetSimultaneousDownloads verifies that an
// explicitly set positive SimultaneousDownloads is preserved on load
// instead of being overwritten by the default.
func TestLoadConfigFromDiskKeepsSetSimultaneousDownloads(t *testing.T) {
	dir := t.TempDir()
	writeConfigFile(t, dir, "PremiumizemeAPIKey: xxxxxxxxx\nSimultaneousDownloads: 3\n")

	cfg, err := loadConfigFromDisk(dir)
	if err != nil {
		t.Fatalf("loadConfigFromDisk() error = %v, want nil", err)
	}
	if cfg.SimultaneousDownloads != 3 {
		t.Fatalf("SimultaneousDownloads = %d, want 3 (the explicitly set value)", cfg.SimultaneousDownloads)
	}
}

// TestUpdateConfigNormalizesNilArrs verifies that an API update body that
// omits Arrs (which JSON decodes as a nil slice) is normalized to an empty
// non-nil slice, so GET /api/config cannot be left serving "Arrs": null.
func TestUpdateConfigNormalizesNilArrs(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		PremiumizemeAPIKey: "xxxxxxxxx",
		Arrs: []ArrConfig{
			{Name: "Sonarr", URL: "http://127.0.0.1:8989", APIKey: "xxxxxxxxx", Type: Sonarr},
		},
	}
	cfg.altConfigLocation = dir
	cfg.appCallback = func(oldConfig Config, newConfig Config) {}

	cfg.UpdateConfig(Config{PremiumizemeAPIKey: "xxxxxxxxx"})

	if cfg.Arrs == nil {
		t.Fatalf("Arrs = nil after UpdateConfig, want non-nil empty slice")
	}
	if len(cfg.Arrs) != 0 {
		t.Fatalf("Arrs = %v, want empty slice (omitted in the update body)", cfg.Arrs)
	}

	file := readConfigFile(t, dir)
	if !strings.Contains(file, "Arrs: []") {
		t.Fatalf("config file does not contain the normalized empty Arrs list:\n%s", file)
	}
}
