package utils

import (
	"bytes"
	"os"
	"strings"
	"testing"

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
