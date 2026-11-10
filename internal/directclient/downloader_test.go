package directclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ensingerphilipp/premiumizearr-nova/pkg/premiumizeme"
)

func TestDownloadCloudFolderRecursivelyPublishesOnlyCompletedFiles(t *testing.T) {
	// These are hard requirements of the production downloader, not
	// optional: a silent skip would report unverified download behavior
	// as passing on a host that lacks the tools.
	if _, err := exec.LookPath("wget"); err != nil {
		t.Fatal("wget is required by the production downloader; install it (e.g. 'apt-get install wget') to run this test")
	}
	if _, err := exec.LookPath("stdbuf"); err != nil {
		t.Fatal("stdbuf is required by the production downloader; install it (e.g. 'apt-get install coreutils') to run this test")
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
	if err := DownloadCloudFolder(context.Background(), &pm, "root", output, "job-1", true, 0, func(done, total int64) {
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
	// A lost response after the atomic publish must be safe to retry: the
	// published manifest carries the same key, so the retry is idempotent.
	if err := DownloadCloudFolder(context.Background(), &pm, "root", output, "job-1", true, 0, nil); err != nil {
		t.Errorf("idempotent retry error = %v", err)
	}
	// A different downloader at the same path is not a retry: the
	// manifest proves the tree belongs to "job-1", so the call must fail
	// instead of adopting the foreign published content.
	if err := DownloadCloudFolder(context.Background(), &pm, "root", output, "other-job", true, 0, nil); err == nil {
		t.Error("a different published key was accepted on an existing output directory")
	}
	manifest, err := os.ReadFile(filepath.Join(output, publishedManifestName))
	if err != nil {
		t.Fatalf("published manifest missing: %v", err)
	}
	if string(manifest) != "job-1" {
		t.Errorf("published manifest = %q, want the publishing job key", manifest)
	}
}

// TestDownloadCloudFolderRejectsForeignPreCreatedDirectory is the regression
// test for R1-2: the fast path treated ANY existing output directory as a
// finished publish, so a pre-created (or stale) directory was adopted with
// zero bytes delivered — and the caller then deleted the cloud folder above
// content this call never produced. Only the manifest written by the
// downloader's own atomic publish can prove the directory is finished.
func TestDownloadCloudFolderRejectsForeignPreCreatedDirectory(t *testing.T) {
	linkRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	// The directory exists before the call, with foreign content, and no
	// manifest from this downloader.
	if err := os.MkdirAll(output, 0755); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(output, "user-file.txt")
	if err := os.WriteFile(foreign, []byte("foreign content"), 0600); err != nil {
		t.Fatal(err)
	}

	err := DownloadCloudFolder(context.Background(), &pm, "root", output, "job-1", true, 0, nil)
	if err == nil {
		t.Fatal("an existing output directory without this downloader's manifest was accepted as a finished publish")
	}
	if !strings.Contains(err.Error(), "not published by this downloader") {
		t.Fatalf("error = %q, want the foreign-directory diagnosis", err)
	}
	if linkRequests != 0 {
		t.Errorf("generated %d Premiumize links for a directory the call refused to adopt", linkRequests)
	}
	// Refusing the directory must not touch its content or write a
	// manifest into it.
	if got, err := os.ReadFile(foreign); err != nil || string(got) != "foreign content" {
		t.Fatalf("foreign content after the refused call = %q (%v), want untouched", got, err)
	}
	if _, err := os.Stat(filepath.Join(output, publishedManifestName)); !os.IsNotExist(err) {
		t.Fatalf("a manifest was written into the foreign directory: %v", err)
	}
	// The SAME downloader may still fail the same way: the directory
	// stays foreign until it is removed, no matter how often it is retried.
	if err := DownloadCloudFolder(context.Background(), &pm, "root", output, "job-1", true, 0, nil); err == nil {
		t.Error("a retry adopted the still-foreign directory")
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
	err := DownloadCloudFolder(context.Background(), &pm, "root", output, "job-1", true, 0, nil)
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
		errc <- DownloadCloudFolder(ctx, &pm, "folder-1", output, "job-1", true, 0, nil)
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

// TestDownloadCloudFolderPartialNameCannotCollideWithSibling is the
// regression test for the finding that a cloud entry named
// <sibling>.partial made the downloader resume (wget -c) against the
// sibling's completed staging file, publishing the sibling's content under
// the sibling's name. The in-progress name is now unique per file ID.
func TestDownloadCloudFolderPartialNameCannotCollideWithSibling(t *testing.T) {
	if _, err := exec.LookPath("wget"); err != nil {
		t.Fatal("wget is required by the production downloader; install it (e.g. 'apt-get install wget') to run this test")
	}
	if _, err := exec.LookPath("stdbuf"); err != nil {
		t.Fatal("stdbuf is required by the production downloader; install it (e.g. 'apt-get install coreutils') to run this test")
	}
	const (
		aContent = "AAAAAAAAAAAAAAAAAAAA"           // file "x" (id fa)
		bContent = "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBB" // file "x.partial" (id fb)
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/folder/list":
			// Listing order matters: the entry whose name is the
			// sibling's staging target is published BEFORE the sibling,
			// which is when the collision was exploitable.
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "content": []map[string]string{
				{"id": "fb", "name": "x.partial", "type": "file"},
				{"id": "fa", "name": "x", "type": "file"},
			}})
		case "/api/item/details":
			id := r.URL.Query().Get("id")
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "success", "type": "file", "id": id, "name": id, "link": serverURL(r) + "/blob/" + id})
		case "/blob/fa", "/blob/fb":
			content := map[string][]byte{"/blob/fa": []byte(aContent), "/blob/fb": []byte(bContent)}[r.URL.Path]
			if rng := r.Header.Get("Range"); strings.HasPrefix(rng, "bytes=") {
				start, err := strconv.Atoi(strings.TrimSuffix(rng[len("bytes="):], "-"))
				if err == nil && start < len(content) {
					w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(content)-1, len(content)))
					w.WriteHeader(http.StatusPartialContent)
					_, _ = w.Write(content[start:])
					return
				}
				w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", len(content)))
				http.Error(w, "Range Not Satisfiable", http.StatusRequestedRangeNotSatisfiable)
				return
			}
			_, _ = w.Write(content)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	pm := premiumizeme.NewPremiumizemeClient("test-secret")
	pm.APIBaseURL = server.URL + "/api/"
	pm.HTTPClient = server.Client()

	output := filepath.Join(t.TempDir(), "download")
	if err := DownloadCloudFolder(context.Background(), &pm, "root", output, "job-1", true, 0, nil); err != nil {
		t.Fatalf("DownloadCloudFolder() error = %v", err)
	}
	for name, want := range map[string]string{"x": aContent, "x.partial": bContent} {
		got, err := os.ReadFile(filepath.Join(output, name))
		if err != nil {
			t.Fatalf("published %q missing: %v", name, err)
		}
		if string(got) != want {
			t.Errorf("published %q = %q, want exactly %q (a sibling's content must not leak into another file)", name, got, want)
		}
	}
}
