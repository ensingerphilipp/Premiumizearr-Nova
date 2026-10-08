package service

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ensingerphilipp/premiumizearr-nova/internal/config"
	log "github.com/sirupsen/logrus"
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

// A walk that stalls in filepath.Walk on an unresponsive mount must be
// distinguishable in the log from a finished no-op cleanup: the period pass
// emits a distinct entry on completion, which a stalled walk never reaches.
func TestCleanUpDownloadDirPeriodLogsCompletion(t *testing.T) {
	var buf bytes.Buffer
	prevOut := log.StandardLogger().Out
	prevLevel := log.GetLevel()
	log.SetOutput(&buf)
	log.SetLevel(log.InfoLevel)
	t.Cleanup(func() {
		log.SetLevel(prevLevel)
		log.SetOutput(prevOut)
	})

	dir := t.TempDir()
	manager := TransferManagerService{}.New()
	manager.Init(nil, nil, &config.Config{DownloadsDirectory: dir})
	manager.CleanUpDownloadDirPeriod()

	const entry = "Cleaning download directory - deleting files older than 4 days"
	const done = "Startup download directory cleanup finished"
	if strings.HasPrefix(entry, done) {
		t.Fatalf("completion entry must be distinct, never a prefix of the entry line (the startup stall hook matches it)")
	}
	out := buf.String()
	if !strings.Contains(out, entry) {
		t.Fatalf("entry log missing:\n%s", out)
	}
	if !strings.Contains(out, done) {
		t.Fatalf("fast cleanup emitted no completion entry; a stalled walk is indistinguishable from it:\n%s", out)
	}
}
