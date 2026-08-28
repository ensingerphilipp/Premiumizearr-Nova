package utils

import (
	"bytes"
	"strings"
	"testing"

	log "github.com/sirupsen/logrus"
)

// captureLogs redirects the global logrus logger to a buffer for the duration
// of the test and restores the previous output and level afterwards.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prevOut := log.StandardLogger().Out
	prevLevel := log.GetLevel()
	log.SetOutput(&buf)
	log.SetLevel(log.InfoLevel)
	t.Cleanup(func() {
		log.SetLevel(prevLevel)
		log.SetOutput(prevOut)
	})
	return &buf
}

func TestIsDirectoryWriteable(t *testing.T) {
	buf := captureLogs(t)

	dir := t.TempDir()
	if !IsDirectoryWriteable(dir) {
		t.Fatalf("IsDirectoryWriteable(%q) = false, want true", dir)
	}

	missing := dir + "/does-not-exist"
	if IsDirectoryWriteable(missing) {
		t.Fatalf("IsDirectoryWriteable(%q) = true, want false", missing)
	}

	out := buf.String()
	if !strings.Contains(out, missing) {
		t.Errorf("log for the missing directory does not contain the path %q:\n%s", missing, out)
	}
	if strings.Contains(out, "%!") {
		t.Errorf("log contains a format placeholder artifact:\n%s", out)
	}
}
