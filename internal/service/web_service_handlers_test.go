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
