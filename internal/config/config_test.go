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
