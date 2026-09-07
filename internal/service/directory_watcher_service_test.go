package service

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ensingerphilipp/premiumizearr-nova/pkg/premiumizeme"
	"github.com/ensingerphilipp/premiumizearr-nova/pkg/stringqueue"
	log "github.com/sirupsen/logrus"
)

func TestProcessUploadCycleQuotaBehavior(t *testing.T) {
	tests := []struct {
		name             string
		accountStatus    int
		accountResponse  string
		wantTransfer     bool
		wantAccountError bool
	}{
		{
			name:            "exhausted without booster points",
			accountStatus:   http.StatusOK,
			accountResponse: `{"status":"success","limit_used":1,"booster_points":0}`,
		},
		{
			name:            "quota available",
			accountStatus:   http.StatusOK,
			accountResponse: `{"status":"success","limit_used":0.99,"booster_points":0}`,
			wantTransfer:    true,
		},
		{
			name:            "booster points available",
			accountStatus:   http.StatusOK,
			accountResponse: `{"status":"success","limit_used":1,"booster_points":1}`,
			wantTransfer:    true,
		},
		{
			name:            "quota fields missing",
			accountStatus:   http.StatusOK,
			accountResponse: `{"status":"success"}`,
			wantTransfer:    true,
		},
		{
			name:             "account API error",
			accountStatus:    http.StatusInternalServerError,
			accountResponse:  `{"status":"error"}`,
			wantTransfer:     true,
			wantAccountError: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var accountRequests atomic.Int32
			var transferRequests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				switch request.URL.Path {
				case "/api/account/info":
					accountRequests.Add(1)
					response.WriteHeader(test.accountStatus)
					_, _ = response.Write([]byte(test.accountResponse))
				case "/api/transfer/create":
					transferRequests.Add(1)
					_, _ = response.Write([]byte(`{"status":"success"}`))
				default:
					http.NotFound(response, request)
				}
			}))
			defer server.Close()

			const apiKey = "secret-api-key"
			service, filePath := newQuotaTestService(t, server, apiKey, "request.magnet")
			var logs bytes.Buffer
			logger := log.StandardLogger()
			oldOutput := logger.Out
			logger.SetOutput(&logs)
			t.Cleanup(func() { logger.SetOutput(oldOutput) })

			service.processUploadCycle()

			if got := accountRequests.Load(); got != 1 {
				t.Fatalf("account requests = %d, want 1", got)
			}
			wantTransfers := int32(0)
			if test.wantTransfer {
				wantTransfers = 1
			}
			if got := transferRequests.Load(); got != wantTransfers {
				t.Fatalf("transfer requests = %d, want %d", got, wantTransfers)
			}

			_, statErr := os.Stat(filePath)
			if test.wantTransfer && !os.IsNotExist(statErr) {
				t.Fatalf("source file still exists after successful transfer: %v", statErr)
			}
			if !test.wantTransfer && statErr != nil {
				t.Fatalf("source file was changed while quota was exhausted: %v", statErr)
			}
			if !test.wantTransfer && service.Queue.Len() != 1 {
				t.Fatalf("queue length while quota was exhausted = %d, want 1", service.Queue.Len())
			}
			if test.wantAccountError && !strings.Contains(logs.String(), "Could not check Premiumize fair-use quota") {
				t.Fatalf("account error was not logged: %s", logs.String())
			}
			if strings.Contains(logs.String(), apiKey) {
				t.Fatalf("API key appeared in logs: %s", logs.String())
			}
		})
	}
}

func TestProcessUploadCycleResumesAndSuppressesRepeatedWarnings(t *testing.T) {
	var accountRequests atomic.Int32
	var transferRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/account/info":
			requestNumber := accountRequests.Add(1)
			if requestNumber <= 2 {
				_, _ = response.Write([]byte(`{"status":"success","limit_used":1,"booster_points":0}`))
				return
			}
			_, _ = response.Write([]byte(`{"status":"success","limit_used":0.5,"booster_points":0}`))
		case "/api/transfer/create":
			transferRequests.Add(1)
			_, _ = response.Write([]byte(`{"status":"success"}`))
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	service, filePath := newQuotaTestService(t, server, "test-key", "resume.magnet")
	var logs bytes.Buffer
	logger := log.StandardLogger()
	oldOutput := logger.Out
	logger.SetOutput(&logs)
	t.Cleanup(func() { logger.SetOutput(oldOutput) })

	service.processUploadCycle()
	service.processUploadCycle()
	if got := transferRequests.Load(); got != 0 {
		t.Fatalf("transfer requests while paused = %d, want 0", got)
	}
	if _, err := os.Stat(filePath); err != nil {
		t.Fatalf("source file was changed while paused: %v", err)
	}

	service.processUploadCycle()
	if got := transferRequests.Load(); got != 1 {
		t.Fatalf("transfer requests after resume = %d, want 1", got)
	}
	if !os.IsNotExist(fileExistsError(filePath)) {
		t.Fatal("source file still exists after processing resumed")
	}
	if got := strings.Count(logs.String(), "new blackhole submissions are paused"); got != 1 {
		t.Fatalf("pause warning count = %d, want 1; logs: %s", got, logs.String())
	}
	if got := strings.Count(logs.String(), "resuming blackhole submissions"); got != 1 {
		t.Fatalf("resume log count = %d, want 1; logs: %s", got, logs.String())
	}
}

func TestProcessUploadCycleChecksQuotaOnceForMultipleFiles(t *testing.T) {
	var accountRequests atomic.Int32
	var transferRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/account/info":
			accountRequests.Add(1)
			_, _ = response.Write([]byte(`{"status":"success","limit_used":0.25,"booster_points":0}`))
		case "/api/transfer/create":
			transferRequests.Add(1)
			_, _ = response.Write([]byte(`{"status":"success"}`))
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	service, _ := newQuotaTestService(t, server, "test-key", "first.magnet")
	secondFile := filepath.Join(t.TempDir(), "second.magnet")
	if err := os.WriteFile(secondFile, []byte("magnet:?xt=urn:btih:second"), 0o600); err != nil {
		t.Fatal(err)
	}
	service.Queue.Add(secondFile)

	service.processUploadCycle()

	if got := accountRequests.Load(); got != 1 {
		t.Fatalf("account requests = %d, want 1", got)
	}
	if got := transferRequests.Load(); got != 2 {
		t.Fatalf("transfer requests = %d, want 2", got)
	}
}

func TestProcessUploadCycleRemovesAlreadyUploadedSource(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/account/info":
			_, _ = response.Write([]byte(`{"status":"success","limit_used":0.25,"booster_points":0}`))
		case "/api/transfer/create":
			_, _ = response.Write([]byte(`{"status":"error","message":"You already added this job."}`))
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	service, filePath := newQuotaTestService(t, server, "test-key", "duplicate.magnet")
	service.processUploadCycle()

	if !os.IsNotExist(fileExistsError(filePath)) {
		t.Fatal("already-uploaded source file still exists")
	}
	if got := service.Queue.Len(); got != 0 {
		t.Fatalf("queue length after already-uploaded response = %d, want 0", got)
	}
}

func TestProcessUploadCycleRequeuesLimitRace(t *testing.T) {
	var transferRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/account/info":
			_, _ = response.Write([]byte(`{"status":"success","limit_used":0.99,"booster_points":0}`))
		case "/api/transfer/create":
			if transferRequests.Add(1) == 1 {
				_, _ = response.Write([]byte(`{"status":"error","message":"Limit of transfers reached!"}`))
				return
			}
			_, _ = response.Write([]byte(`{"status":"success"}`))
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	service, filePath := newQuotaTestService(t, server, "test-key", "limit-race.magnet")
	if got := service.processUploadCycle(); got != 0 {
		t.Fatalf("processed count after retryable limit response = %d, want 0", got)
	}
	if _, err := os.Stat(filePath); err != nil {
		t.Fatalf("retryable source file was changed: %v", err)
	}
	if got := service.Queue.Len(); got != 1 {
		t.Fatalf("queue length after retryable limit response = %d, want 1", got)
	}

	service.processUploadCycle()
	if got := transferRequests.Load(); got != 2 {
		t.Fatalf("transfer requests after retry = %d, want 2", got)
	}
	if !os.IsNotExist(fileExistsError(filePath)) {
		t.Fatal("source file still exists after successful retry")
	}
}

func newQuotaTestService(t *testing.T, server *httptest.Server, apiKey, fileName string) (*DirectoryWatcherService, string) {
	t.Helper()
	client := premiumizeme.NewPremiumizemeClient(apiKey)
	client.APIBaseURL = server.URL + "/api/"
	client.HTTPClient = server.Client()

	filePath := filepath.Join(t.TempDir(), fileName)
	if err := os.WriteFile(filePath, []byte("magnet:?xt=urn:btih:test"), 0o600); err != nil {
		t.Fatal(err)
	}

	service := NewDirectoryWatcherService()
	service.premiumizemeClient = &client
	service.Queue = stringqueue.NewStringQueue()
	service.Queue.Add(filePath)
	service.downloadsFolderID = "folder-id"
	return &service, filePath
}

func fileExistsError(filePath string) error {
	_, err := os.Stat(filePath)
	return err
}

func TestAccountCheckTransportErrorRedactsAPIKey(t *testing.T) {
	const apiKey = "transport secret/+"
	client := premiumizeme.NewPremiumizemeClient(apiKey)
	client.HTTPClient = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("request failed for %s", request.URL)
	})}

	_, err := client.GetAccountInfo()
	if err == nil {
		t.Fatal("GetAccountInfo() error = nil, want transport error")
	}
	if strings.Contains(err.Error(), apiKey) {
		t.Fatalf("API key appeared in account error: %s", err)
	}
	if strings.Contains(err.Error(), "transport+secret%2F%2B") {
		t.Fatalf("encoded API key appeared in account error: %s", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (roundTrip roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

func TestUploadBatchPreservesTransferPacing(t *testing.T) {
	var calls []time.Time
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/account/info" {
			w.Write([]byte(`{"status":"success"}`))
			return
		}
		calls = append(calls, time.Now())
		w.Write([]byte(`{"status":"success"}`))
	}))
	defer server.Close()
	svc, _ := newQuotaTestService(t, server, "test-key", "first.magnet")
	second := filepath.Join(t.TempDir(), "second.magnet")
	if err := os.WriteFile(second, []byte("magnet:?xt=urn:btih:second"), 0600); err != nil {
		t.Fatal(err)
	}
	svc.Queue.Add(second)
	svc.processUploadCycle()
	if len(calls) != 2 {
		t.Fatalf("transfers = %d", len(calls))
	}
	if gap := calls[1].Sub(calls[0]); gap < 2*time.Second {
		t.Fatalf("transfer gap = %s, want at least 2s", gap)
	}
}

func TestQuotaLookupFailurePreservesLastKnownState(t *testing.T) {
	response := `{"status":"success","limit_used":1,"booster_points":0}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(response)) }))
	defer server.Close()
	svc, _ := newQuotaTestService(t, server, "test-key", "state.magnet")
	var logs bytes.Buffer
	logger := log.StandardLogger()
	old := logger.Out
	logger.SetOutput(&logs)
	defer logger.SetOutput(old)
	if svc.submissionsAllowed() {
		t.Fatal("exhausted account allowed")
	}
	response = `{"status":"error","message":"temporarily unavailable"}`
	for range 2 {
		if !svc.submissionsAllowed() {
			t.Fatal("lookup error must fail open")
		}
	}
	if strings.Contains(svc.GetStatus(), "Paused") {
		t.Fatalf("stale status: %s", svc.GetStatus())
	}
	response = `{"status":"success","limit_used":1,"booster_points":0}`
	if svc.submissionsAllowed() {
		t.Fatal("exhausted account allowed after error")
	}
	if n := strings.Count(logs.String(), "new blackhole submissions are paused"); n != 1 {
		t.Fatalf("pause warnings = %d", n)
	}
	if n := strings.Count(logs.String(), "Could not check Premiumize fair-use quota"); n != 1 {
		t.Fatalf("lookup warnings = %d", n)
	}
	response = `{"status":"success","limit_used":0.5,"booster_points":0}`
	if !svc.submissionsAllowed() || svc.GetStatus() != "Okay" {
		t.Fatal("quota recovery failed")
	}
	if !strings.Contains(logs.String(), "resuming blackhole submissions") {
		t.Fatal("missing recovery log")
	}
}

func TestDuplicateQueueAdditionIsNotLoggedAsAdded(t *testing.T) {
	svc := NewDirectoryWatcherService()
	svc.Queue = stringqueue.NewStringQueue()
	var logs bytes.Buffer
	logger := log.StandardLogger()
	old := logger.Out
	logger.SetOutput(&logs)
	defer logger.SetOutput(old)
	svc.addFileToQueue("same.magnet")
	svc.addFileToQueue("same.magnet")
	if n := strings.Count(logs.String(), "added to Queue"); n != 1 {
		t.Fatalf("addition logs = %d", n)
	}
}
