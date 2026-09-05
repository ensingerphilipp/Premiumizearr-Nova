package premiumizeme

import (
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCreateTransferRequestsIncludeFolderID(t *testing.T) {
	tests := []struct {
		name       string
		extension  string
		content    string
		createFunc func(*os.File, *url.URL, string) (*http.Request, error)
	}{
		{
			name:       "nzb",
			extension:  ".nzb",
			content:    "<nzb></nzb>",
			createFunc: createNZBRequest,
		},
		{
			name:       "magnet",
			extension:  ".magnet",
			content:    "magnet:?xt=urn:btih:123",
			createFunc: createMagnetRequest,
		},
		{
			name:       "torrent",
			extension:  ".torrent",
			content:    "torrent-bytes",
			createFunc: createTorrentRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			file := createTempTransferFile(t, tt.extension, tt.content)
			defer file.Close()

			requestURL := &url.URL{Scheme: "https", Host: "example.com", Path: "/api/transfer/create"}
			req, err := tt.createFunc(file, requestURL, "target-folder-id")
			if err != nil {
				t.Fatalf("create request: %v", err)
			}

			fields := readMultipartFields(t, req)
			if fields["folder_id"] != "target-folder-id" {
				t.Fatalf("folder_id = %q, want %q", fields["folder_id"], "target-folder-id")
			}
			if fields["src"] != tt.content {
				t.Fatalf("src = %q, want %q", fields["src"], tt.content)
			}
		})
	}
}

func createTempTransferFile(t *testing.T, extension string, content string) *os.File {
	t.Helper()

	path := filepath.Join(t.TempDir(), "transfer"+extension)
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("write temp transfer file: %v", err)
	}

	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open temp transfer file: %v", err)
	}

	return file
}

func readMultipartFields(t *testing.T, req *http.Request) map[string]string {
	t.Helper()

	contentType := req.Header.Get("Content-Type")
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		t.Fatalf("parse content type %q: %v", contentType, err)
	}
	if mediaType != "multipart/form-data" {
		t.Fatalf("content type = %q, want multipart/form-data", mediaType)
	}

	reader := multipart.NewReader(req.Body, params["boundary"])
	fields := make(map[string]string)
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read multipart part: %v", err)
		}

		value, err := io.ReadAll(part)
		if err != nil {
			t.Fatalf("read multipart value: %v", err)
		}
		fields[part.FormName()] = strings.TrimSuffix(string(value), "\n")
	}

	return fields
}
