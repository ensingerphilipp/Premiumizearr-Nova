package directclient

// Round-2 regression tests for the two handler findings about spooled
// multipart temp files: a rejected oversized upload must not leave the
// parser's temp file behind (qBittorrent add: R1-25; SABnzbd addfile:
// R1-4).

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// spooledTempFiles lists the spooled multipart temp files present in the
// directory the parser pools into. The test redirects the pool through
// TMPDIR to a private directory, so the set is closed and the diff is
// exact.
func spooledTempFiles(dir string) []string {
	entries, _ := os.ReadDir(dir)
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "multipart-") {
			out = append(out, e.Name())
		}
	}
	return out
}

// TestQBitAddOversizedMultipartLeavesNoSpooledTemp is the round-2
// regression test for the finding that a rejected oversized multipart
// upload spooled the file part to a temp file the handler never removed:
// the size limit is a spool THRESHOLD, so the parse succeeds for an
// oversized part and only the later size check rejects it.
func TestQBitAddOversizedMultipartLeavesNoSpooledTemp(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("TMPDIR", tempDir)
	const bigFile = 64<<20 + 1 // the pinned oversized shape
	h := NewQBitHandler(&qbitFake{}, "arr", "secret")
	cc := login(t, h)
	var body strings.Builder
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("category", "tv")
	part, err := mw.CreateFormFile("torrents", "big.torrent")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(bytes.Repeat([]byte("x"), bigFile)); err != nil {
		t.Fatal(err)
	}
	_ = mw.Close()
	w := request(h, "POST", "/api/v2/torrents/add", body.String(), mw.FormDataContentType(), cc)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "exceeds 64 MiB limit") {
		t.Fatalf("error body = %q, want size limit error", w.Body.String())
	}
	if leaked := spooledTempFiles(tempDir); len(leaked) != 0 {
		t.Fatalf("the rejected upload left spooled temp files in the parser's pool: %v", leaked)
	}
}

// TestSABAddfileSpooledTempRemovedOnSizeReject is the round-2 regression
// test for the addfile twin of the same finding: a file part strictly
// larger than the per-file limit (but a whole body still inside the
// up-front bound) spools to temp storage, and the size rejection must
// remove the spool instead of leaving it in the temp directory.
func TestSABAddfileSpooledTempRemovedOnSizeReject(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("TMPDIR", tempDir)
	b := &fakeSABBackend{}
	h := NewSABHandler(b, "key")
	// The file part is 64.5 MiB: above the per-file limit, below the
	// whole-body bound, so the parse (and the spool) succeeds and only
	// the per-file check can reject it.
	const fileSize = nzbFileLimit + (1<<20)/2
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("name", "Fat.nzb")
	if err != nil {
		t.Fatal(err)
	}
	chunk := bytes.Repeat([]byte("0123456789abcdef"), (1<<20)/16)
	var written int64
	for written < int64(fileSize) {
		n := int64(len(chunk))
		if n > int64(fileSize)-written {
			n = int64(fileSize) - written
		}
		_, _ = part.Write(chunk[:n])
		written += n
	}
	_ = mw.Close()
	r := httptest.NewRequest(http.MethodPost, "/api?mode=addfile&apikey=key", bytes.NewReader(body.Bytes()))
	r.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if !strings.Contains(w.Body.String(), "NZB exceeds 64 MiB limit") {
		t.Fatalf("rejected upload: body = %s", w.Body.String())
	}
	if b.added.ID != "" {
		t.Fatalf("oversized file part reached the backend: %#v", b.added)
	}
	if leaked := spooledTempFiles(tempDir); len(leaked) != 0 {
		t.Fatalf("the size rejection left the spooled temp file behind: %v", leaked)
	}
}
