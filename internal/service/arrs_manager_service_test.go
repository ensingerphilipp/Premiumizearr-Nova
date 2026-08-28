package service

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ensingerphilipp/premiumizearr-nova/internal/config"
	log "github.com/sirupsen/logrus"
)

// TestArrsManagerServiceUnknownArrType verifies that an arr with an unknown
// type is rejected with a formatted log line (no literal directives) and is
// not added to the manager. Start() with only unknown types performs no
// network access, so the test is deterministic.
func TestArrsManagerServiceUnknownArrType(t *testing.T) {
	var buf bytes.Buffer
	prevOut := log.StandardLogger().Out
	prevLevel := log.GetLevel()
	log.SetOutput(&buf)
	log.SetLevel(log.InfoLevel)
	t.Cleanup(func() {
		log.SetLevel(prevLevel)
		log.SetOutput(prevOut)
	})

	am := ArrsManagerService{}.New()
	am.Init(&config.Config{
		Arrs: []config.ArrConfig{
			{Name: "WeirdArr", URL: "http://127.0.0.1:1", APIKey: "test", Type: "SomethingElse"},
		},
	})
	am.Start()

	if got := len(am.GetArrs()); got != 0 {
		t.Fatalf("GetArrs() len = %d, want 0 (unknown type must not be added)", got)
	}

	out := buf.String()
	if strings.Contains(out, "%s") {
		t.Errorf("unknown-arr log line contains an unformatted directive:\n%s", out)
	}
	if !strings.Contains(out, "Unknown arr type: SomethingElse, not adding Arr WeirdArr") {
		t.Errorf("expected formatted unknown-arr log line:\n%s", out)
	}
}
