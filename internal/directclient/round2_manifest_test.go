package directclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ensingerphilipp/premiumizearr-nova/pkg/premiumizeme"
)

// TestDownloadCloudFolderRejectsManifestNamedFile is the round-2 regression
// test for the finding that a cloud file named exactly the downloader's
// manifest name (.directclient-published) is downloaded, then the manifest
// write truncates it to the 5-byte key before the staging rename publishes
// the tree: the folder publishes "successfully" with that file's content
// silently replaced, the completion path deletes the cloud source (the only
// copy of the original bytes), and every retry re-reads the matching
// manifest as success — permanent, idempotent corruption. The collect stage
// now rejects the reserved name before any download starts.
func TestDownloadCloudFolderRejectsManifestNamedFile(t *testing.T) {
	linkRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/folder/list":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "content": []map[string]string{
				{"id": "mf", "name": ".directclient-published", "type": "file"},
				{"id": "ok", "name": "ok", "type": "file"},
			}})
		case "/api/item/details":
			linkRequests++
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "success", "type": "file", "id": r.URL.Query().Get("id"), "name": "x", "link": serverURL(r) + "/blob"})
		case "/blob":
			_, _ = w.Write([]byte("payload"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	pm := premiumizeme.NewPremiumizemeClient("test-secret")
	pm.APIBaseURL = server.URL + "/api/"
	pm.HTTPClient = server.Client()

	output := filepath.Join(t.TempDir(), "download")
	err := DownloadCloudFolder(context.Background(), &pm, "root", output, "job-1", true, 0, nil)
	if err == nil {
		t.Fatal("a listing containing a file named .directclient-published was accepted; the manifest write would truncate it to the key before publish")
	}
	if !strings.Contains(err.Error(), "reserved manifest name") {
		t.Fatalf("error = %q, want the reserved-manifest-name diagnosis", err)
	}
	if linkRequests != 0 {
		t.Errorf("generated %d Premiumize links after the reserved name was detected", linkRequests)
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Errorf("a refused publish created the output directory: %v", err)
	}
}

// TestDownloadCloudFolderRejectsManifestNamedFolder is the sibling variant:
// a cloud SUBFOLDER named .directclient-published makes the same manifest
// WriteFile hit EISDIR at publish, failing the download with an opaque OS
// error naming no fix. The collect-stage rejection covers folders too.
func TestDownloadCloudFolderRejectsManifestNamedFolder(t *testing.T) {
	linkRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/folder/list":
			switch r.URL.Query().Get("id") {
			case "root":
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "content": []map[string]string{
					{"id": "md", "name": ".directclient-published", "type": "folder"},
					{"id": "ok", "name": "ok", "type": "file"},
				}})
			case "md":
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "content": []map[string]string{
					{"id": "mf1", "name": "inner.mkv", "type": "file"},
				}})
			default:
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "content": []map[string]string{}})
			}
		case "/api/item/details":
			linkRequests++
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "success", "type": "file", "id": r.URL.Query().Get("id"), "name": "inner.mkv", "link": serverURL(r) + "/blob"})
		case "/blob":
			_, _ = w.Write([]byte("payload"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	pm := premiumizeme.NewPremiumizemeClient("test-secret")
	pm.APIBaseURL = server.URL + "/api/"
	pm.HTTPClient = server.Client()

	output := filepath.Join(t.TempDir(), "download")
	err := DownloadCloudFolder(context.Background(), &pm, "root", output, "job-1", true, 0, nil)
	if err == nil {
		t.Fatal("a listing containing a folder named .directclient-published was accepted; the manifest write would hit EISDIR at publish")
	}
	if !strings.Contains(err.Error(), "reserved manifest name") {
		t.Fatalf("error = %q, want the reserved-manifest-name diagnosis", err)
	}
	if linkRequests != 0 {
		t.Errorf("generated %d Premiumize links after the reserved name was detected", linkRequests)
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Errorf("a refused publish created the output directory: %v", err)
	}
}
