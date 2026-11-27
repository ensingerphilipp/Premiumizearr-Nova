package service

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	watcherWithoutQueue := NewDirectoryWatcherService()
	for _, tc := range []struct {
		name    string
		watcher *DirectoryWatcherService
	}{
		{"missing watcher", nil},
		{"missing queue", &watcherWithoutQueue},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			webServer := WebServerService{directoryWatcherService: tc.watcher}
			webServer.PollBlackholeHandler(response, request)
			if response.Code != http.StatusServiceUnavailable {
				t.Fatalf("response status = %d, want %d", response.Code, http.StatusServiceUnavailable)
			}
		})
	}
}

func TestPollBlackholeHandlerReportsScanError(t *testing.T) {
	missingDirectory := filepath.Join(t.TempDir(), "missing")
	directoryWatcher := NewDirectoryWatcherService()
	directoryWatcher.Init(nil, &config.Config{BlackholeDirectory: missingDirectory})
	directoryWatcher.Queue = stringqueue.NewStringQueue()
	webServer := WebServerService{directoryWatcherService: &directoryWatcher}
	request := httptest.NewRequest(http.MethodPost, "/api/blackhole/poll", nil)
	response := httptest.NewRecorder()

	webServer.PollBlackholeHandler(response, request)

	if response.Code != http.StatusInternalServerError {
		t.Fatalf("response status = %d, want %d: %s", response.Code, http.StatusInternalServerError, response.Body.String())
	}
	// R1-4: the unauthenticated 500 body must stay generic; the OS error
	// (which names the configured directory) belongs in the log.
	if strings.Contains(response.Body.String(), missingDirectory) {
		t.Fatalf("response body leaks the configured blackhole directory: %s", response.Body.String())
	}
}

func TestBlackholeHandlerRendersBaseNameAcrossPathSeparators(t *testing.T) {
	directoryWatcher := NewDirectoryWatcherService()
	directoryWatcher.Queue = stringqueue.NewStringQueue()
	// Poll-mode queue keys are OS-native: forward slashes on Linux,
	// backslashes on Windows (filepath.Join at the scan site).
	directoryWatcher.Queue.Add("/bh/movie.nzb")
	directoryWatcher.Queue.Add(`C:\bh\movie.nzb`)
	webServer := WebServerService{directoryWatcherService: &directoryWatcher}

	request := httptest.NewRequest(http.MethodGet, "/api/blackhole", nil)
	response := httptest.NewRecorder()
	webServer.BlackholeHandler(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("response status = %d, want %d", response.Code, http.StatusOK)
	}
	var body BlackholeResponse
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(body.BlackholeFiles))
	for _, file := range body.BlackholeFiles {
		got = append(got, file.Name)
	}
	// R1-9: both separators must render the file name only; Linux display
	// stays unchanged.
	want := []string{"movie.nzb", "movie.nzb"}
	if len(got) != len(want) {
		t.Fatalf("displayed names = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("displayed names = %v, want %v", got, want)
		}
	}
}

func TestScanNowSkipsInFlightFile(t *testing.T) {
	blackholeDirectory := t.TempDir()
	queuedPath := filepath.Join(blackholeDirectory, "movie.nzb")
	if err := os.WriteFile(queuedPath, []byte("test"), 0o600); err != nil {
		t.Fatal(err)
	}

	directoryWatcher := NewDirectoryWatcherService()
	directoryWatcher.Init(nil, &config.Config{BlackholeDirectory: blackholeDirectory})
	directoryWatcher.Queue = stringqueue.NewStringQueue()
	if queued, err := directoryWatcher.ScanNow(); err != nil || queued != 1 {
		t.Fatalf("first scan = (%d, %v), want (1, nil)", queued, err)
	}
	if ok, path := directoryWatcher.Queue.PopTopOfQueue(); !ok || path != queuedPath {
		t.Fatalf("popped file = (%t, %q), want (true, %q)", ok, path, queuedPath)
	}
	if queued, err := directoryWatcher.ScanNow(); err != nil || queued != 0 {
		t.Fatalf("scan while processing = (%d, %v), want (0, nil)", queued, err)
	}
	if length := directoryWatcher.Queue.Len(); length != 0 {
		t.Fatalf("queue length while processing = %d, want 0", length)
	}

	directoryWatcher.Queue.Done(queuedPath)
	if queued, err := directoryWatcher.ScanNow(); err != nil || queued != 1 {
		t.Fatalf("scan after processing = (%d, %v), want (1, nil)", queued, err)
	}
}

func TestScanNowConcurrentWithConfigUpdate(t *testing.T) {
	firstDirectory := t.TempDir()
	secondDirectory := t.TempDir()
	directoryWatcher := NewDirectoryWatcherService()
	currentConfig, err := config.LoadOrCreateConfig(t.TempDir(), directoryWatcher.ConfigUpdatedCallback)
	if err != nil {
		t.Fatal(err)
	}
	currentConfig.BlackholeDirectory = firstDirectory
	directoryWatcher.Init(nil, &currentConfig)
	directoryWatcher.Queue = stringqueue.NewStringQueue()

	start := make(chan struct{})
	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		<-start
		for i := 0; i < 40; i++ {
			nextConfig := currentConfig
			if i%2 == 0 {
				nextConfig.BlackholeDirectory = secondDirectory
			} else {
				nextConfig.BlackholeDirectory = firstDirectory
			}
			currentConfig.UpdateConfig(nextConfig)
		}
	}()
	go func() {
		defer workers.Done()
		<-start
		for i := 0; i < 40; i++ {
			if _, err := directoryWatcher.ScanNow(); err != nil {
				t.Errorf("concurrent scan failed: %v", err)
				return
			}
		}
	}()
	close(start)
	workers.Wait()

	// R1-6: the watcher's cached directory must follow the last config
	// update, not the one passed to Init.
	finalConfig := currentConfig
	finalConfig.BlackholeDirectory = secondDirectory
	currentConfig.UpdateConfig(finalConfig)
	// UpdateConfig applies service callbacks asynchronously on the
	// serialized reconfiguration worker; wait for the final update's
	// callback before asserting the cached directory.
	config.WaitReconfigIdle()
	if got := directoryWatcher.getBlackholeDirectory(); got != secondDirectory {
		t.Fatalf("cached blackhole directory = %q, want last-updated %q", got, secondDirectory)
	}
}

// TestBlackholeHandlerDerivesArrFromPath is the regression test for
// findings R1-12 and S-23. The Arr column must be derived with filepath
// (not path), because fsnotify event names carry the platform separator -
// on Windows that is a backslash, which path.Base/path.Dir do not split.
// filepath normalizes both separator styles on both platforms. And the
// Arr must be set only for a file directly inside a CURRENTLY CONFIGURED
// Arr subfolder under the CURRENT blackhole root (the same classification
// the upload routing uses): a root file, a feature-off file, a file in an
// arbitrary or unconfigured subfolder, a nested file, and a file in a
// leftover of a previous blackhole location all report an empty Arr. This
// test pins the Linux behavior; the Windows backslash behavior follows
// from the same filepath calls but is not executable here because CI runs
// on Linux only.
func TestBlackholeHandlerDerivesArrFromPath(t *testing.T) {
	bh := t.TempDir()
	sonarrDir := filepath.Join(bh, "sonarr")
	if err := os.MkdirAll(sonarrDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A leftover of a previous blackhole location that still carries an
	// Arr-named subfolder: it is not under the current blackhole root, so
	// its file must not be classified as an Arr file.
	oldBH := t.TempDir()
	oldSonarrDir := filepath.Join(oldBH, "sonarr")
	if err := os.MkdirAll(oldSonarrDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// An unconfigured subfolder under the current blackhole: its name is
	// not a configured Arr slug, so its file must not be classified as an
	// Arr file.
	otherDir := filepath.Join(bh, "pirater")
	if err := os.MkdirAll(otherDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A nested directory inside the configured subfolder: the Arr is
	// derived only from the DIRECT parent of the file.
	nestedDir := filepath.Join(sonarrDir, "season1")
	if err := os.MkdirAll(nestedDir, 0o755); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		filePath string
		enabled  bool
		wantArr  string
	}{
		{"file directly inside configured subfolder", filepath.Join(sonarrDir, "episode.magnet"), true, "sonarr"},
		{"file in blackhole root", filepath.Join(bh, "root-file.magnet"), true, ""},
		{"feature off", filepath.Join(sonarrDir, "episode-off.magnet"), false, ""},
		{"unconfigured subfolder", filepath.Join(otherDir, "other.magnet"), true, ""},
		{"nested inside configured subfolder", filepath.Join(nestedDir, "nested.magnet"), true, ""},
		{"leftover of previous blackhole", filepath.Join(oldSonarrDir, "old.magnet"), true, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dw := NewDirectoryWatcherService()
			dw.Queue = stringqueue.NewStringQueue()
			dw.Queue.Add(tt.filePath)

			s := &WebServerService{
				directoryWatcherService: &dw,
				config: &config.Config{
					BlackholeDirectory:  bh,
					EnableArrSubfolders: tt.enabled,
					Arrs:                []config.ArrConfig{{Name: "sonarr"}},
				},
			}

			req := httptest.NewRequest(http.MethodGet, "/api/blackhole", nil)
			rec := httptest.NewRecorder()
			s.BlackholeHandler(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("BlackholeHandler status = %d, want 200; body: %s", rec.Code, rec.Body.String())
			}

			var resp BlackholeResponse
			if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
				t.Fatalf("decoding response: %v", err)
			}
			if resp.Status == "Not Initialized" {
				t.Fatal("BlackholeHandler reported Not Initialized for an initialized watcher")
			}
			if len(resp.BlackholeFiles) != 1 {
				t.Fatalf("BlackholeFiles = %v, want 1 entry", resp.BlackholeFiles)
			}
			if got := resp.BlackholeFiles[0].Arr; got != tt.wantArr {
				t.Fatalf("Arr for %q = %q, want %q", filepath.Base(tt.filePath), got, tt.wantArr)
			}
		})
	}
}

// TestBlackholeHandlerNotInitialized guards the nil-watcher path: the
// handler reports the uninitialized state instead of panicking.
func TestBlackholeHandlerNotInitialized(t *testing.T) {
	s := &WebServerService{config: &config.Config{}}

	req := httptest.NewRequest(http.MethodGet, "/api/blackhole", nil)
	rec := httptest.NewRecorder()
	s.BlackholeHandler(rec, req)

	var resp BlackholeResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp.Status != "Not Initialized" {
		t.Fatalf("Status = %q, want Not Initialized", resp.Status)
	}
}
