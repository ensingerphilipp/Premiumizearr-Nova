package service

// Round-2 regression test for the finding that the config save endpoint
// wrote its success response only AFTER the update, and the update's
// in-handler web-server restart (a rotated DirectClientAPIKey, or a
// BindIP/BindPort/WebRoot change) closed the very connection serving the
// request: the client saw a truncated or failed response for a change that
// had persisted, and the UI reported the save as failed. The response is
// now written and FLUSHED to the socket first — with a pinned
// Content-Length so it is self-contained instead of chunked, whose
// terminator would only be emitted when the handler returns — before the
// update runs; a small write alone would still sit in the server's
// buffered writer when the restart closes the connection.

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ensingerphilipp/premiumizearr-nova/internal/config"
)

func TestConfigSaveResponseSurvivesInHandlerRestart(t *testing.T) {
	dir := t.TempDir()
	// Start() parses the UI template from the working directory.
	if err := os.MkdirAll(filepath.Join(dir, "static"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "static", "index.html"), []byte("<html><body>ui</body></html>"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	// A free port for the REAL listener: the assertion is about what the
	// client socket sees, not a recorder.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	var ws WebServerService
	ws = WebServerService{}.New()
	cfg, err := config.LoadOrCreateConfig(dir, func(oldConfig, newConfig config.Config) {
		ws.ConfigUpdatedCallback(oldConfig, newConfig)
	})
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.BindIP = "127.0.0.1"
	cfg.BindPort = strconv.Itoa(port)
	ws.Init(nil, nil, nil, &cfg)
	ws.Start()

	// A full UI-shaped payload that ROTATES the direct client key: the
	// callback's comparison restarts the web server inside this handler,
	// which is what used to kill the response.
	original, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(original, &payload); err != nil {
		t.Fatal(err)
	}
	payload["DirectClientAPIKey"] = json.RawMessage(`"rotated-direct-key-2"`)
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := http.Post("http://127.0.0.1:"+strconv.Itoa(port)+"/api/config", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("the client lost the save request: %v", err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("the save response was truncated by the in-handler restart: %v", err)
	}
	if string(got) != `{"succeeded":true,"status":"Config updated"}` {
		t.Fatalf("save response = %q, want the complete success body", got)
	}
	// And the change actually persisted: the config on disk carries the
	// rotated key. The update (and its persist) runs on the server after
	// the response was flushed, so the client already holds the success
	// reply while the save may still be in flight; wait for the file to
	// converge before asserting on it, so the test does not race its own
	// temp-dir cleanup.
	cfgPath := filepath.Join(dir, "config.yaml")
	deadline := time.Now().Add(5 * time.Second)
	var data []byte
	for {
		var err error
		data, err = os.ReadFile(cfgPath)
		if err == nil && strings.Contains(string(data), "rotated-direct-key-2") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("rotated key not persisted to config.yaml; last content:\n%s", data)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
