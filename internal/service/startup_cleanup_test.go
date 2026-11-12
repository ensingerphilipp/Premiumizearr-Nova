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
	old := filepath.Join(dir, "old.mkv")
	if err := os.WriteFile(old, []byte("media"), 0644); err != nil {
		t.Fatal(err)
	}
	modified := time.Now().AddDate(0, 0, -5)
	if err := os.Chtimes(old, modified, modified); err != nil {
		t.Fatal(err)
	}
	manager := TransferManagerService{}.New()
	manager.Init(nil, nil, &config.Config{DownloadsDirectory: dir})
	manager.CleanUpDownloadDirPeriod()

	const entry = "Cleaning download directory - deleting files older than 4 days"
	const done = "Startup download directory cleanup finished"
	out := buf.String()
	if !strings.Contains(out, entry) {
		t.Fatalf("entry log missing:\n%s", out)
	}
	// Assert against the captured entry line, not the const pair (which
	// cannot see drift in the service strings): if the completion text
	// ever appears inside the entry line, a prose matcher on the entry
	// could also match a completion. The startup stall hook itself
	// matches the marker field, not prose (cmd/premiumizearrd/startup_test.go).
	entryIdx := strings.Index(out, entry)
	entryLine := out[strings.LastIndexByte(out[:entryIdx], '\n')+1:]
	if nl := strings.IndexByte(entryLine, '\n'); nl >= 0 {
		entryLine = entryLine[:nl]
	}
	if strings.Contains(entryLine, done) {
		t.Fatalf("completion text appears inside the entry line:\n%s", entryLine)
	}
	doneIdx := strings.Index(out, done)
	if doneIdx < 0 {
		t.Fatalf("fast cleanup emitted no completion entry; a stalled walk is indistinguishable from it:\n%s", out)
	}
	// Pin the position, not just the presence: the completion line must come
	// after every walk line, so a mutation that moves it in front of a
	// stalled "Deleting …" entry is caught. LastIndex is -1 when no file
	// was deleted, in which case any position is after the walk output.
	lastDeleting := strings.LastIndex(out, "Deleting ")
	if doneIdx <= lastDeleting {
		t.Fatalf("completion line at offset %d does not come AFTER the walk output (last Deleting at %d):\n%s", doneIdx, lastDeleting, out)
	}
}
