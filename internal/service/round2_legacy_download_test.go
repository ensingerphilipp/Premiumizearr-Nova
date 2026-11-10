package service

// Round-2 regression test for the finding that a failed download through
// the legacy (blackhole) path wrote straight to the FINAL file name inside
// the *arr-watched folder, so a truncated file was left there: the blackhole
// would import the partial content, and a later resume (wget -c) would
// accept the stale bytes against different content. The legacy caller now
// removes the failed file; the directclient caller is exempt because it
// stages under an invisible ID-suffixed name.

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ensingerphilipp/premiumizearr-nova/internal/config"
	"github.com/ensingerphilipp/premiumizearr-nova/pkg/premiumizeme"
)

func TestLegacyBlackholeDownloadFailureRemovesTruncatedFile(t *testing.T) {
	if _, err := exec.LookPath("wget"); err != nil {
		t.Fatal("wget is required by the production downloader; install it (e.g. 'apt-get install wget') to run this test")
	}
	if _, err := exec.LookPath("stdbuf"); err != nil {
		t.Fatal("stdbuf is required by the production downloader; install it (e.g. 'apt-get install coreutils') to run this test")
	}
	var blobHits atomic.Int32
	media := bytes.Repeat([]byte("01234569abcdef"), 16384) // 128 KiB
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/folder/list":
			if r.URL.Query().Get("id") == "folder-1" {
				fmt.Fprint(w, `{"status":"success","content":[{"id":"file-1","name":"Movie.mkv","type":"file"}]}`)
			} else {
				fmt.Fprint(w, `{"status":"success","content":[]}`)
			}
		case "/api/item/details":
			fmt.Fprintf(w, `{"status":"success","id":"file-1","name":"Movie.mkv","type":"file","link":"http://%s/blob"}`, r.Host)
		case "/blob":
			// First hit: announce 128 KiB, deliver half, then drop the
			// connection — wget observes a truncated body and retries;
			// later hits are a definitive 404 that terminates it, so the
			// failure stays fast instead of riding wget's retry backoff.
			if blobHits.Add(1) == 1 {
				hj, ok := w.(http.Hijacker)
				if !ok {
					t.Fatal("recorder does not support hijacking")
					return
				}
				conn, bufw, err := hj.Hijack()
				if err != nil {
					t.Fatal(err)
					return
				}
				_, _ = bufw.Write([]byte("HTTP/1.1 200 OK\r\nContent-Type: application/octet-stream\r\nContent-Length: " + strconv.Itoa(len(media)) + "\r\nConnection: close\r\n\r\n"))
				_, _ = bufw.Write(media[:len(media)/2])
				conn.Close()
				return
			}
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	downloadsDir := t.TempDir()
	cfg := &config.Config{
		DownloadsDirectory: downloadsDir,
		TransferDirectory:  "arrDownloads",
		EnableTlsCheck:     true,
	}
	pm := premiumizeme.NewPremiumizemeClient("test-key")
	pm.APIBaseURL = server.URL + "/api/"
	pm.HTTPClient = server.Client()

	ts := TransferManagerService{}.New()
	ts.Init(&pm, nil, cfg)

	fileSavePath := filepath.Join(downloadsDir, "Release", "Movie.mkv")
	if err := ts.downloadFolderRecursively(premiumizeme.Item{ID: "folder-1", Name: "Release", Type: "folder"}, downloadsDir); err == nil {
		t.Fatal("expected the failed download to be reported")
	}
	if _, err := os.Stat(fileSavePath); err == nil {
		t.Fatalf("a failed legacy download left a truncated file at the final name inside the watched folder: %s", fileSavePath)
	}
}

// The success twin: a completed legacy download must keep the full file at
// the final name (guarding against over-deletion).
func TestLegacyBlackholeDownloadSuccessKeepsFile(t *testing.T) {
	if _, err := exec.LookPath("wget"); err != nil {
		t.Fatal("wget is required by the production downloader; install it (e.g. 'apt-get install wget') to run this test")
	}
	if _, err := exec.LookPath("stdbuf"); err != nil {
		t.Fatal("stdbuf is required by the production downloader; install it (e.g. 'apt-get install coreutils') to run this test")
	}
	media := bytes.Repeat([]byte("01234569abcdef"), 16384)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/folder/list":
			if r.URL.Query().Get("id") == "folder-1" {
				fmt.Fprint(w, `{"status":"success","content":[{"id":"file-1","name":"Movie.mkv","type":"file"}]}`)
			} else {
				fmt.Fprint(w, `{"status":"success","content":[]}`)
			}
		case "/api/item/details":
			fmt.Fprintf(w, `{"status":"success","id":"file-1","name":"Movie.mkv","type":"file","link":"http://%s/blob"}`, r.Host)
		case "/blob":
			w.Header().Set("Content-Type", "application/octet-stream")
			if rng := r.Header.Get("Range"); strings.HasPrefix(rng, "bytes=") {
				start, err := strconv.Atoi(strings.TrimSuffix(rng[len("bytes="):], "-"))
				if err == nil && start < len(media) {
					w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(media)-1, len(media)))
					w.WriteHeader(http.StatusPartialContent)
					_, _ = w.Write(media[start:])
					return
				}
				w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", len(media)))
				http.Error(w, "Range Not Satisfiable", http.StatusRequestedRangeNotSatisfiable)
				return
			}
			_, _ = w.Write(media)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	downloadsDir := t.TempDir()
	cfg := &config.Config{
		DownloadsDirectory: downloadsDir,
		TransferDirectory:  "arrDownloads",
		EnableTlsCheck:     true,
	}
	pm := premiumizeme.NewPremiumizemeClient("test-key")
	pm.APIBaseURL = server.URL + "/api/"
	pm.HTTPClient = server.Client()

	ts := TransferManagerService{}.New()
	ts.Init(&pm, nil, cfg)

	if err := ts.downloadFolderRecursively(premiumizeme.Item{ID: "folder-1", Name: "Release", Type: "folder"}, downloadsDir); err != nil {
		t.Fatalf("successful legacy download reported an error: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(downloadsDir, "Release", "Movie.mkv"))
	if err != nil {
		t.Fatalf("published file missing after a successful download: %v", err)
	}
	if !bytes.Equal(got, media) {
		t.Fatalf("published content = %d bytes, want the full %d-byte payload", len(got), len(media))
	}
}
