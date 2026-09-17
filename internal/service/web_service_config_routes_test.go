package service

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"strings"
	"testing"

	"github.com/ensingerphilipp/premiumizearr-nova/internal/config"
)

// TestConfigHandlerGetNeverEmitsNullArrs reproduces the issue where a legacy
// config file without an Arrs section left the in-memory config with a nil
// Arrs slice, so GET /api/config served "Arrs": null and the Config page
// crashed. The config is built through the product load path
// (LoadOrCreateConfig), then served by the handler.
func TestConfigHandlerGetNeverEmitsNullArrs(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(path.Join(dir, "config.yaml"), []byte("PremiumizemeAPIKey: xxxxxxxxx\n"), 0644); err != nil {
		t.Fatalf("write config.yaml: %v", err)
	}

	cfg, err := config.LoadOrCreateConfig(dir, func(oldConfig config.Config, newConfig config.Config) {})
	if err != nil {
		t.Fatalf("LoadOrCreateConfig() error = %v, want nil", err)
	}

	s := &WebServerService{config: &cfg}
	request := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	response := httptest.NewRecorder()
	s.ConfigHandler(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("ConfigHandler() status = %d, want %d", response.Code, http.StatusOK)
	}

	body := response.Body.String()
	if strings.Contains(body, `"Arrs":null`) {
		t.Fatalf("GET /api/config emitted a null Arrs field: %s", body)
	}

	var served struct {
		Arrs []config.ArrConfig `json:"Arrs"`
	}
	if err := json.Unmarshal([]byte(body), &served); err != nil {
		t.Fatalf("unmarshal response %q: %v", body, err)
	}
	if served.Arrs == nil {
		t.Fatalf("served Arrs decoded as nil: %s", body)
	}
}
