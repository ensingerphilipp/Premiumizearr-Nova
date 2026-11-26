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

// TestLoadConfigFromDiskBackfillsMaxActiveTransfers is the acceptance test
// for review finding R1-11: a config file without a positive
// MaxActivePremiumizeTransfers value — the key missing, or a 0 stored via
// the web route before load-time normalization landed — is normalized to
// the published default of 20 on load, and the backfilled value is written
// back to the file, mirroring the grace-period backfill tests above.
func TestLoadConfigFromDiskBackfillsMaxActiveTransfers(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{name: "missing key", value: ""},
		{name: "explicit zero", value: "0"},
		{name: "explicit negative", value: "-3"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			file := "PremiumizemeAPIKey: xxxxxxxxx\n"
			if test.value != "" {
				file += "MaxActivePremiumizeTransfers: " + test.value + "\n"
			}
			writeConfigFile(t, dir, file)

			cfg, err := loadConfigFromDisk(dir)
			if err != nil {
				t.Fatalf("loadConfigFromDisk() error = %v, want nil", err)
			}
			if cfg.MaxActivePremiumizeTransfers != DefaultMaxActivePremiumizeTransfers {
				t.Fatalf("MaxActivePremiumizeTransfers = %d, want %d (backfilled default)", cfg.MaxActivePremiumizeTransfers, DefaultMaxActivePremiumizeTransfers)
			}

			rewritten := readConfigFile(t, dir)
			if !strings.Contains(rewritten, "MaxActivePremiumizeTransfers: 20") {
				t.Fatalf("config file does not contain the backfilled default:\n%s", rewritten)
			}
		})
	}
}

// TestLoadConfigFromDiskKeepsSetMaxActiveTransfers verifies that an
// explicitly set positive MaxActivePremiumizeTransfers is preserved on
// load instead of being overwritten by the default.
func TestLoadConfigFromDiskKeepsSetMaxActiveTransfers(t *testing.T) {
	dir := t.TempDir()
	writeConfigFile(t, dir, "PremiumizemeAPIKey: xxxxxxxxx\nMaxActivePremiumizeTransfers: 25\n")

	cfg, err := loadConfigFromDisk(dir)
	if err != nil {
		t.Fatalf("loadConfigFromDisk() error = %v, want nil", err)
	}
	if cfg.MaxActivePremiumizeTransfers != 25 {
		t.Fatalf("MaxActivePremiumizeTransfers = %d, want 25 (the explicitly set value)", cfg.MaxActivePremiumizeTransfers)
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
