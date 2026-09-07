package service

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/ensingerphilipp/premiumizearr-nova/internal/config"
	"github.com/ensingerphilipp/premiumizearr-nova/pkg/stringqueue"
)

func TestPollBlackholeHandler(t *testing.T) {
	blackholeDirectory := t.TempDir()
	queuedPath := filepath.Join(blackholeDirectory, "movie.nzb")
	if err := os.WriteFile(queuedPath, []byte("test"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blackholeDirectory, "ignore.txt"), []byte("test"), 0o600); err != nil {
		t.Fatal(err)
	}

	directoryWatcher := NewDirectoryWatcherService()
	directoryWatcher.Init(nil, &config.Config{BlackholeDirectory: blackholeDirectory})
	directoryWatcher.Queue = stringqueue.NewStringQueue()
	webServer := WebServerService{directoryWatcherService: &directoryWatcher}

	request := httptest.NewRequest(http.MethodPost, "/api/blackhole/poll", nil)
	response := httptest.NewRecorder()
	webServer.PollBlackholeHandler(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("response status = %d, want %d: %s", response.Code, http.StatusOK, response.Body.String())
	}

	var body BlackholePollResponse
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Queued != 1 {
		t.Fatalf("queued files = %d, want 1", body.Queued)
	}

	queue := directoryWatcher.Queue.GetQueue()
	if len(queue) != 1 || queue[0] != queuedPath {
		t.Fatalf("queue = %v, want [%s]", queue, queuedPath)
	}

	secondResponse := httptest.NewRecorder()
	webServer.PollBlackholeHandler(secondResponse, request)
	if secondResponse.Code != http.StatusOK {
		t.Fatalf("second response status = %d, want %d: %s", secondResponse.Code, http.StatusOK, secondResponse.Body.String())
	}
	if err := json.NewDecoder(secondResponse.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Queued != 0 {
		t.Fatalf("files queued by second poll = %d, want 0", body.Queued)
	}
	if queueLength := directoryWatcher.Queue.Len(); queueLength != 1 {
		t.Fatalf("queue length after second poll = %d, want 1", queueLength)
	}
}

func TestPollBlackholeHandlerRejectsOtherMethods(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/api/blackhole/poll", nil)
	response := httptest.NewRecorder()

	webServer := WebServerService{}
	webServer.PollBlackholeHandler(response, request)

	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("response status = %d, want %d", response.Code, http.StatusMethodNotAllowed)
	}
}

func TestPollBlackholeHandlerRequiresInitializedWatcher(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/api/blackhole/poll", nil)
	response := httptest.NewRecorder()

	webServer := WebServerService{}
	webServer.PollBlackholeHandler(response, request)

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("response status = %d, want %d", response.Code, http.StatusServiceUnavailable)
	}
}
