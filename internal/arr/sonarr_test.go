package arr

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ensingerphilipp/premiumizearr-nova/internal/config"
	log "github.com/sirupsen/logrus"
	"golift.io/starr"
	"golift.io/starr/sonarr"
)

// TestHistoryContainsNotInHistoryLog verifies that the "not in history" trace
// line is formatted with both the arr name and the looked-up file name.
// The Sonarr history endpoint is faked with httptest (no network).
func TestHistoryContainsNotInHistoryLog(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/history" {
			t.Errorf("unexpected request path %q", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"records":[],"totalRecords":0}`)
	}))
	defer srv.Close()

	var buf bytes.Buffer
	prevOut := log.StandardLogger().Out
	prevLevel := log.GetLevel()
	log.SetOutput(&buf)
	log.SetLevel(log.TraceLevel)
	t.Cleanup(func() {
		log.SetLevel(prevLevel)
		log.SetOutput(prevOut)
	})

	a := &SonarrArr{
		Name:       "TestSonarr",
		Client:     sonarr.New(starr.New("test-api-key", srv.URL, 0)),
		History:    nil,
		LastUpdate: time.Now(),
		Config:     &config.Config{ArrHistoryUpdateIntervalSeconds: 20},
	}

	const name = "missing.show.s01e01.720p"
	id, found := a.HistoryContains(name)
	if found {
		t.Fatalf("HistoryContains(%q) = found, want not found", name)
	}
	if id != -1 {
		t.Fatalf("HistoryContains(%q) id = %d, want -1", name, id)
	}

	out := buf.String()
	if strings.Contains(out, "%!s(MISSING)") {
		t.Errorf("trace log contains a missing-argument placeholder:\n%s", out)
	}
	if !strings.Contains(out, "Sonarr [TestSonarr]: "+name+" Not in History") {
		t.Errorf("expected formatted not-in-history trace line with arr name and file name:\n%s", out)
	}
}
