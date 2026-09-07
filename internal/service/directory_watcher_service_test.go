package service

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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

type uploadTestTransport func(*http.Request) (*http.Response, error)

func (f uploadTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestFailedUploadRetriesWithoutRescan(t *testing.T) {
	for _, failure := range []string{"temporary backend failure", "Limit of transfers reached!", "account_limit_reached", "new provider limit wording"} {
		t.Run(failure, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					w.Write([]byte(`{"status":"error","message":"` + failure + `"}`))
					return
				}
				w.Write([]byte(`{"status":"success"}`))
			}))
			defer server.Close()
			target, _ := url.Parse(server.URL)
			original := http.DefaultTransport
			http.DefaultTransport = uploadTestTransport(func(r *http.Request) (*http.Response, error) {
				copy := r.Clone(r.Context())
				copy.URL.Scheme = target.Scheme
				copy.URL.Host = target.Host
				return original.RoundTrip(copy)
			})
			defer func() { http.DefaultTransport = original }()
			file := filepath.Join(t.TempDir(), "retry.magnet")
			if err := os.WriteFile(file, []byte("magnet:?xt=urn:btih:test"), 0600); err != nil {
				t.Fatal(err)
			}
			client := premiumizeme.NewPremiumizemeClient("dummy-test-key")
			svc := NewDirectoryWatcherService()
			svc.Queue = stringqueue.NewStringQueue()
			svc.premiumizemeClient = &client
			// The processor has just popped this path. No watcher event/rescan follows.
			if delay := svc.processUpload(file); delay < 10*time.Second {
				t.Errorf("retry delay = %s", delay)
			}
			if _, err := os.Stat(file); err != nil {
				t.Fatal("failed upload lost source")
			}
			ok, retry := svc.Queue.PopTopOfQueue()
			if !ok || retry != file {
				t.Fatal("failed upload was stranded outside queue")
			}
			if delay := svc.processUpload(retry); delay != 2*time.Second {
				t.Fatalf("success delay = %s", delay)
			}
			if _, err := os.Stat(file); !os.IsNotExist(err) {
				t.Fatal("successful upload retained source")
			}
			if svc.Queue.Len() != 0 || calls.Load() != 2 {
				t.Fatal("unexpected retry count")
			}
		})
	}
}

func TestPollingDoesNotDuplicateQueuedUploads(t *testing.T) {
	svc := NewDirectoryWatcherService()
	svc.Queue = stringqueue.NewStringQueue()
	var logs bytes.Buffer
	logger := log.StandardLogger()
	old := logger.Out
	logger.SetOutput(&logs)
	defer logger.SetOutput(old)
	svc.addFileToQueue("request.magnet")
	svc.addFileToQueue("request.magnet")
	if svc.Queue.Len() != 1 {
		t.Fatal("polling added duplicate work")
	}
	if strings.Count(logs.String(), "added to Queue") != 1 {
		t.Fatal("duplicate insertion logged")
	}
}

func TestAlreadyUploadedSourceIsRemoved(t *testing.T) {
	original := http.DefaultTransport
	http.DefaultTransport = uploadTestTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"status":"error","message":" YOU ALREADY ADDED THIS JOB! "}`)), Header: make(http.Header)}, nil
	})
	defer func() { http.DefaultTransport = original }()
	file := filepath.Join(t.TempDir(), "duplicate.magnet")
	if err := os.WriteFile(file, []byte("magnet:?xt=urn:btih:test"), 0600); err != nil {
		t.Fatal(err)
	}
	pm := premiumizeme.NewPremiumizemeClient("dummy-key")
	svc := NewDirectoryWatcherService()
	svc.Queue = stringqueue.NewStringQueue()
	svc.premiumizemeClient = &pm
	if delay := svc.processUpload(file); delay != 2*time.Second {
		t.Fatalf("terminal delay = %s", delay)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatal("already-uploaded source retained")
	}
	if svc.Queue.Len() != 0 || svc.GetStatus() != "Okay" {
		t.Fatal("terminal job queued for retry")
	}
}

func TestRemovedSourceDoesNotRetry(t *testing.T) {
	pm := premiumizeme.NewPremiumizemeClient("dummy-key")
	svc := NewDirectoryWatcherService()
	svc.Queue = stringqueue.NewStringQueue()
	svc.premiumizemeClient = &pm
	svc.processUpload(filepath.Join(t.TempDir(), "missing.magnet"))
	if svc.Queue.Len() != 0 {
		t.Fatal("missing source queued forever")
	}
}
