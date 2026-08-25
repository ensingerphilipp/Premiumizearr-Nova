package service

import "testing"

func TestResolveTargetFolderID(t *testing.T) {
	blackholeDir := "/blackhole"
	mainFolderID := "main-folder-id"
	arrFolders := map[string]string{
		"sonarr": "sonarr-folder-id",
	}

	tests := []struct {
		name     string
		filePath string
		wantID   string
		wantOK   bool
		wantSlug string
	}{
		{"file in main folder", "/blackhole/movie.torrent", mainFolderID, true, ""},
		{"file in resolved Arr subfolder", "/blackhole/sonarr/episode.nzb", "sonarr-folder-id", true, "sonarr"},
		{"file in unresolved subfolder", "/blackhole/radarr/movie.magnet", "", false, "radarr"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, ok, slug := resolveTargetFolderID(tt.filePath, blackholeDir, mainFolderID, arrFolders)
			if id != tt.wantID || ok != tt.wantOK || slug != tt.wantSlug {
				t.Fatalf("resolveTargetFolderID(%q) = (%q, %v, %q), want (%q, %v, %q)",
					tt.filePath, id, ok, slug, tt.wantID, tt.wantOK, tt.wantSlug)
			}
		})
	}
}
