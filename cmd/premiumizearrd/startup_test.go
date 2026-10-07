package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ensingerphilipp/premiumizearr-nova/internal/config"
	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v2"
)

// Stall cleanup at its entry point to simulate an uninterruptible filesystem
// call. Use a subprocess because App.Start and the stalled worker never return.
type stalledCleanupHook struct{}

func (stalledCleanupHook) Levels() []log.Level { return []log.Level{log.InfoLevel} }
func (stalledCleanupHook) Fire(entry *log.Entry) error {
	if entry.Message == "Cleaning download directory - deleting files older than 4 days" {
		fmt.Println("startup-cleanup-blocked")
		select {}
	}
	return nil
}

type startupTransport struct{}

func (startupTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	// The blackhole watcher resolves its cloud folder before Run starts.
	// No test should contact the real Premiumize API.
	if r.URL.Path != "/api/folder/list" {
		return nil, fmt.Errorf("unexpected startup API path %s", r.URL.Path)
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body: io.NopCloser(strings.NewReader(
			`{"status":"success","content":[{"id":"folder","name":"arrDownloads","type":"folder"}]}`)),
		Request: r,
	}, nil
}

func TestHTTPAvailableDuringStartupCleanup(t *testing.T) {
	if os.Getenv("PREMIUMIZEARR_TEST_STALLED_CLEANUP") == "1" {
		http.DefaultTransport = startupTransport{}
		log.AddHook(stalledCleanupHook{})
		if err := (&App{}).Start("info", ".", "."); err != nil {
			t.Fatal(err)
		}
		return
	}

	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "static"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "static", "index.html"), []byte("<html>ready</html>"), 0644); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := ln.Addr().String()
	_, port, err := net.SplitHostPort(address)
	ln.Close()
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		Arrs:                         []config.ArrConfig{},
		BindIP:                       "127.0.0.1",
		BindPort:                     port,
		DownloadsDirectory:           filepath.Join(dir, "unavailable-downloads"),
		BlackholeDirectory:           dir,
		TransferDirectory:            "arrDownloads",
		PollBlackholeDirectory:       true,
		PollBlackholeIntervalMinutes: 60,
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), data, 0644); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestHTTPAvailableDuringStartupCleanup$")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "PREMIUMIZEARR_TEST_STALLED_CLEANUP=1")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	blocked := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		var output strings.Builder
		for scanner.Scan() {
			output.WriteString(scanner.Text() + "\n")
			if scanner.Text() == "startup-cleanup-blocked" {
				blocked <- nil
				return
			}
		}
		blocked <- fmt.Errorf("cleanup did not start: %v\n%s", scanner.Err(), output.String())
	}()
	select {
	case err := <-blocked:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("startup did not reach cleanup")
	}
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + address + "/api/config")
	if err != nil {
		t.Fatalf("HTTP must remain available while startup cleanup is blocked: %v", err)
	}
	defer resp.Body.Close()
	var got config.Config
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || got.DownloadsDirectory != cfg.DownloadsDirectory {
		t.Fatalf("config API while cleanup is blocked: status=%d downloads=%q", resp.StatusCode, got.DownloadsDirectory)
	}
}
