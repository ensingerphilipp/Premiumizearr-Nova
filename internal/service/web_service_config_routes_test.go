package service

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"reflect"
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
const nullSimultaneousDownloadsPayload = `{"PremiumizemeAPIKey":"xxxxxxxxx","Arrs":[],"BlackholeDirectory":"/blackhole","PollBlackholeDirectory":false,"PollBlackholeIntervalMinutes":10,"DownloadsDirectory":"/downloads","TransferDirectory":"arrDownloads","BindIP":"0.0.0.0","BindPort":"8182","WebRoot":"","SimultaneousDownloads":null,"DownloadSpeedLimit":100,"EnableTlsCheck":false,"TransferOnlyMode":false,"EnableArrSubfolders":false,"ArrHistoryUpdateIntervalSeconds":20,"ErroredTransferDeleteGracePeriodSeconds":300}`

// Same payload with explicit zeros: 0 is a legitimate value the user can
// still save (e.g. speed limit 0 = unlimited).
const zeroNumericFieldsPayload = `{"PremiumizemeAPIKey":"xxxxxxxxx","Arrs":[],"BlackholeDirectory":"/blackhole","PollBlackholeDirectory":false,"PollBlackholeIntervalMinutes":0,"DownloadsDirectory":"/downloads","TransferDirectory":"arrDownloads","BindIP":"0.0.0.0","BindPort":"8182","WebRoot":"","SimultaneousDownloads":0,"DownloadSpeedLimit":0,"EnableTlsCheck":false,"TransferOnlyMode":false,"EnableArrSubfolders":false,"ArrHistoryUpdateIntervalSeconds":0,"ErroredTransferDeleteGracePeriodSeconds":0}`

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

// TestConfigHandlerRejectsNullBody is the regression test for review finding
// R1-1: a top-level JSON null body unmarshals into a nil raw map
// (validation no-op) and into a zero-value config.Config (decode no-op), so
// UpdateConfig wiped the entire config while answering succeeded: true.
func TestConfigHandlerRejectsNullBody(t *testing.T) {
	ws, cfg := newConfigRouteTestService(t)
	apiKeyBefore := cfg.PremiumizemeAPIKey

	req := httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader("null"))
	rec := httptest.NewRecorder()
	ws.ConfigHandler(rec, req)

	var resp ConfigChangeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshaling response %q: %v", rec.Body.String(), err)
	}
	if resp.Succeeded {
		t.Errorf("succeeded = true, want false for a top-level null payload")
	}
	if cfg.PremiumizemeAPIKey != apiKeyBefore || cfg.SimultaneousDownloads != 5 {
		t.Errorf("config was modified by a rejected null payload: PremiumizemeAPIKey=%q SimultaneousDownloads=%d",
			cfg.PremiumizemeAPIKey, cfg.SimultaneousDownloads)
	}
}

// TestConfigHandlerRejectsOversizedBody is the regression test for review
// finding R1-2: the POST handler must cap request-body buffering so an
// unauthenticated caller cannot force the daemon to buffer an unbounded body
// (OOM DoS); the cap must still pass legitimate payloads (the ~450 B
// zeroNumericFieldsPayload fixture in TestConfigHandlerAcceptsExplicitZeroNumericFields).
func TestConfigHandlerRejectsOversizedBody(t *testing.T) {
	ws, cfg := newConfigRouteTestService(t)

	req := httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(strings.Repeat("x", 2<<20)))
	rec := httptest.NewRecorder()
	ws.ConfigHandler(rec, req)

	var resp ConfigChangeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshaling response %q: %v", rec.Body.String(), err)
	}
	if resp.Succeeded {
		t.Fatalf("succeeded = true for an oversized body")
	}
	if !strings.Contains(resp.Status, "too large") {
		t.Errorf("status = %q, want the body-size cap error", resp.Status)
	}
	if cfg.SimultaneousDownloads != 5 {
		t.Errorf("SimultaneousDownloads = %d, want 5 (rejected body must not replace the config)", cfg.SimultaneousDownloads)
	}
}

// isIntegerKind covers every reflect integer kind, so a renamed or widened
// integer config field (int64, uint32, ...) cannot slip past the sync guard.
func isIntegerKind(k reflect.Kind) bool {
	switch k {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return true
	default:
		return false
	}
}

// TestNumericConfigFieldsMatchesConfigIntFields is the sync guard for review
// findings R1-3/R1-4: numericConfigFields must exactly match the integer
// fields of config.Config, keyed by their json tag (first token, "-" skipped,
// absent/empty tag falls back to the Go field name) across every integer
// kind — not just reflect.Int by Go field name.
func TestNumericConfigFieldsMatchesConfigIntFields(t *testing.T) {
	want := make(map[string]bool)
	v := reflect.TypeOf(config.Config{})
	for i := 0; i < v.NumField(); i++ {
		f := v.Field(i)
		if f.PkgPath != "" {
			continue // unexported
		}
		if !isIntegerKind(f.Type.Kind()) {
			continue
		}
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		key := strings.Split(tag, ",")[0]
		if key == "" {
			key = f.Name
		}
		want[key] = true
	}
	got := make(map[string]bool, len(numericConfigFields))
	for _, name := range numericConfigFields {
		got[name] = true
	}
	for key := range want {
		if !got[key] {
			t.Errorf("config.Config has integer json field %q but numericConfigFields does not list it", key)
		}
	}
	for key := range got {
		if !want[key] {
			t.Errorf("numericConfigFields lists %q but config.Config has no integer field with that json key", key)
		}
	}
}

// assertConfigUnchanged fails if a rejected payload modified the config.
func assertConfigUnchanged(t *testing.T, cfg *config.Config, apiKeyBefore string, sdBefore int) {
	t.Helper()
	if cfg.PremiumizemeAPIKey != apiKeyBefore || cfg.SimultaneousDownloads != sdBefore {
		t.Errorf("config was modified by a rejected payload: PremiumizemeAPIKey=%q (was %q) SimultaneousDownloads=%d (was %d)",
			cfg.PremiumizemeAPIKey, apiKeyBefore, cfg.SimultaneousDownloads, sdBefore)
	}
}

// TestConfigHandlerRejectsIncompletePayload is the regression test for review
// finding R1-1: an empty or partial object passes the null guard but still
// wipes the whole config through the whole-struct replace while answering
// succeeded: true.
func TestConfigHandlerRejectsIncompletePayload(t *testing.T) {
	t.Run("empty object", func(t *testing.T) {
		ws, cfg := newConfigRouteTestService(t)
		apiKeyBefore, sdBefore := cfg.PremiumizemeAPIKey, cfg.SimultaneousDownloads

		req := httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader("{}"))
		rec := httptest.NewRecorder()
		ws.ConfigHandler(rec, req)

		var resp ConfigChangeResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshaling response %q: %v", rec.Body.String(), err)
		}
		if resp.Succeeded {
			t.Errorf("succeeded = true, want false for an empty payload")
		}
		if !strings.Contains(resp.Status, "PremiumizemeAPIKey") {
			t.Errorf("status = %q, want it to name missing fields", resp.Status)
		}
		assertConfigUnchanged(t, cfg, apiKeyBefore, sdBefore)
	})
	t.Run("partial object", func(t *testing.T) {
		ws, cfg := newConfigRouteTestService(t)
		apiKeyBefore, sdBefore := cfg.PremiumizemeAPIKey, cfg.SimultaneousDownloads

		req := httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(`{"BindPort":"8182"}`))
		rec := httptest.NewRecorder()
		ws.ConfigHandler(rec, req)

		var resp ConfigChangeResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshaling response %q: %v", rec.Body.String(), err)
		}
		if resp.Succeeded {
			t.Errorf("succeeded = true, want false for a partial payload")
		}
		if !strings.Contains(resp.Status, "SimultaneousDownloads") {
			t.Errorf("status = %q, want it to name missing fields", resp.Status)
		}
		assertConfigUnchanged(t, cfg, apiKeyBefore, sdBefore)
	})
}

// TestConfigHandlerRejectsCaseVariantNullField is the regression test for
// review finding R1-2: encoding/json resolves field names case-insensitively,
// so a case-variant spelling of a numeric field with a null value must be
// rejected exactly like the exact-case spelling.
func TestConfigHandlerRejectsCaseVariantNullField(t *testing.T) {
	ws, cfg := newConfigRouteTestService(t)
	apiKeyBefore, sdBefore := cfg.PremiumizemeAPIKey, cfg.SimultaneousDownloads

	// Full payload (every config key present) where SimultaneousDownloads is
	// set canonically and repeated in lower case with a null value.
	body := strings.Replace(nullSimultaneousDownloadsPayload,
		`"SimultaneousDownloads":null`,
		`"SimultaneousDownloads":7,"simultaneousdownloads":null`, 1)

	req := httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(body))
	rec := httptest.NewRecorder()
	ws.ConfigHandler(rec, req)

	var resp ConfigChangeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshaling response %q: %v", rec.Body.String(), err)
	}
	if resp.Succeeded {
		t.Errorf("succeeded = true, want false for a case-variant null SimultaneousDownloads")
	}
	if !strings.Contains(resp.Status, "SimultaneousDownloads") {
		t.Errorf("status = %q, want the canonical field name", resp.Status)
	}
	assertConfigUnchanged(t, cfg, apiKeyBefore, sdBefore)
}

// TestConfigHandlerRejectsWrongTypedField exercises the struct-decode error
// branch (review finding R1-3): a payload that passes validation but fails
// the json decode into config.Config must be rejected without touching the
// config.
func TestConfigHandlerRejectsWrongTypedField(t *testing.T) {
	ws, cfg := newConfigRouteTestService(t)
	apiKeyBefore, sdBefore := cfg.PremiumizemeAPIKey, cfg.SimultaneousDownloads

	body := strings.Replace(zeroNumericFieldsPayload,
		`"SimultaneousDownloads":0`,
		`"SimultaneousDownloads":"five"`, 1)

	req := httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(body))
	rec := httptest.NewRecorder()
	ws.ConfigHandler(rec, req)

	var resp ConfigChangeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshaling response %q: %v", rec.Body.String(), err)
	}
	if resp.Succeeded {
		t.Fatalf("succeeded = true for a wrongly typed field: %s", resp.Status)
	}
	assertConfigUnchanged(t, cfg, apiKeyBefore, sdBefore)
}

// configIntField reads an exported int field of config.Config by name for
// the per-field table tests.
func configIntField(t *testing.T, cfg *config.Config, name string) int {
	t.Helper()
	f := reflect.ValueOf(cfg).Elem().FieldByName(name)
	if !f.IsValid() || f.Kind() != reflect.Int {
		t.Fatalf("config.Config has no int field %q", name)
	}
	return int(f.Int())
}

func cloneStringAnyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// TestConfigHandlerNumericFieldNullRejectedZeroAccepted is the per-field
// table for review finding R1-3: for every numeric field, an explicit null
// must be rejected (naming the field, config unchanged) while an explicit 0
// must remain storable. The rows vary null vs 0, not null vs absent —
// absent fields are rejected by the completeness check
// (TestConfigHandlerRejectsIncompletePayload).
func TestConfigHandlerNumericFieldNullRejectedZeroAccepted(t *testing.T) {
	base := map[string]any{
		"PremiumizemeAPIKey":                      "xxxxxxxxx",
		"Arrs":                                    []any{},
		"BlackholeDirectory":                      "/blackhole",
		"PollBlackholeDirectory":                  false,
		"PollBlackholeIntervalMinutes":            0,
		"DownloadsDirectory":                      "/downloads",
		"TransferDirectory":                       "arrDownloads",
		"BindIP":                                  "0.0.0.0",
		"BindPort":                                "8182",
		"WebRoot":                                 "",
		"SimultaneousDownloads":                   0,
		"DownloadSpeedLimit":                      0,
		"EnableTlsCheck":                          false,
		"TransferOnlyMode":                        false,
		"EnableArrSubfolders":                     false,
		"ArrHistoryUpdateIntervalSeconds":         0,
		"ErroredTransferDeleteGracePeriodSeconds": 0,
	}

	for _, field := range numericConfigFields {
		t.Run(field+"_null_rejected", func(t *testing.T) {
			ws, cfg := newConfigRouteTestService(t)
			before := configIntField(t, cfg, field)

			payload := cloneStringAnyMap(base)
			payload[field] = nil
			body, err := json.Marshal(payload)
			if err != nil {
				t.Fatalf("marshaling payload: %v", err)
			}
			req := httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(string(body)))
			rec := httptest.NewRecorder()
			ws.ConfigHandler(rec, req)

			var resp ConfigChangeResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("unmarshaling response %q: %v", rec.Body.String(), err)
			}
			if resp.Succeeded {
				t.Errorf("succeeded = true, want false for null %s", field)
			}
			if !strings.Contains(resp.Status, field) {
				t.Errorf("status = %q, want it to name the rejected field", resp.Status)
			}
			if got := configIntField(t, cfg, field); got != before {
				t.Errorf("%s = %d, want %d (rejected payload must not replace the config)", field, got, before)
			}
		})
		t.Run(field+"_zero_accepted", func(t *testing.T) {
			ws, cfg := newConfigRouteTestService(t)

			body, err := json.Marshal(cloneStringAnyMap(base))
			if err != nil {
				t.Fatalf("marshaling payload: %v", err)
			}
			req := httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(string(body)))
			rec := httptest.NewRecorder()
			ws.ConfigHandler(rec, req)

			var resp ConfigChangeResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("unmarshaling response %q: %v", rec.Body.String(), err)
			}
			if !resp.Succeeded {
				t.Fatalf("succeeded = false, want true for explicit zero %s: %s", field, resp.Status)
			}
			if got := configIntField(t, cfg, field); got != 0 {
				t.Errorf("%s = %d, want 0 (explicit zero must be stored as-is)", field, got)
			}
		})
	}
}
