package arr

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ensingerphilipp/premiumizearr-nova/internal/config"
	"github.com/ensingerphilipp/premiumizearr-nova/pkg/premiumizeme"
	log "github.com/sirupsen/logrus"
	"golift.io/starr"
	"golift.io/starr/lidarr"
	"golift.io/starr/radarr"
	"golift.io/starr/sonarr"
)

// fakeHistoryRecord is the JSON shape of a *arr history record served by the
// httptest fakes in this package.
type fakeHistoryRecord struct {
	ID          int64  `json:"id"`
	EventType   string `json:"eventType"`
	SourceTitle string `json:"sourceTitle"`
	Message     string `json:"message"`
	Date        string `json:"date,omitempty"`
}

// fakeArrServer is an httptest fake for the *arr history and
// history-failed endpoints (no network). It records every id the client
// reports as failed.
type fakeArrServer struct {
	*httptest.Server
	apiVersion string
	records    []fakeHistoryRecord
	failedIDs  []int64
}

// newFakeArrServer serves GET /api/<version>/history with the given records
// (oldest first, as the *arr APIs return them) and records the ids of POSTs
// that mark history items as failed.
func newFakeArrServer(t *testing.T, apiVersion string, records []fakeHistoryRecord) *fakeArrServer {
	t.Helper()

	fake := &fakeArrServer{apiVersion: apiVersion, records: records}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/"+apiVersion+"/history", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("unexpected %s on /api/%s/history", r.Method, apiVersion)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"records":      fake.records,
			"totalRecords": len(fake.records),
		})
	})

	failPath := "/api/" + apiVersion + "/history/failed"
	// Sonarr and Radarr (API v3) use POST /history/failed/{id}; Lidarr (API
	// v1) POSTs the id in the request body.
	idInPath := apiVersion == "v3"
	if idInPath {
		failPath += "/"
	}
	mux.HandleFunc(failPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("unexpected %s on %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		id := fake.parseFailedID(t, r, idInPath)
		fake.failedIDs = append(fake.failedIDs, id)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{}`)
	})

	fake.Server = httptest.NewServer(mux)
	t.Cleanup(fake.Close)

	return fake
}

// parseFailedID extracts the failed history id from the request: from the
// URL path (Sonarr/Radarr) or the form-encoded body (Lidarr).
func (fake *fakeArrServer) parseFailedID(t *testing.T, r *http.Request, idInPath bool) int64 {
	t.Helper()

	var raw string
	if idInPath {
		raw = path.Base(r.URL.Path)
	} else {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read fail request body: %v", err)
			return 0
		}
		values, err := url.ParseQuery(string(body))
		if err != nil {
			t.Errorf("parse fail request body %q: %v", body, err)
			return 0
		}
		raw = values.Get("id")
	}

	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		t.Errorf("invalid history id %q in fail request: %v", raw, err)
		return 0
	}

	return id
}

// newArrFunc constructs an IArr backed by an httptest fake server.
type newArrFunc func(serverURL string) IArr

func newTestSonarrArr(serverURL string) IArr {
	return &SonarrArr{
		Name:       "TestSonarr",
		Client:     sonarr.New(starr.New("test-api-key", serverURL, 0)),
		History:    nil,
		LastUpdate: time.Now(),
		Config:     &config.Config{ArrHistoryUpdateIntervalSeconds: 20},
	}
}

func newTestRadarrArr(serverURL string) IArr {
	return &RadarrArr{
		Name:       "TestRadarr",
		Client:     radarr.New(starr.New("test-api-key", serverURL, 0)),
		History:    nil,
		LastUpdate: time.Now(),
		Config:     &config.Config{ArrHistoryUpdateIntervalSeconds: 20},
	}
}

func newTestLidarrArr(serverURL string) IArr {
	return &LidarrArr{
		Name:       "TestLidarr",
		Client:     lidarr.New(starr.New("test-api-key", serverURL, 0)),
		History:    nil,
		LastUpdate: time.Now(),
		Config:     &config.Config{ArrHistoryUpdateIntervalSeconds: 20},
	}
}

// runErrorTransferReportingTest is the issue #22 regression scenario, shared
// by the Sonarr, Radarr and Lidarr wrappers: an older non-grabbed history
// record (a previous download failure for the same release) and a newer
// grabbed record share the release name of an errored transfer. The *arr
// API returns history oldest first, so the older record is met first.
// HistoryContains must resolve the grabbed record, and HandleErrorTransfer
// must report the failure to the *arr for that record.
func runErrorTransferReportingTest(t *testing.T, newArr newArrFunc, apiVersion string) {
	t.Helper()

	const (
		releaseName  = "Show.S01E01.720p.WEB.x264-GRP.mkv"
		transferName = releaseName + ".nzb"
		failedID     = int64(101)
		grabbedID    = int64(102)
	)

	fake := newFakeArrServer(t, apiVersion, []fakeHistoryRecord{
		{ID: failedID, EventType: "downloadFailed", SourceTitle: releaseName, Message: "Repair failed, not enough repair blocks (28 short)", Date: "2026-08-01T12:00:00.0000000Z"},
		{ID: grabbedID, EventType: "grabbed", SourceTitle: releaseName, Message: "Grabbed", Date: "2026-08-02T12:00:00.0000000Z"},
	})

	a := newArr(fake.URL)

	id, found := a.HistoryContains(transferName)
	if !found {
		t.Fatalf("HistoryContains(%q) = not found, want found", transferName)
	}
	if id != grabbedID {
		t.Fatalf("HistoryContains(%q) id = %d, want %d (the newest grabbed record, not the older downloadFailed record)", transferName, id, grabbedID)
	}

	transfer := premiumizeme.Transfer{
		ID:      "transfer-123",
		Name:    transferName,
		Message: "Repair failed, not enough repair blocks (28 short)",
		Status:  "error",
	}
	// An empty API key makes DeleteTransfer fail deterministically without
	// any network call; reaching the delete step proves the *arr was
	// notified first.
	pm := premiumizeme.NewPremiumizemeClient("")
	err := a.HandleErrorTransfer(&transfer, id, &pm)
	if err == nil || !strings.Contains(err.Error(), "failed to delete transfer from premiumize.me") {
		t.Fatalf("HandleErrorTransfer error = %v, want the premiumize.me delete step to be reached (only possible after the *arr was told about the failure)", err)
	}
	if len(fake.failedIDs) != 1 || fake.failedIDs[0] != grabbedID {
		t.Fatalf("expected the *arr to be told that history record %d failed, got failed calls: %v", grabbedID, fake.failedIDs)
	}
}

// runOnlyNonGrabbedRecordsTest verifies that a transfer name that only
// matches non-grabbed history records (an old download failure for the same
// release) is reported as not in history, so the caller does not delete the
// transfer without notifying the *arr. If wantNotInHistoryLog is non-empty,
// the trace line must be present in the log output.
func runOnlyNonGrabbedRecordsTest(t *testing.T, newArr newArrFunc, apiVersion string, wantNotInHistoryLog string) {
	t.Helper()

	const (
		releaseName  = "Show.S01E01.720p.WEB.x264-GRP.mkv"
		transferName = releaseName + ".nzb"
	)

	var logBuf *bytes.Buffer
	if wantNotInHistoryLog != "" {
		logBuf = &bytes.Buffer{}
		prevOut := log.StandardLogger().Out
		prevLevel := log.GetLevel()
		log.SetOutput(logBuf)
		log.SetLevel(log.TraceLevel)
		t.Cleanup(func() {
			log.SetLevel(prevLevel)
			log.SetOutput(prevOut)
		})
	}

	fake := newFakeArrServer(t, apiVersion, []fakeHistoryRecord{
		{ID: 101, EventType: "downloadFailed", SourceTitle: releaseName, Message: "Repair failed, not enough repair blocks (28 short)", Date: "2026-08-01T12:00:00.0000000Z"},
	})

	a := newArr(fake.URL)
	id, found := a.HistoryContains(transferName)
	if found {
		t.Fatalf("HistoryContains(%q) = found (id %d), want not found: only a non-grabbed record matches", transferName, id)
	}
	if id != -1 {
		t.Fatalf("HistoryContains(%q) id = %d, want -1", transferName, id)
	}
	if len(fake.failedIDs) != 0 {
		t.Fatalf("expected no failed-history calls, got %v", fake.failedIDs)
	}
	if wantNotInHistoryLog != "" && !strings.Contains(logBuf.String(), wantNotInHistoryLog) {
		t.Errorf("expected formatted not-in-history trace line %q in log output:\n%s", wantNotInHistoryLog, logBuf.String())
	}
}
