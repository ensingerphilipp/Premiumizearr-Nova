package service

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"encoding/json"

	"github.com/ensingerphilipp/premiumizearr-nova/internal/config"
	"github.com/ensingerphilipp/premiumizearr-nova/pkg/stringqueue"
)

func newBlockerTestWatcher(t *testing.T, bh string, arrs ...config.ArrConfig) *DirectoryWatcherService {
	t.Helper()
	dw := NewDirectoryWatcherService()
	dw.config = &config.Config{
		BlackholeDirectory:  bh,
		EnableArrSubfolders: true,
		Arrs:                arrs,
	}
	dw.Queue = stringqueue.NewStringQueue()
	return &dw
}

// TestScanNowCountsArrSubfolderFilesSynchronously is the regression test for
// the manual-poll merge blocker: a manual scan of a blackhole with a file
// in a configured Arr subfolder must report that file in its returned
// count (and have it in the queue immediately), not only the root files.
// Before the fix the subfolder scan was fired as an async goroutine and
// ScanNow returned queued=0 while the file appeared in the queue later.
func TestScanNowCountsArrSubfolderFilesSynchronously(t *testing.T) {
	bh := t.TempDir()
	sonarrDir := filepath.Join(bh, "sonarr")
	if err := os.MkdirAll(sonarrDir, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(sonarrDir, "demo.magnet")
	if err := os.WriteFile(file, []byte("magnet:?xt=urn:btih:demo"), 0o600); err != nil {
		t.Fatal(err)
	}

	dw := newBlockerTestWatcher(t, bh, config.ArrConfig{Name: "sonarr"})
	queued, err := dw.ScanNow()
	if err != nil {
		t.Fatalf("ScanNow: %v", err)
	}
	if queued != 1 {
		t.Fatalf("ScanNow queued = %d, want 1 (Arr subfolder file must be counted synchronously)", queued)
	}
	if got := dw.Queue.GetQueue(); len(got) != 1 || got[0] != file {
		t.Fatalf("queue after ScanNow = %v, want [%s]", got, file)
	}
}

// TestScanNowDoesNotRecurseIntoNestedSlugDirectories is the regression test
// for the depth half of the manual-poll merge blocker: a directory nested
// inside a configured Arr subfolder is not a drop zone even when its own
// name matches another configured slug. Before the fix the subfolder scan
// recursed through checkFile, so blackhole/sonarr/radarr/demo.magnet was
// queued and then routed to the main destination.
func TestScanNowDoesNotRecurseIntoNestedSlugDirectories(t *testing.T) {
	bh := t.TempDir()
	nested := filepath.Join(bh, "sonarr", "radarr")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(nested, "demo.magnet")
	if err := os.WriteFile(file, []byte("magnet:?xt=urn:btih:demo"), 0o600); err != nil {
		t.Fatal(err)
	}

	dw := newBlockerTestWatcher(t, bh,
		config.ArrConfig{Name: "sonarr"},
		config.ArrConfig{Name: "radarr"},
	)
	queued, err := dw.ScanNow()
	if err != nil {
		t.Fatalf("ScanNow: %v", err)
	}
	if queued != 0 {
		t.Fatalf("ScanNow queued = %d, want 0 (nested directories are never drop zones)", queued)
	}
	if got := dw.Queue.GetQueue(); len(got) != 0 {
		t.Fatalf("queue after ScanNow = %v, want empty (nested file must not be queued)", got)
	}
}

// TestConfiguredBlackholeSlugAtFilesystemRoot is the regression test for
// the root-blackhole merge blocker: with BlackholeDirectory set to a
// filesystem root, the direct-child test must still recognize a direct
// Arr subfolder. Before the fix a separator-prefix check built "//" for
// the root and never matched, so /sonarr/demo.magnet was misrouted to the
// main destination (and the UI showed no Arr).
func TestConfiguredBlackholeSlugAtFilesystemRoot(t *testing.T) {
	arrs := []config.ArrConfig{{Name: "sonarr"}, {Name: "radarr"}}
	tests := []struct {
		name      string
		blackhole string
		filePath  string
		wantSlug  string
	}{
		{"direct slug file under root blackhole", "/", "/sonarr/demo.magnet", "sonarr"},
		{"root file under root blackhole", "/", "/demo.magnet", ""},
		{"nested slug dir under root blackhole", "/", "/sonarr/radarr/demo.magnet", ""},
		{"unconfigured dir under root blackhole", "/", "/pirater/demo.magnet", ""},
		{"outside the root blackhole", "/", "/other/sonarr/demo.magnet", ""},
		{"direct slug file under regular blackhole", "/bh", "/bh/sonarr/demo.magnet", "sonarr"},
		{"direct slug file under working-dir blackhole", ".", "sonarr/demo.magnet", "sonarr"},
		{"root file under working-dir blackhole", ".", "demo.magnet", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := configuredBlackholeSlug(tt.filePath, tt.blackhole, true, arrs)
			if got != tt.wantSlug {
				t.Fatalf("configuredBlackholeSlug(%q, %q) = %q, want %q", tt.filePath, tt.blackhole, got, tt.wantSlug)
			}
		})
	}
}

// TestResolveTargetFolderIDAtFilesystemRoot pins the upload routing half of
// the root-blackhole blocker: a direct Arr file under a root blackhole must
// resolve to the Arr's subfolder ID, not the main destination.
func TestResolveTargetFolderIDAtFilesystemRoot(t *testing.T) {
	arrs := []config.ArrConfig{{Name: "sonarr"}}
	arrFolders := map[string]string{"sonarr": "sonarr-folder-id"}

	folderID, ok, slug := resolveTargetFolderID("/sonarr/demo.magnet", "/", "main-folder-id", arrFolders, true, arrs)
	if !ok || folderID != "sonarr-folder-id" || slug != "sonarr" {
		t.Fatalf("resolveTargetFolderID = (%q, %v, %q), want (sonarr-folder-id, true, sonarr)", folderID, ok, slug)
	}
}

// TestBlackholeHandlerArrAtFilesystemRoot pins the UI half of the
// root-blackhole blocker: the queue view must report the Arr for a direct
// subfolder file even when the blackhole is a filesystem root.
func TestBlackholeHandlerArrAtFilesystemRoot(t *testing.T) {
	dw := NewDirectoryWatcherService()
	dw.Queue = stringqueue.NewStringQueue()
	dw.Queue.Add("/sonarr/demo.magnet")
	s := &WebServerService{
		directoryWatcherService: &dw,
		config: &config.Config{
			BlackholeDirectory:  "/",
			EnableArrSubfolders: true,
			Arrs:                []config.ArrConfig{{Name: "sonarr"}},
		},
	}

	req := httptest.NewRequest(http.MethodGet, "/api/blackhole", nil)
	rec := httptest.NewRecorder()
	s.BlackholeHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("BlackholeHandler status = %d, want 200", rec.Code)
	}
	var resp BlackholeResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if len(resp.BlackholeFiles) != 1 {
		t.Fatalf("BlackholeFiles = %v, want 1 entry", resp.BlackholeFiles)
	}
	if got := resp.BlackholeFiles[0].Arr; got != "sonarr" {
		t.Fatalf("Arr for /sonarr/demo.magnet under root blackhole = %q, want sonarr", got)
	}
	if got := resp.BlackholeFiles[0].Name; got != "demo.magnet" {
		t.Fatalf("Name = %q, want demo.magnet", got)
	}
}
