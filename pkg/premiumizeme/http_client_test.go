package premiumizeme

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestAllRequestsUseTheConfiguredClient is the regression test for
// finding C-4 (d): every premiumize.me request must run through the
// configured *http.Client so its timeout applies. Before the fix,
// ListFolder, GetFolders, CreateFolder, DeleteFolder, MoveItem and the
// file/zip link helpers each built a raw *http.Client{} or used
// http.DefaultClient: against an endpoint that accepts the connection and
// never answers, a reconfiguration triggered by a config save would hang
// without bound. The explicit bound below keeps the pre-fix shape failing
// fast instead of hitting the global test timeout.
func TestAllRequestsUseTheConfiguredClient(t *testing.T) {
	// The handlers park on done instead of select{}: a handler that never
	// returns keeps its connection goroutine alive and blocks
	// httptest.Server.Close in cleanup.
	done := make(chan struct{})
	stalled := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Accept the connection and never respond until the test ends.
		<-done
	}))
	t.Cleanup(func() {
		close(done)
		stalled.Close()
	})

	client := NewPremiumizemeClient("test-key")
	client.APIBaseURL = stalled.URL + "/api/"
	client.HTTPClient = &http.Client{Timeout: 200 * time.Millisecond}

	magnetFile := filepath.Join(t.TempDir(), "test.magnet")
	if err := os.WriteFile(magnetFile, []byte("magnet:?xt=urn:btih:test"), 0o600); err != nil {
		t.Fatal(err)
	}

	const (
		folderID = "main-id"
		itemID   = "item-id"
	)

	tests := map[string]func() error{
		"GetAccountInfo": func() error {
			_, err := client.GetAccountInfo()
			return err
		},
		"GetTransfers": func() error {
			_, err := client.GetTransfers()
			return err
		},
		"ListFolder": func() error {
			_, err := client.ListFolder(folderID)
			return err
		},
		"GetFolders": func() error {
			_, err := client.GetFolders()
			return err
		},
		"CreateTransfer": func() error {
			return client.CreateTransfer(magnetFile, folderID)
		},
		"DeleteTransfer": func() error {
			return client.DeleteTransfer("transfer-id")
		},
		"CreateFolder": func() error {
			parent := folderID
			_, err := client.CreateFolder("sonarr", &parent)
			return err
		},
		"DeleteFolder": func() error {
			return client.DeleteFolder(folderID)
		},
		"MoveItem": func() error {
			return client.MoveItem(itemID, folderID)
		},
		"GenerateFileLink": func() error {
			_, err := client.GenerateFileLink(itemID)
			return err
		},
		"GenerateZippedFileLink": func() error {
			_, err := client.GenerateZippedFileLink(itemID)
			return err
		},
		"GenerateZippedFolderLink": func() error {
			_, err := client.GenerateZippedFolderLink(itemID)
			return err
		},
	}

	for name, call := range tests {
		t.Run(name, func(t *testing.T) {
			start := time.Now()
			err := call()
			elapsed := time.Since(start)
			if err == nil {
				t.Fatal("request against a stalled endpoint returned nil error, want a timeout error")
			}
			if elapsed > 5*time.Second {
				t.Fatalf("request took %s, want the configured 200ms client timeout to bound it", elapsed)
			}
		})
	}
}
