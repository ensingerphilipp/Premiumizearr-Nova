package premiumizeme

import (
	"errors"
	"fmt"
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

func TestAPITransportErrorsRedactKeys(t *testing.T) {
	key := "test secret/+"
	client := NewPremiumizemeClient(key)
	file := createTempTransferFile(t, ".magnet", "magnet:?xt=urn:btih:test")
	file.Close()
	old := http.DefaultTransport
	http.DefaultTransport = reviewTransport(func(r *http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("connection failed for %s (key %s)", r.URL, key)
	})
	defer func() { http.DefaultTransport = old }()
	cases := map[string]func() error{
		"transfer/create": func() error { return client.CreateTransfer(file.Name(), "folder") },
		"transfer/list":   func() error { _, err := client.GetTransfers(); return err },
		"folder/list":     func() error { _, err := client.ListFolder("folder"); return err },
		"folders":         func() error { _, err := client.GetFolders(); return err },
		"folder/delete":   func() error { return client.DeleteFolder("folder") },
		"item/move":       func() error { return client.MoveItem("item", "folder") },
		"folder/create":   func() error { _, err := client.CreateFolder("folder", nil); return err },
		"transfer/delete": func() error { return client.DeleteTransfer("transfer") },
		"zip/file":        func() error { _, err := client.GenerateZippedFileLink("file"); return err },
		"zip/folder":      func() error { _, err := client.GenerateZippedFolderLink("folder"); return err },
		"item/details":    func() error { _, err := client.GenerateFileLink("file"); return err },
	}
	for name, call := range cases {
		t.Run(name, func(t *testing.T) {
			err := call()
			if err == nil {
				t.Fatal("expected transport error")
			}
			for _, format := range []string{"%s", "%v", "%+v", "%#v"} {
				message := fmt.Sprintf(format, err)
				for _, secret := range []string{key, url.QueryEscape(key), url.PathEscape(key)} {
					if strings.Contains(message, secret) {
						t.Errorf("error formatting %s exposes API key", format)
					}
				}
			}
		})
	}
}

type reviewTransport func(*http.Request) (*http.Response, error)

func (f reviewTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestTransferFailuresHaveStableClassification(t *testing.T) {
	for _, test := range []struct {
		message string
		kind    error
	}{
		{"You already added this job.", ErrTransferAlreadyExists},
		{" YOU ALREADY ADDED THIS JOB! ", ErrTransferAlreadyExists},
		{"Limit of transfers reached!", ErrTransferLimitReached},
		{"account_limit_reached", ErrTransferLimitReached},
		{"unknown failure", nil},
	} {
		t.Run(test.message, func(t *testing.T) {
			pm := NewPremiumizemeClient("dummy-key")
			err := pm.transferFailure(test.message)
			if err.Error() != test.message {
				t.Fatal("displayed provider message changed")
			}
			wrapped := fmt.Errorf("upload failed: %w", err)
			if test.kind != nil && !errors.Is(wrapped, test.kind) {
				t.Fatal("wrapped error lost classification")
			}
			if test.kind == nil && (errors.Is(wrapped, ErrTransferAlreadyExists) || errors.Is(wrapped, ErrTransferLimitReached)) {
				t.Fatal("unknown failure misclassified")
			}
		})
	}
}
