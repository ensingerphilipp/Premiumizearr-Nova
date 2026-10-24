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

	"github.com/ensingerphilipp/premiumizearr-nova/internal/config"
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
		maxTransfers     int
		transfersBody    string
		transfersStatus  int
		wantCapPause     bool
		wantCountError   bool
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
		{
			name:            "transfer cap available",
			accountStatus:   http.StatusOK,
			accountResponse: `{"status":"success","limit_used":0.5,"booster_points":0}`,
			maxTransfers:    2,
			transfersBody:   `{"status":"success","transfers":[{"id":"1","status":"running"}]}`,
			wantTransfer:    true,
		},
		{
			name:            "transfer cap exhausted",
			accountStatus:   http.StatusOK,
			accountResponse: `{"status":"success","limit_used":0.5,"booster_points":0}`,
			maxTransfers:    1,
			transfersBody:   `{"status":"success","transfers":[{"id":"1","status":"running"}]}`,
			wantCapPause:    true,
		},
		{
			name:            "transfer cap check error",
			accountStatus:   http.StatusOK,
			accountResponse: `{"status":"success","limit_used":0.5,"booster_points":0}`,
			maxTransfers:    2,
			transfersStatus: http.StatusInternalServerError,
			wantCountError:  true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var accountRequests atomic.Int32
			var transfersRequests atomic.Int32
			var transferRequests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				switch request.URL.Path {
				case "/api/account/info":
					accountRequests.Add(1)
					response.WriteHeader(test.accountStatus)
					_, _ = response.Write([]byte(test.accountResponse))
				case "/api/transfer/list":
					transfersRequests.Add(1)
					if test.transfersStatus != 0 {
						response.WriteHeader(test.transfersStatus)
						_, _ = response.Write([]byte(`{"status":"error"}`))
						return
					}
					body := test.transfersBody
					if body == "" {
						body = `{"status":"success","transfers":[]}`
					}
					_, _ = response.Write([]byte(body))
				case "/api/transfer/create":
					transferRequests.Add(1)
					_, _ = response.Write([]byte(`{"status":"success"}`))
				default:
					http.NotFound(response, request)
				}
			}))
			defer server.Close()

			const apiKey = "secret-api-key"
			service, filePath := newQuotaTestService(t, server, apiKey, "request.magnet", test.maxTransfers)
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
			wantTransfersListChecks := int32(0)
			if test.maxTransfers > 0 {
				wantTransfersListChecks = 1
			}
			if got := transfersRequests.Load(); got != wantTransfersListChecks {
				t.Fatalf("transfer list checks = %d, want %d", got, wantTransfersListChecks)
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
			if test.wantCapPause && !strings.Contains(logs.String(), "pausing blackhole submissions at configured limit") {
				t.Fatalf("cap pause warning was not logged: %s", logs.String())
			}
			if test.wantCountError && !strings.Contains(logs.String(), "Could not check active Premiumize transfers; blackhole submissions are paused") {
				t.Fatalf("transfer count pause warning was not logged: %s", logs.String())
			}
			if strings.Contains(logs.String(), apiKey) {
				t.Fatalf("API key appeared in logs: %s", logs.String())
			}
		})
	}
}

// TestProcessUploadCycleCapCountErrorRecovers is the two-phase probe for
// review finding R1-9: a transfer-count check failure pauses blackhole
// submissions, and the next successful check must clear the paused status and
// resume. Dropping the activeTransferCountCheckFailed reset re-fires the
// recovery log and fails this test.
func TestProcessUploadCycleCapCountErrorRecovers(t *testing.T) {
	var listRequests atomic.Int32
	var transferRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/account/info":
			_, _ = response.Write([]byte(`{"status":"success","limit_used":0.5,"booster_points":0}`))
		case "/api/transfer/list":
			if listRequests.Add(1) == 1 {
				response.WriteHeader(http.StatusInternalServerError)
				_, _ = response.Write([]byte(`{"status":"error"}`))
				return
			}
			_, _ = response.Write([]byte(`{"status":"success","transfers":[{"id":"1","status":"running"}]}`))
		case "/api/transfer/create":
			transferRequests.Add(1)
			_, _ = response.Write([]byte(`{"status":"success"}`))
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	service, filePath := newQuotaTestService(t, server, "test-key", "recover.magnet", 2)
	var logs bytes.Buffer
	logger := log.StandardLogger()
	oldOutput := logger.Out
	logger.SetOutput(&logs)
	t.Cleanup(func() { logger.SetOutput(oldOutput) })

	if got := service.processUploadCycle(); got != 0 {
		t.Fatalf("processed count while the count check fails = %d, want 0", got)
	}
	if got := service.GetStatus(); got != "Paused: Premiumize active transfer count unavailable" {
		t.Fatalf("status while paused = %q, want the paused message", got)
	}
	if got := transferRequests.Load(); got != 0 {
		t.Fatalf("transfer requests while paused = %d, want 0", got)
	}
	if _, err := os.Stat(filePath); err != nil {
		t.Fatalf("source file was changed while paused: %v", err)
	}

	service.processUploadCycle()
	if got := transferRequests.Load(); got != 1 {
		t.Fatalf("transfer requests after recovery = %d, want 1", got)
	}
	if !os.IsNotExist(fileExistsError(filePath)) {
		t.Fatal("source file still exists after recovery")
	}
	if got := service.GetStatus(); got != "Okay" {
		t.Fatalf("status after recovery = %q, want Okay", got)
	}
	if n := strings.Count(logs.String(), "Could not check active Premiumize transfers; blackhole submissions are paused"); n != 1 {
		t.Fatalf("pause warning count = %d, want 1; logs: %s", n, logs.String())
	}
	if n := strings.Count(logs.String(), "Premiumize active transfer count is available again; resuming transfer checks"); n != 1 {
		t.Fatalf("recovery log count = %d, want 1; logs: %s", n, logs.String())
	}
}

// TestProcessUploadCycleCapBlockedRecoversBelowCap covers the
// wasBlocked/blocked transition pair of updateActiveTransferLimitStatus: a
// cap-blocked cycle pauses submissions, and the next successful check below
// the cap must log the resume exactly once and clear the paused status.
func TestProcessUploadCycleCapBlockedRecoversBelowCap(t *testing.T) {
	var listRequests atomic.Int32
	var transferRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/account/info":
			_, _ = response.Write([]byte(`{"status":"success","limit_used":0.5,"booster_points":0}`))
		case "/api/transfer/list":
			if listRequests.Add(1) == 1 {
				_, _ = response.Write([]byte(`{"status":"success","transfers":[{"id":"1","status":"running"}]}`))
				return
			}
			_, _ = response.Write([]byte(`{"status":"success","transfers":[]}`))
		case "/api/transfer/create":
			transferRequests.Add(1)
			_, _ = response.Write([]byte(`{"status":"success"}`))
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	service, filePath := newQuotaTestService(t, server, "test-key", "blocked.magnet", 1)
	var logs bytes.Buffer
	logger := log.StandardLogger()
	oldOutput := logger.Out
	logger.SetOutput(&logs)
	t.Cleanup(func() { logger.SetOutput(oldOutput) })

	if got := service.processUploadCycle(); got != 0 {
		t.Fatalf("processed count while blocked = %d, want 0", got)
	}
	if got := service.GetStatus(); got != "Paused: 1 active Premiumize transfers (limit 1)" {
		t.Fatalf("status while blocked = %q, want the cap-paused message", got)
	}
	if _, err := os.Stat(filePath); err != nil {
		t.Fatalf("source file was changed while blocked: %v", err)
	}

	service.processUploadCycle()
	if got := transferRequests.Load(); got != 1 {
		t.Fatalf("transfer requests after resume = %d, want 1", got)
	}
	if !os.IsNotExist(fileExistsError(filePath)) {
		t.Fatal("source file still exists after resume")
	}
	if got := service.GetStatus(); got != "Okay" {
		t.Fatalf("status after resume = %q, want Okay", got)
	}
	if n := strings.Count(logs.String(), "pausing blackhole submissions at configured limit"); n != 1 {
		t.Fatalf("cap pause warning count = %d, want 1; logs: %s", n, logs.String())
	}
	if n := strings.Count(logs.String(), "resuming blackhole submissions"); n != 1 {
		t.Fatalf("resume log count = %d, want 1; logs: %s", n, logs.String())
	}
}

// TestProcessUploadCycleCapPauseNotRefiredAfterCountError is the probe for
// review finding R1-16: a cap pause interrupted by a transient
// transfer-count check failure must keep its block state, so the pause
// warning fires exactly once per pause episode — re-firing it on every
// failure/recovery at the cap would page anything keyed on that line with
// no state change.
func TestProcessUploadCycleCapPauseNotRefiredAfterCountError(t *testing.T) {
	var listRequests atomic.Int32
	var transferRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/account/info":
			_, _ = response.Write([]byte(`{"status":"success","limit_used":0.5,"booster_points":0}`))
		case "/api/transfer/list":
			if listRequests.Add(1) == 2 {
				response.WriteHeader(http.StatusInternalServerError)
				_, _ = response.Write([]byte(`{"status":"error"}`))
				return
			}
			_, _ = response.Write([]byte(`{"status":"success","transfers":[{"id":"1","status":"running"}]}`))
		case "/api/transfer/create":
			transferRequests.Add(1)
			_, _ = response.Write([]byte(`{"status":"success"}`))
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	service, filePath := newQuotaTestService(t, server, "test-key", "still-paused.magnet", 1)
	var logs bytes.Buffer
	logger := log.StandardLogger()
	oldOutput := logger.Out
	logger.SetOutput(&logs)
	t.Cleanup(func() { logger.SetOutput(oldOutput) })

	service.processUploadCycle()
	service.processUploadCycle()
	service.processUploadCycle()

	if got := transferRequests.Load(); got != 0 {
		t.Fatalf("transfer requests = %d, want 0 (still at the cap)", got)
	}
	if _, err := os.Stat(filePath); err != nil {
		t.Fatalf("source file was changed while paused: %v", err)
	}
	if got := service.GetStatus(); got != "Paused: 1 active Premiumize transfers (limit 1)" {
		t.Fatalf("status after recovery at the cap = %q, want the cap-paused message", got)
	}
	if n := strings.Count(logs.String(), "pausing blackhole submissions at configured limit"); n != 1 {
		t.Fatalf("cap pause warning count = %d, want 1 (the pause never lifted); logs: %s", n, logs.String())
	}
	if n := strings.Count(logs.String(), "Could not check active Premiumize transfers; blackhole submissions are paused"); n != 1 {
		t.Fatalf("count pause warning count = %d, want 1; logs: %s", n, logs.String())
	}
	if n := strings.Count(logs.String(), "Premiumize active transfer count is available again; resuming transfer checks"); n != 1 {
		t.Fatalf("recovery log count = %d, want 1; logs: %s", n, logs.String())
	}
}

// TestProcessUploadCycleCapCheckPreservesQuotaUnknownStatus is the probe for
// review finding R1-1: with the cap enabled, a failed /api/account/info
// lookup fails open and sets the honest quota-unknown status; the cycle then
// runs a healthy cap check below the limit, and the transfer create fails
// with a non-limit error. The cap-status update must not clobber the
// quota-unknown status with "Okay" while the fair-use quota is still
// unknown — pre-PR the same sequence ended on the quota-unknown status.
// Once the account lookup recovers, the status returns to "Okay".
func TestProcessUploadCycleCapCheckPreservesQuotaUnknownStatus(t *testing.T) {
	var accountRequests atomic.Int32
	var transferRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/account/info":
			if accountRequests.Add(1) == 1 {
				response.WriteHeader(http.StatusInternalServerError)
				_, _ = response.Write([]byte(`{"status":"error"}`))
				return
			}
			_, _ = response.Write([]byte(`{"status":"success","limit_used":0.5,"booster_points":0}`))
		case "/api/transfer/list":
			_, _ = response.Write([]byte(`{"status":"success","transfers":[{"id":"1","status":"running"}]}`))
		case "/api/transfer/create":
			transferRequests.Add(1)
			response.WriteHeader(http.StatusInternalServerError)
			_, _ = response.Write([]byte(`{"status":"error"}`))
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	service, filePath := newQuotaTestService(t, server, "test-key", "degraded.magnet", 2)
	recoveryPath := filepath.Join(t.TempDir(), "recovery.magnet")
	if err := os.WriteFile(recoveryPath, []byte("magnet:?xt=urn:btih:recovery"), 0o600); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	logger := log.StandardLogger()
	oldOutput := logger.Out
	logger.SetOutput(&logs)
	t.Cleanup(func() { logger.SetOutput(oldOutput) })

	if got := service.processUploadCycle(); got != 1 {
		t.Fatalf("processed count in the degraded window = %d, want 1", got)
	}
	if got := service.GetStatus(); got != "Premiumize fair-use quota unknown; continuing submissions" {
		t.Fatalf("status with quota unknown and a healthy cap check = %q, want the quota-unknown status", got)
	}
	if got := transferRequests.Load(); got != 1 {
		t.Fatalf("transfer requests in the degraded window = %d, want 1", got)
	}
	if _, err := os.Stat(filePath); err != nil {
		t.Fatalf("source file was removed during the degraded window: %v", err)
	}

	service.Queue.Add(recoveryPath)
	if got := service.processUploadCycle(); got != 1 {
		t.Fatalf("processed count after the account lookup recovers = %d, want 1", got)
	}
	if got := service.GetStatus(); got != "Okay" {
		t.Fatalf("status after the account lookup recovers = %q, want Okay", got)
	}
	if got := transferRequests.Load(); got != 2 {
		t.Fatalf("transfer requests after recovery = %d, want 2", got)
	}
	if _, err := os.Stat(recoveryPath); err != nil {
		t.Fatalf("recovery source file was removed while the create still fails: %v", err)
	}
	if n := strings.Count(logs.String(), "Could not check Premiumize fair-use quota; continuing with existing submission behavior"); n != 1 {
		t.Fatalf("quota-unknown warning count = %d, want 1; logs: %s", n, logs.String())
	}
	if n := strings.Count(logs.String(), "Error creating transfer:"); n != 2 {
		t.Fatalf("create error count = %d, want 2; logs: %s", n, logs.String())
	}
}

// TestActivePremiumizeTransferCountCountsOnlyQueuedAndRunning is the
// acceptance test for review finding R1-13: the counter must count
// case/whitespace-insensitive "queued" and "running" statuses and exclude
// inactive and empty statuses. Mutating the switch to "running" only makes
// this fail (the fixture contains four queued/running variants).
func TestActivePremiumizeTransferCountCountsOnlyQueuedAndRunning(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/transfer/list" {
			http.NotFound(response, request)
			return
		}
		_, _ = response.Write([]byte(`{"status":"success","transfers":[` +
			`{"id":"1","status":"queued"},` +
			`{"id":"2","status":"running"},` +
			`{"id":"3","status":"QUEUED"},` +
			`{"id":"4","status":" running "},` +
			`{"id":"5","status":"done"},` +
			`{"id":"6"}` +
			`]}`))
	}))
	defer server.Close()

	client := premiumizeme.NewPremiumizemeClient("test-key")
	client.APIBaseURL = server.URL + "/api/"
	client.HTTPClient = server.Client()

	service := NewDirectoryWatcherService()
	service.Init(&client, &config.Config{})

	got, err := service.activePremiumizeTransferCount()
	if err != nil {
		t.Fatalf("activePremiumizeTransferCount() error = %v, want nil", err)
	}
	if got != 4 {
		t.Fatalf("active count = %d, want 4 (queued/running variants only; inactive and empty statuses excluded)", got)
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

	service, filePath := newQuotaTestService(t, server, "test-key", "resume.magnet", 0)
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

	service, _ := newQuotaTestService(t, server, "test-key", "first.magnet", 0)
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

	service, filePath := newQuotaTestService(t, server, "test-key", "duplicate.magnet", 0)
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

	service, filePath := newQuotaTestService(t, server, "test-key", "limit-race.magnet", 0)
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

func newQuotaTestService(t *testing.T, server *httptest.Server, apiKey, fileName string, maxActiveTransfers int) (*DirectoryWatcherService, string) {
	t.Helper()
	client := premiumizeme.NewPremiumizemeClient(apiKey)
	client.APIBaseURL = server.URL + "/api/"
	client.HTTPClient = server.Client()

	filePath := filepath.Join(t.TempDir(), fileName)
	if err := os.WriteFile(filePath, []byte("magnet:?xt=urn:btih:test"), 0o600); err != nil {
		t.Fatal(err)
	}

	service := NewDirectoryWatcherService()
	service.Init(&client, &config.Config{MaxActivePremiumizeTransfers: maxActiveTransfers})
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
	svc, _ := newQuotaTestService(t, server, "test-key", "first.magnet", 0)
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
	svc, _ := newQuotaTestService(t, server, "test-key", "state.magnet", 0)
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

// TestConfigUpdatedCallbackLogsCapChange is the probe for review finding
// R1-8: a MaxActivePremiumizeTransfers change reaching the running service
// must leave a log trace with the old and new values. The callback branch is
// log-only — it reads its arguments and never reassigns dw.config, so the
// cycle picks the new value up on its next live read.
func TestConfigUpdatedCallbackLogsCapChange(t *testing.T) {
	client := premiumizeme.NewPremiumizemeClient("test-key")
	svc := NewDirectoryWatcherService()
	svc.Init(&client, &config.Config{MaxActivePremiumizeTransfers: 20})
	var logs bytes.Buffer
	logger := log.StandardLogger()
	old := logger.Out
	logger.SetOutput(&logs)
	defer logger.SetOutput(old)

	svc.ConfigUpdatedCallback(config.Config{MaxActivePremiumizeTransfers: 20}, config.Config{MaxActivePremiumizeTransfers: 5})
	if n := strings.Count(logs.String(), "MaxActivePremiumizeTransfers changed from 20 to 5"); n != 1 {
		t.Fatalf("cap change logs = %d, want 1; logs: %s", n, logs.String())
	}
	logs.Reset()

	svc.ConfigUpdatedCallback(config.Config{MaxActivePremiumizeTransfers: 20}, config.Config{MaxActivePremiumizeTransfers: 20})
	if n := strings.Count(logs.String(), "MaxActivePremiumizeTransfers changed"); n != 0 {
		t.Fatalf("unchanged cap produced a change log; logs: %s", logs.String())
	}
}

// TestConfigUpdatedCallbackClearsCapStateOnDisable drives a cap-blocked
// state and disables the cap through the callback, asserting the callback
// clears the cap-derived pause state instead of leaving it stale: the
// blocked flag and status must return to the normal state ("Okay", or the
// quota branch's quota-unknown status when the quota lookup is failing),
// so the UI does not stay pinned at Paused and the one-per-episode pause
// warning fires again when the cap is re-enabled at the limit.
func TestConfigUpdatedCallbackClearsCapStateOnDisable(t *testing.T) {
	t.Run("disabling the cap clears the blocked state", func(t *testing.T) {
		client := premiumizeme.NewPremiumizemeClient("test-key")
		svc := NewDirectoryWatcherService()
		svc.Init(&client, &config.Config{MaxActivePremiumizeTransfers: 2})

		svc.updateActiveTransferLimitStatus(2, 2)
		if !svc.activeTransferLimitBlocked {
			t.Fatalf("drive: expected the cap to be blocked, status %q", svc.GetStatus())
		}

		svc.ConfigUpdatedCallback(config.Config{MaxActivePremiumizeTransfers: 2}, config.Config{MaxActivePremiumizeTransfers: 0})

		if svc.activeTransferLimitBlocked {
			t.Fatalf("after disabling the cap, activeTransferLimitBlocked = true, want false (status %q)", svc.GetStatus())
		}
		if svc.activeTransferCountCheckFailed {
			t.Fatalf("after disabling the cap, activeTransferCountCheckFailed = true, want false")
		}
		if got := svc.GetStatus(); got != "Okay" {
			t.Fatalf("after disabling the cap, status = %q, want Okay", got)
		}
	})

	t.Run("disabling the cap keeps the quota-unknown status", func(t *testing.T) {
		client := premiumizeme.NewPremiumizemeClient("test-key")
		svc := NewDirectoryWatcherService()
		svc.Init(&client, &config.Config{MaxActivePremiumizeTransfers: 2})
		svc.quotaCheckFailed = true

		svc.updateActiveTransferLimitStatus(2, 2)
		if !svc.activeTransferLimitBlocked {
			t.Fatalf("drive: expected the cap to be blocked, status %q", svc.GetStatus())
		}

		svc.ConfigUpdatedCallback(config.Config{MaxActivePremiumizeTransfers: 2}, config.Config{MaxActivePremiumizeTransfers: 0})

		if svc.activeTransferLimitBlocked {
			t.Fatalf("after disabling the cap, activeTransferLimitBlocked = true, want false (status %q)", svc.GetStatus())
		}
		if svc.activeTransferCountCheckFailed {
			t.Fatalf("after disabling the cap, activeTransferCountCheckFailed = true, want false")
		}
		want := "Premiumize fair-use quota unknown; continuing submissions"
		if got := svc.GetStatus(); got != want {
			t.Fatalf("after disabling the cap, status = %q, want %q", got, want)
		}
	})
}

// TestProcessUploadCycleFetchesTransferListOncePerCycle is the probe for
// review finding R1-7: the cycle fetches /transfer/list once and decides
// later iterations from the local reservation of its own submissions, so a
// deep blackhole queue does not turn every queued file into a fresh round
// trip downloading the account's complete transfer history. The old
// per-file re-list made list calls grow with queue depth; a mutation that
// re-fetches per file fails this test (want 1, gets 3).
func TestProcessUploadCycleFetchesTransferListOncePerCycle(t *testing.T) {
	var listRequests atomic.Int32
	var transferRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/account/info":
			_, _ = response.Write([]byte(`{"status":"success","limit_used":0.5,"booster_points":0}`))
		case "/api/transfer/list":
			listRequests.Add(1)
			_, _ = response.Write([]byte(`{"status":"success","transfers":[{"id":"1","status":"running"}]}`))
		case "/api/transfer/create":
			transferRequests.Add(1)
			_, _ = response.Write([]byte(`{"status":"success"}`))
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	service, firstPath := newQuotaTestService(t, server, "test-key", "once-first.magnet", 5)
	secondPath := filepath.Join(t.TempDir(), "once-second.magnet")
	if err := os.WriteFile(secondPath, []byte("magnet:?xt=urn:btih:second"), 0o600); err != nil {
		t.Fatal(err)
	}
	thirdPath := filepath.Join(t.TempDir(), "once-third.magnet")
	if err := os.WriteFile(thirdPath, []byte("magnet:?xt=urn:btih:third"), 0o600); err != nil {
		t.Fatal(err)
	}
	service.Queue.Add(secondPath)
	service.Queue.Add(thirdPath)

	if got := service.processUploadCycle(); got != 3 {
		t.Fatalf("processed count = %d, want 3", got)
	}
	if got := listRequests.Load(); got != 1 {
		t.Fatalf("transfer list fetches = %d, want 1 (once per cycle)", got)
	}
	if got := transferRequests.Load(); got != 3 {
		t.Fatalf("transfer requests = %d, want 3", got)
	}
	for _, p := range []string{firstPath, secondPath, thirdPath} {
		if !os.IsNotExist(fileExistsError(p)) {
			t.Fatalf("source file %s still exists after the cycle", p)
		}
	}
	if got := service.GetStatus(); got != "Okay" {
		t.Fatalf("status after the cycle = %q, want Okay", got)
	}
}

// TestProcessUploadCycleReservationStopsStaleListBatch is the acceptance
// test for review finding R1-10: the local reservation of the cycle's own
// submissions must stop a multi-file batch from running against a stale
// /transfer/list response that never shows the transfers created earlier in
// the batch. With the reservation bump in the submitted branch removed, the
// second file is consumed against the stale list and this test flips
// PASS -> FAIL (transfer creates = 2, want 1).
func TestProcessUploadCycleReservationStopsStaleListBatch(t *testing.T) {
	var transferRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/account/info":
			_, _ = response.Write([]byte(`{"status":"success","limit_used":0.5,"booster_points":0}`))
		case "/api/transfer/list":
			// The stale count never reflects the cycle's own submission.
			_, _ = response.Write([]byte(`{"status":"success","transfers":[{"id":"1","status":"running"}]}`))
		case "/api/transfer/create":
			transferRequests.Add(1)
			_, _ = response.Write([]byte(`{"status":"success"}`))
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	service, firstPath := newQuotaTestService(t, server, "test-key", "stale-first.magnet", 2)
	secondPath := filepath.Join(t.TempDir(), "stale-second.magnet")
	if err := os.WriteFile(secondPath, []byte("magnet:?xt=urn:btih:second"), 0o600); err != nil {
		t.Fatal(err)
	}
	service.Queue.Add(secondPath)
	var logs bytes.Buffer
	logger := log.StandardLogger()
	oldOutput := logger.Out
	logger.SetOutput(&logs)
	t.Cleanup(func() { logger.SetOutput(oldOutput) })

	if got := service.processUploadCycle(); got != 1 {
		t.Fatalf("processed count = %d, want 1", got)
	}
	if got := transferRequests.Load(); got != 1 {
		t.Fatalf("transfer creates = %d, want 1 (the reservation stops the second file)", got)
	}
	if !os.IsNotExist(fileExistsError(firstPath)) {
		t.Fatal("first source file still exists after its successful create")
	}
	if _, err := os.Stat(secondPath); err != nil {
		t.Fatalf("second source file was consumed against the stale list: %v", err)
	}
	if got := service.Queue.Len(); got != 1 {
		t.Fatalf("queue length = %d, want 1 (second file stays queued)", got)
	}
	if got := service.GetStatus(); got != "Paused: 2 active Premiumize transfers (limit 2)" {
		t.Fatalf("status = %q, want the cap-paused message", got)
	}
	if n := strings.Count(logs.String(), "Premiumize has 2 active transfers; pausing blackhole submissions at configured limit 2"); n != 1 {
		t.Fatalf("cap pause warning count = %d, want 1; logs: %s", n, logs.String())
	}
}
