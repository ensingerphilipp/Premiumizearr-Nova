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

// newConfigRouteTestService builds a WebServerService whose config saves to a
// temp dir: LoadOrCreateConfig sets the config location and installs a no-op
// callback, so UpdateConfig's Save() never touches the repository.
func newConfigRouteTestService(t *testing.T) (*WebServerService, *config.Config) {
	t.Helper()
	cfg, err := config.LoadOrCreateConfig(t.TempDir(), func(oldConfig, newConfig config.Config) {})
	if err != nil {
		t.Fatalf("LoadOrCreateConfig: %v", err)
	}
	ws := WebServerService{}.New()
	ws.Init(nil, nil, nil, &cfg)
	return &ws, &cfg
}

// UI-shaped payload with every config field present, as submitted by
// Config.svelte after the "Simultaneous Downloads" input has been cleared
// (carbon-components-svelte binds null for a cleared number input).
const nullSimultaneousDownloadsPayload = `{"PremiumizemeAPIKey":"xxxxxxxxx","Arrs":[],"BlackholeDirectory":"/blackhole","PollBlackholeDirectory":false,"PollBlackholeIntervalMinutes":10,"DownloadsDirectory":"/downloads","TransferDirectory":"arrDownloads","BindIP":"0.0.0.0","BindPort":"8182","WebRoot":"","SimultaneousDownloads":null,"DownloadSpeedLimit":100,"EnableTlsCheck":false,"TransferOnlyMode":false,"ArrHistoryUpdateIntervalSeconds":20,"ErroredTransferDeleteGracePeriodSeconds":300}`

// Same payload with explicit zeros: 0 is a legitimate value the user can
// still save (e.g. speed limit 0 = unlimited).
const zeroNumericFieldsPayload = `{"PremiumizemeAPIKey":"xxxxxxxxx","Arrs":[],"BlackholeDirectory":"/blackhole","PollBlackholeDirectory":false,"PollBlackholeIntervalMinutes":0,"DownloadsDirectory":"/downloads","TransferDirectory":"arrDownloads","BindIP":"0.0.0.0","BindPort":"8182","WebRoot":"","SimultaneousDownloads":0,"DownloadSpeedLimit":0,"EnableTlsCheck":false,"TransferOnlyMode":false,"ArrHistoryUpdateIntervalSeconds":0,"ErroredTransferDeleteGracePeriodSeconds":0}`

// TestConfigHandlerRejectsNullNumericField is the regression test for issue
// #89: a cleared numeric UI input sends null, which Go's json decoder treats
// as a no-op for non-pointer int fields, so the whole-struct replace saved 0
// while answering succeeded: true.
func TestConfigHandlerRejectsNullNumericField(t *testing.T) {
	ws, cfg := newConfigRouteTestService(t)

	req := httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(nullSimultaneousDownloadsPayload))
	rec := httptest.NewRecorder()
	ws.ConfigHandler(rec, req)

	var resp ConfigChangeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshaling response %q: %v", rec.Body.String(), err)
	}
	if resp.Succeeded {
		t.Errorf("succeeded = true, want false for null SimultaneousDownloads")
	}
	if !strings.Contains(resp.Status, "SimultaneousDownloads") {
		t.Errorf("status = %q, want it to name the rejected field", resp.Status)
	}
	if cfg.SimultaneousDownloads != 5 {
		t.Errorf("SimultaneousDownloads = %d, want 5 (rejected payload must not replace the config)", cfg.SimultaneousDownloads)
	}
}

// TestConfigHandlerAcceptsExplicitZeroNumericFields guards the boundary of
// the fix: the payload check must reject null only, not explicit 0.
func TestConfigHandlerAcceptsExplicitZeroNumericFields(t *testing.T) {
	ws, cfg := newConfigRouteTestService(t)

	req := httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(zeroNumericFieldsPayload))
	rec := httptest.NewRecorder()
	ws.ConfigHandler(rec, req)

	var resp ConfigChangeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshaling response %q: %v", rec.Body.String(), err)
	}
	if !resp.Succeeded {
		t.Fatalf("succeeded = false, want true for explicit zero values: %s", resp.Status)
	}
	if cfg.PollBlackholeIntervalMinutes != 0 || cfg.SimultaneousDownloads != 0 || cfg.DownloadSpeedLimit != 0 ||
		cfg.ArrHistoryUpdateIntervalSeconds != 0 || cfg.ErroredTransferDeleteGracePeriodSeconds != 0 {
		t.Errorf("explicit zero values were not saved as-is: PollBlackholeIntervalMinutes=%d SimultaneousDownloads=%d DownloadSpeedLimit=%d ArrHistoryUpdateIntervalSeconds=%d ErroredTransferDeleteGracePeriodSeconds=%d",
			cfg.PollBlackholeIntervalMinutes, cfg.SimultaneousDownloads, cfg.DownloadSpeedLimit,
			cfg.ArrHistoryUpdateIntervalSeconds, cfg.ErroredTransferDeleteGracePeriodSeconds)
	}
}
