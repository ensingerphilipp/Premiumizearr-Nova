package utils

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/ensingerphilipp/premiumizearr-nova/pkg/premiumizeme"
	log "github.com/sirupsen/logrus"
)

// captureLogs redirects the global logrus logger to a buffer for the duration
// of the test and restores the previous output and level afterwards.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prevOut := log.StandardLogger().Out
	prevLevel := log.GetLevel()
	log.SetOutput(&buf)
	log.SetLevel(log.InfoLevel)
	t.Cleanup(func() {
		log.SetLevel(prevLevel)
		log.SetOutput(prevOut)
	})
	return &buf
}

func TestIsDirectoryWriteable(t *testing.T) {
	buf := captureLogs(t)

	dir := t.TempDir()
	if !IsDirectoryWriteable(dir) {
		t.Fatalf("IsDirectoryWriteable(%q) = false, want true", dir)
	}

	missing := dir + "/does-not-exist"
	if IsDirectoryWriteable(missing) {
		t.Fatalf("IsDirectoryWriteable(%q) = true, want false", missing)
	}

	out := buf.String()
	if !strings.Contains(out, missing) {
		t.Errorf("log for the missing directory does not contain the path %q:\n%s", missing, out)
	}
	if strings.Contains(out, "%!") {
		t.Errorf("log contains a format placeholder artifact:\n%s", out)
	}
}

func TestStripDownloadTypesExtention(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "strips .nzb suffix", input: "file.nzb", want: "file"},
		{name: "strips .magnet suffix", input: "file.magnet", want: "file"},
		{name: "strips .torrent suffix", input: "file.torrent", want: "file"},
		{name: "strips only the trailing suffix", input: "file.tar.nzb", want: "file.tar"},
		{name: "keeps unknown suffix", input: "file.txt", want: "file.txt"},
		{name: "keeps uppercase .NZB", input: "file.NZB", want: "file.NZB"},
		{name: "keeps uppercase .MAGNET", input: "file.MAGNET", want: "file.MAGNET"},
		{name: "keeps uppercase .TORRENT", input: "file.TORRENT", want: "file.TORRENT"},
		{name: "keeps extension in the middle", input: "file.nzb.bak", want: "file.nzb.bak"},
		{name: "keeps extension text without the dot", input: "filenzb", want: "filenzb"},
		{name: "keeps empty string", input: "", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := StripDownloadTypesExtention(tt.input); got != tt.want {
				t.Errorf("StripDownloadTypesExtention(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestStripMediaTypesExtention(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "strips .mkv suffix", input: "movie.mkv", want: "movie"},
		{name: "strips .mp4 suffix", input: "movie.mp4", want: "movie"},
		{name: "strips .avi suffix", input: "movie.avi", want: "movie"},
		{name: "strips .ts suffix", input: "movie.ts", want: "movie"},
		{name: "strips .webm suffix", input: "movie.webm", want: "movie"},
		{name: "strips .m2ts suffix", input: "movie.m2ts", want: "movie"},
		{name: "strips only the trailing suffix", input: "movie.tar.mkv", want: "movie.tar"},
		{name: "keeps unknown suffix", input: "movie.bin", want: "movie.bin"},
		{name: "keeps uppercase .MKV", input: "movie.MKV", want: "movie.MKV"},
		{name: "keeps uppercase .MP4", input: "movie.MP4", want: "movie.MP4"},
		{name: "keeps extension in the middle", input: "movie.mkv.bak", want: "movie.mkv.bak"},
		{name: "keeps extension text without the dot", input: "moviemkv", want: "moviemkv"},
		{name: "keeps empty string", input: "", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := StripMediaTypesExtention(tt.input); got != tt.want {
				t.Errorf("StripMediaTypesExtention(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestStringInSlice(t *testing.T) {
	tests := []struct {
		name   string
		needle string
		list   []string
		want   int
	}{
		{name: "returns the first matching index", needle: "b", list: []string{"a", "b", "c"}, want: 1},
		{name: "returns index zero for the first element", needle: "a", list: []string{"a", "b"}, want: 0},
		{name: "duplicates return the first index", needle: "x", list: []string{"y", "x", "x"}, want: 1},
		{name: "missing value returns -1", needle: "z", list: []string{"a", "b"}, want: -1},
		{name: "nil slice returns -1", needle: "a", list: nil, want: -1},
		{name: "empty slice returns -1", needle: "a", list: []string{}, want: -1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := StringInSlice(tt.needle, tt.list); got != tt.want {
				t.Errorf("StringInSlice(%q, %v) = %d, want %d", tt.needle, tt.list, got, tt.want)
			}
		})
	}
}

func TestEnvOrDefault(t *testing.T) {
	const (
		setVarName   = "PREMIUMIZEARR_NOVA_TEST_ENV_DO_NOT_SET"
		unsetVarName = "PREMIUMIZEARR_NOVA_TEST_ENV_NEVER_SET"
	)

	tests := []struct {
		name    string
		varName string
		value   string
		unset   bool
		def     string
		want    string
	}{
		{
			name:    "unset env returns the default",
			varName: unsetVarName,
			unset:   true,
			def:     "fallback",
			want:    "fallback",
		},
		{
			name:    "empty env returns the default",
			varName: setVarName,
			value:   "",
			def:     "fallback",
			want:    "fallback",
		},
		{
			name:    "non-empty env is returned unchanged",
			varName: setVarName,
			value:   "from-env",
			def:     "fallback",
			want:    "from-env",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.unset {
				// unsetVarName is unique to this test, so unsetting it guarantees
				// the unset branch without relying on the host environment.
				os.Unsetenv(tt.varName)
			} else {
				t.Setenv(tt.varName, tt.value)
			}
			if got := EnvOrDefault(tt.varName, tt.def); got != tt.want {
				t.Errorf("EnvOrDefault(%q, %q) = %q, want %q", tt.varName, tt.def, got, tt.want)
			}
		})
	}
}

// subfolderStub serves the premiumize.me endpoints GetOrCreateSubfolderID
// uses (folder/list, folder/create) with scripted list results and create
// responses, for the R1-7 list-then-create race tests.
type subfolderStub struct {
	server      *httptest.Server
	mu          sync.Mutex
	listResults [][]premiumizeme.Item // the nth list call returns the nth entry (clamped to the last)
	listCount   int
	createNames []string
	createBody  string
}

func newSubfolderStub(t *testing.T, listResults [][]premiumizeme.Item, createBody string) *subfolderStub {
	t.Helper()
	s := &subfolderStub{listResults: listResults, createBody: createBody}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		switch r.URL.Path {
		case "/api/folder/list":
			result := s.listResults[len(s.listResults)-1]
			if s.listCount < len(s.listResults) {
				result = s.listResults[s.listCount]
			}
			s.listCount++
			data, _ := json.Marshal(result)
			_, _ = w.Write([]byte(`{"status":"success","content":` + string(data) + `}`))
		case "/api/folder/create":
			s.createNames = append(s.createNames, r.URL.Query().Get("name"))
			_, _ = w.Write([]byte(s.createBody))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.server.Close)
	return s
}

func newSubfolderTestClient(t *testing.T, s *subfolderStub) *premiumizeme.Premiumizeme {
	t.Helper()
	client := premiumizeme.NewPremiumizemeClient("test-key")
	client.APIBaseURL = s.server.URL + "/api/"
	client.HTTPClient = s.server.Client()
	return &client
}

// TestGetOrCreateSubfolderIDAdoptsConcurrentCreator is the regression test
// for finding R1-7: a concurrent caller (the manager's resolveArrFolders
// and the watcher's resolveSingleArrFolder resolve the same slug on
// different goroutines) may create the folder in the window between the
// list and the create. The "already exists" error must trigger one
// re-list and adoption of the winner's folder instead of surfacing the
// error.
func TestGetOrCreateSubfolderIDAdoptsConcurrentCreator(t *testing.T) {
	s := newSubfolderStub(t, [][]premiumizeme.Item{
		nil, // list at call time: the folder is missing
		{{ID: "winner-id", Name: "sonarr", Type: "folder"}}, // re-list after the conflict: the winner is present
	}, `{"status":"error","message":"This folder already exists."}`)
	client := newSubfolderTestClient(t, s)

	id, err := GetOrCreateSubfolderID(client, "main-id", "sonarr")
	if err != nil {
		t.Fatalf("GetOrCreateSubfolderID: %v; a concurrent creation must be adopted, not surfaced", err)
	}
	if id != "winner-id" {
		t.Fatalf("GetOrCreateSubfolderID = %q, want winner-id (the concurrently created folder)", id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listCount != 2 {
		t.Fatalf("folder/list called %d times, want 2 (initial list + one re-list)", s.listCount)
	}
	if len(s.createNames) != 1 {
		t.Fatalf("folder/create called %d times, want 1", len(s.createNames))
	}
}

func TestGetOrCreateSubfolderIDCreatesWhenMissing(t *testing.T) {
	s := newSubfolderStub(t, [][]premiumizeme.Item{nil}, `{"status":"success","id":"new-id"}`)
	client := newSubfolderTestClient(t, s)

	id, err := GetOrCreateSubfolderID(client, "main-id", "sonarr")
	if err != nil {
		t.Fatalf("GetOrCreateSubfolderID: %v", err)
	}
	if id != "new-id" {
		t.Fatalf("GetOrCreateSubfolderID = %q, want new-id", id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listCount != 1 {
		t.Fatalf("folder/list called %d times, want 1 (no retry on a successful create)", s.listCount)
	}
	if len(s.createNames) != 1 || s.createNames[0] != "sonarr" {
		t.Fatalf("folder/create names = %v, want [sonarr]", s.createNames)
	}
}

func TestGetOrCreateSubfolderIDReturnsExistingWithoutCreating(t *testing.T) {
	s := newSubfolderStub(t, [][]premiumizeme.Item{{{ID: "existing-id", Name: "sonarr", Type: "folder"}}}, `{"status":"success","id":"new-id"}`)
	client := newSubfolderTestClient(t, s)

	id, err := GetOrCreateSubfolderID(client, "main-id", "sonarr")
	if err != nil {
		t.Fatalf("GetOrCreateSubfolderID: %v", err)
	}
	if id != "existing-id" {
		t.Fatalf("GetOrCreateSubfolderID = %q, want existing-id", id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.createNames) != 0 {
		t.Fatalf("folder/create called for a folder the list already found: %v", s.createNames)
	}
}

func TestGetOrCreateSubfolderIDPropagatesUnrelatedCreateErrors(t *testing.T) {
	s := newSubfolderStub(t, [][]premiumizeme.Item{nil}, `{"status":"error","message":"quota exceeded"}`)
	client := newSubfolderTestClient(t, s)

	id, err := GetOrCreateSubfolderID(client, "main-id", "sonarr")
	if err == nil {
		t.Fatalf("GetOrCreateSubfolderID = %q, want an error", id)
	}
	if !strings.Contains(err.Error(), "quota exceeded") {
		t.Fatalf("error %q does not carry the API message", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listCount != 1 {
		t.Fatalf("folder/list called %d times, want 1 (no re-list for non-conflict errors)", s.listCount)
	}
	if len(s.createNames) != 1 {
		t.Fatalf("folder/create called %d times, want 1", len(s.createNames))
	}
}
