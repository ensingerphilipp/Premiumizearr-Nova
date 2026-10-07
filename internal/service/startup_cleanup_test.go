package service

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ensingerphilipp/premiumizearr-nova/internal/config"
)

func TestDownloadCleanupDeferredUntilWorkerRuns(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "old.mkv")
	recent := filepath.Join(dir, "recent.mkv")
	for _, name := range []string{old, recent} {
		if err := os.WriteFile(name, []byte("media"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	modified := time.Now().AddDate(0, 0, -5)
	if err := os.Chtimes(old, modified, modified); err != nil {
		t.Fatal(err)
	}
	manager := TransferManagerService{}.New()
	manager.Init(nil, nil, &config.Config{DownloadsDirectory: dir})
	if _, err := os.Stat(old); err != nil {
		t.Fatalf("Init must not touch downloads before HTTP starts: %v", err)
	}
	manager.CleanUpDownloadDirPeriod()
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("cleanup must still remove expired downloads: %v", err)
	}
	if _, err := os.Stat(recent); err != nil {
		t.Fatalf("cleanup must retain recent downloads: %v", err)
	}
}
