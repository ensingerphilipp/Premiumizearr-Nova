package directclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ensingerphilipp/premiumizearr-nova/pkg/premiumizeme"
)

func TestDownloadCloudFolderRecursivelyPublishesOnlyCompletedFiles(t *testing.T) {
	if _, err := exec.LookPath("wget"); err != nil {
		t.Skip("wget is required by the production downloader")
	}
	if _, err := exec.LookPath("stdbuf"); err != nil {
		t.Skip("stdbuf is required by the production downloader")
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/folder/list":
			w.Header().Set("Content-Type", "application/json")
			var items []map[string]string
			switch r.URL.Query().Get("id") {
			case "root":
				items = []map[string]string{{"id": "nested", "name": "Season 1", "type": "folder"}, {"id": "f2", "name": "cover.jpg", "type": "file"}}
			case "nested":
				items = []map[string]string{{"id": "f1", "name": "episode.mkv", "type": "file"}}
			default:
				http.Error(w, "unknown folder", http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "content": items})
		case "/api/item/details":
			w.Header().Set("Content-Type", "application/json")
			id := r.URL.Query().Get("id")
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "success", "type": "file", "id": id, "name": id, "link": serverURL(r) + "/blob/" + id})
		case "/blob/f1":
			_, _ = w.Write([]byte("episode contents"))
		case "/blob/f2":
			_, _ = w.Write([]byte("cover contents"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	pm := premiumizeme.NewPremiumizemeClient("test-secret")
	pm.APIBaseURL = server.URL + "/api/"
	pm.HTTPClient = server.Client()

	output := filepath.Join(t.TempDir(), "download")
	var progressCalls int
	if err := DownloadCloudFolder(context.Background(), &pm, "root", output, true, 0, func(done, total int64) {
		progressCalls++
		if total != 0 {
			t.Errorf("unknown folder total should be zero, got %d", total)
		}
	}); err != nil {
		t.Fatalf("DownloadCloudFolder() error = %v", err)
	}
	for name, want := range map[string]string{
		filepath.Join("Season 1", "episode.mkv"): "episode contents",
		"cover.jpg":                              "cover contents",
	} {
		got, err := os.ReadFile(filepath.Join(output, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if progressCalls != 2 {
		t.Errorf("progress callback calls = %d, want 2", progressCalls)
	}
	if _, err := os.Stat(output + ".partial"); !os.IsNotExist(err) {
		t.Errorf("staging directory remains after successful publish: %v", err)
	}
	// A lost response after the atomic publish must be safe to retry.
	if err := DownloadCloudFolder(context.Background(), &pm, "root", output, true, 0, nil); err != nil {
		t.Errorf("idempotent retry error = %v", err)
	}
}

func TestDownloadCloudFolderRejectsTraversalBeforeGeneratingLinks(t *testing.T) {
	linkRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/folder/list" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "content": []map[string]string{{"id": "f1", "name": "../escape", "type": "file"}}})
			return
		}
		if r.URL.Path == "/api/item/details" {
			linkRequests++
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	pm := premiumizeme.NewPremiumizemeClient("test-secret")
	pm.APIBaseURL = server.URL + "/api/"
	pm.HTTPClient = server.Client()

	output := filepath.Join(t.TempDir(), "download")
	err := DownloadCloudFolder(context.Background(), &pm, "root", output, true, 0, nil)
	if err == nil {
		t.Fatal("expected unsafe item name to be rejected")
	}
	if linkRequests != 0 {
		t.Errorf("generated %d links after traversal was detected", linkRequests)
	}
}

// TestDownloadCloudFolderInterruptsHungFolderListing is the regression test
// for the finding that a Premiumize folder listing could hang forever with
// no way to interrupt the download (ListFolder had no context parameter).
func TestDownloadCloudFolderInterruptsHungFolderListing(t *testing.T) {
	parked := make(chan struct{})
	release := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/folder/list" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		parked <- struct{}{}
		// Never respond on its own: the request must be interrupted by the
		// client side. The release (sent at the test's end) keeps the
		// handler terminating so the server close cannot wait on it.
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer server.Close()

	pm := premiumizeme.NewPremiumizemeClient("test-secret")
	pm.APIBaseURL = server.URL + "/api/"
	pm.HTTPClient = server.Client()

	output := filepath.Join(t.TempDir(), "download")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errc := make(chan error, 1)
	go func() {
		errc <- DownloadCloudFolder(ctx, &pm, "folder-1", output, true, 0, nil)
	}()
	select {
	case <-parked:
	case <-time.After(5 * time.Second):
		t.Fatal("the folder listing was never attempted; DownloadCloudFolder bailed out before the listing")
	}
	cancel()
	select {
	case err := <-errc:
		// The client redacts request errors into plain strings (the API
		// key rides in the URL), so a cancellation arrives as the message
		// "context canceled" rather than the unwrappable sentinel; accept
		// either shape.
		if !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "context canceled") {
			t.Fatalf("DownloadCloudFolder err = %v, want a cancellation error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("DownloadCloudFolder did not return after the context was cancelled")
	}
	release <- struct{}{}
}

func serverURL(r *http.Request) string {
	return "http://" + r.Host
}
