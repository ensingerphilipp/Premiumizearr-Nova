package progress_downloader

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// requireDownloadTools fails (rather than skips) when the downloader's
// external tools are absent, with the install path: a silent skip would let
// a host without wget/stdbuf report the download behavior as "verified".
func requireDownloadTools(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("wget"); err != nil {
		t.Fatal("wget is required by the production downloader; install it (e.g. 'apt-get install wget') to run this test")
	}
	if _, err := exec.LookPath("stdbuf"); err != nil {
		t.Fatal("stdbuf is required by the production downloader; install it (e.g. 'apt-get install coreutils') to run this test")
	}
}

// patternedPayload builds a deterministic byte pattern so prefix equality
// means content equality, not just length equality.
func patternedPayload(n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = byte(i*31 + i/251)
	}
	return out
}

// TestDownloadFileContextResumesPartialFile is the committed evidence for
// the finding that wget's resume was doubted: with a 40960-byte partial
// file, wget sends "Range: bytes=40960-" and only the missing bytes are
// served, producing a byte-identical file. The wget exit code is a harness
// artifact and deliberately not asserted; the bytes are the contract.
func TestDownloadFileContextResumesPartialFile(t *testing.T) {
	requireDownloadTools(t)
	payload := patternedPayload(120000)
	const partialSize = 40960

	var mu sync.Mutex
	var rangeValues []string
	var served atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		appendRange(&mu, &rangeValues, r.Header.Get("Range"))
		if start, ok := resumeRangeStart(r.Header.Get("Range")); ok {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(payload)-1, len(payload)))
			w.WriteHeader(http.StatusPartialContent)
			n, _ := w.Write(payload[start:])
			served.Add(int64(n))
			return
		}
		n, _ := w.Write(payload)
		served.Add(int64(n))
	}))
	defer server.Close()

	outPath := filepath.Join(t.TempDir(), "resumed.bin")
	if err := os.WriteFile(outPath, payload[:partialSize], 0600); err != nil {
		t.Fatal(err)
	}

	// wget's exit code is a harness artifact; the file bytes are asserted
	// below instead.
	err := DownloadFileContext(context.Background(), false, 0, server.URL+"/file.bin", outPath, NewWriteCounter())
	if err != nil {
		t.Logf("DownloadFileContext returned %v (wget exit code not asserted; bytes are)", err)
	}

	mu.Lock()
	ranges := append([]string(nil), rangeValues...)
	mu.Unlock()
	if len(ranges) == 0 {
		t.Fatal("the server never saw a Range header; the partial file was not resumed")
	}
	if ranges[0] != fmt.Sprintf("bytes=%d-", partialSize) {
		t.Fatalf("Range headers = %v, want the first to be %q", ranges, fmt.Sprintf("bytes=%d-", partialSize))
	}
	if got := served.Load(); got != int64(len(payload)-partialSize) {
		t.Fatalf("served %d bytes, want %d (only the missing bytes)", got, len(payload)-partialSize)
	}
	final, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("reading the resumed file: %v", err)
	}
	if !bytes.Equal(final, payload) {
		t.Fatalf("resumed file is not byte-identical to the source (len=%d, want %d)", len(final), len(payload))
	}
}

// TestDownloadFileContextKeepsPartialAfterServerFailure is the regression
// test for the finding that a wget failure deleted the partial file —
// discarding exactly the bytes the next "wget -c" attempt needs to resume.
// The partial must survive.
//
// The server truncates the first attempt mid-body (a retryable failure for
// wget) and answers every retry with a definitive 404: wget does not retry
// a permanent 4xx, so the failure is terminal and fast instead of running
// wget's full ~150 s retry backoff against an eternally flaky server.
func TestDownloadFileContextKeepsPartialAfterServerFailure(t *testing.T) {
	requireDownloadTools(t)
	payload := patternedPayload(256 << 10)
	var firstAttempt atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if firstAttempt.CompareAndSwap(false, true) {
			// Declare the full body, serve exactly half, then drop the
			// connection abruptly: wget fails on the truncated body and
			// has written a resumable partial.
			h, ok := w.(http.Hijacker)
			if !ok {
				http.Error(w, "hijack unsupported", http.StatusInternalServerError)
				return
			}
			conn, buf, err := h.Hijack()
			if err != nil {
				return
			}
			defer conn.Close()
			_, _ = fmt.Fprintf(buf, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\n\r\n", len(payload))
			_ = buf.Flush()
			_, _ = conn.Write(payload[:128<<10])
			time.Sleep(100 * time.Millisecond)
			// conn.Close() in the defer drops the connection mid-body.
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	outPath := filepath.Join(t.TempDir(), "partial.bin")
	err := DownloadFileContext(context.Background(), false, 0, server.URL+"/file.bin", outPath, NewWriteCounter())
	if err == nil {
		t.Fatal("DownloadFileContext succeeded although the server dropped the connection mid-body")
	}
	info, err := os.Stat(outPath)
	if err != nil {
		t.Fatalf("partial file missing after a wget failure: %v", err)
	}
	if info.Size() == 0 {
		t.Fatal("partial file is empty after a wget failure; the resumable bytes were discarded")
	}
	if info.Size() > int64(len(payload)) {
		t.Fatalf("partial file is larger than the source (%d > %d)", info.Size(), len(payload))
	}
	got, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("reading the retained partial: %v", err)
	}
	if !bytes.HasPrefix(payload, got) {
		t.Fatalf("retained partial (len=%d) is not a prefix of the source; a resume would corrupt the file", len(got))
	}
}

func appendRange(mu *sync.Mutex, values *[]string, v string) {
	mu.Lock()
	defer mu.Unlock()
	*values = append(*values, v)
}

// resumeRangeStart extracts the start offset from the "bytes=START-"
// Range header the only form wget -c sends when resuming (net/http keeps
// its range parser unexported, so the test server parses the one form it
// needs).
func resumeRangeStart(value string) (int64, bool) {
	if !strings.HasPrefix(value, "bytes=") {
		return 0, false
	}
	rest := value[len("bytes="):]
	if !strings.HasSuffix(rest, "-") {
		return 0, false
	}
	start, err := strconv.ParseInt(rest[:len(rest)-1], 10, 64)
	if err != nil || start < 0 {
		return 0, false
	}
	return start, true
}

// TestDownloadFileContextCancellationKeepsPartialFile is the regression
// test for the finding that a cancelled transfer deleted its partial file,
// forcing a full re-download on retry: the partial file must survive the
// cancellation so wget -c can resume it.
func TestDownloadFileContextCancellationKeepsPartialFile(t *testing.T) {
	requireDownloadTools(t)
	payload := patternedPayload(128 << 10)
	release := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Flush half the payload, then park: the transfer stops only when
		// the caller's context is cancelled (the release at the test's end
		// keeps the handler terminating so the server close cannot wait).
		_, _ = w.Write(payload[:64<<10])
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		select {
		case <-release:
			_, _ = w.Write(payload[64<<10:])
		case <-r.Context().Done():
		}
	}))
	defer server.Close()

	outPath := filepath.Join(t.TempDir(), "partial.bin")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errc := make(chan error, 1)
	go func() {
		errc <- DownloadFileContext(ctx, false, 0, server.URL+"/file.bin", outPath, NewWriteCounter())
	}()

	// The partial file grows while the server is parked.
	deadline := time.Now().Add(15 * time.Second)
	for {
		if fi, err := os.Stat(outPath); err == nil && fi.Size() >= 4096 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("partial file never grew past 4 KiB while the transfer ran")
		}
		time.Sleep(50 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("DownloadFileContext err = %v, want context.Canceled", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("DownloadFileContext did not return after the context was cancelled")
	}

	got, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("partial file missing after cancellation: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("partial file is empty after cancellation")
	}
	if !bytes.HasPrefix(payload, got) {
		t.Fatalf("partial file (len=%d) is not a prefix of the payload", len(got))
	}
	release <- struct{}{}
}
