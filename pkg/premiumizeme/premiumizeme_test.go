package premiumizeme

import (
	"context"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCreateTransferFromBytes(t *testing.T) {
	tests := []struct {
		kind TransferSourceKind
		name string
		data string
	}{
		{TransferSourceMagnet, "", "magnet:?xt=urn:btih:123"},
		{TransferSourceTorrent, "release.torrent", "torrent-bytes"},
		{TransferSourceNZB, "release.nzb", "<nzb></nzb>"},
	}
	for _, tt := range tests {
		t.Run(string(tt.kind), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/api/transfer/create" {
					t.Errorf("request = %s %s", r.Method, r.URL.Path)
				}
				if r.URL.Query().Get("apikey") != "test-key" {
					t.Errorf("missing API key")
				}
				mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
				if err != nil || mediaType != "multipart/form-data" {
					t.Errorf("content type = %q, err = %v", mediaType, err)
					return
				}
				reader := multipart.NewReader(r.Body, params["boundary"])
				parts := map[string]string{}
				filenames := map[string]string{}
				for {
					part, err := reader.NextPart()
					if err == io.EOF {
						break
					}
					if err != nil {
						t.Errorf("read multipart: %v", err)
						return
					}
					value, err := io.ReadAll(part)
					if err != nil {
						t.Errorf("read part: %v", err)
						return
					}
					parts[part.FormName()] = string(value)
					filenames[part.FormName()] = part.FileName()
				}
				if parts["src"] != tt.data || parts["folder_id"] != "folder-1" {
					t.Errorf("multipart fields = %#v", parts)
				}
				if got := filenames["src"]; got != tt.name {
					t.Errorf("src filename = %q, want %q", got, tt.name)
				}
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"status":"success","id":"transfer-123","name":"release","type":"torrent"}`)
			}))
			defer server.Close()
			client := NewPremiumizemeClient("test-key")
			client.APIBaseURL = server.URL + "/api/"
			got, err := client.CreateTransferFromBytes(context.Background(), tt.kind, []byte(tt.data), tt.name, "folder-1")
			if err != nil {
				t.Fatal(err)
			}
			if got.ID != "transfer-123" || got.Status != "success" {
				t.Fatalf("response = %#v", got)
			}
		})
	}
}

func TestCreateTransferFromBytesValidationAndResponseErrors(t *testing.T) {
	client := NewPremiumizemeClient("key")
	for _, tc := range []struct {
		kind TransferSourceKind
		data []byte
		name string
	}{
		{TransferSourceKind("unknown"), []byte("data"), "x"},
		{TransferSourceTorrent, nil, "x.torrent"},
		{TransferSourceMagnet, make([]byte, 16*1024+1), ""},
		{TransferSourceTorrent, []byte("data"), ""},
	} {
		if _, err := client.CreateTransferFromBytes(context.Background(), tc.kind, tc.data, tc.name, ""); err == nil {
			t.Errorf("expected validation error for kind %q", tc.kind)
		}
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"status":"error","message":"bad request"}`)
	}))
	defer server.Close()
	client.APIBaseURL = server.URL + "/api/"
	if _, err := client.CreateTransferFromBytes(context.Background(), TransferSourceMagnet, []byte("magnet:?x"), "", ""); err == nil || !strings.Contains(err.Error(), "bad request") {
		t.Fatalf("response error = %v", err)
	}
}

func TestCreateTransferFromBytesRedactsAPIKeyOnRequestError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	baseURL := server.URL + "/api/"
	server.Close()
	client := NewPremiumizemeClient("secret key+value")
	client.APIBaseURL = baseURL
	_, err := client.CreateTransferFromBytes(context.Background(), TransferSourceMagnet, []byte("magnet:?x"), "", "")
	if err == nil || strings.Contains(err.Error(), client.APIKey) || !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("request error = %v", err)
	}
}

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

func TestAccountErrorIncludesRedactedReason(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"error","message":"invalid key secret-key"}`))
	}))
	defer server.Close()
	client := NewPremiumizemeClient("secret-key")
	client.APIBaseURL = server.URL
	_, err := client.GetAccountInfo()
	if err == nil || !strings.Contains(err.Error(), "invalid key") || strings.Contains(err.Error(), "secret-key") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestRequestErrorsRedactsAPIKey verifies that network-level request errors
// (a *url.Error whose message embeds the request URL, and therefore the
// apikey query parameter) are returned with the API key redacted, so they
// can be logged without leaking the key. A key with URL-special characters
// is exercised too, so the QueryEscape/PathEscape replacement arms run:
// the request URL carries the key in its escaped form.
func TestRequestErrorsRedactsAPIKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	baseURL := server.URL + "/api/"
	// Closing the server makes the request fail at the network level with
	// a *url.Error containing the full request URL.
	server.Close()

	for _, apiKey := range []string{"super-secret-api-key", "my key+with/special&chars"} {
		client := NewPremiumizemeClient(apiKey)
		client.APIBaseURL = baseURL

		operations := []struct {
			name string
			run  func() error
		}{
			{
				name: "GetTransfers",
				run: func() error {
					_, err := client.GetTransfers()
					return err
				},
			},
			{
				name: "DeleteTransfer",
				run: func() error {
					return client.DeleteTransfer("t1")
				},
			},
		}
		for _, op := range operations {
			err := op.run()
			if err == nil || !strings.Contains(err.Error(), "[REDACTED]") {
				t.Fatalf("%s with key %q: error = %v, want a network error containing [REDACTED]", op.name, apiKey, err)
			}
			for _, secret := range []string{apiKey, url.QueryEscape(apiKey), url.PathEscape(apiKey)} {
				if secret != "" && strings.Contains(err.Error(), secret) {
					t.Fatalf("%s with key %q: error leaks the API key (form %q): %v", op.name, apiKey, secret, err)
				}
			}
		}
	}
}
