package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"sort"
	"strings"

	"github.com/ensingerphilipp/premiumizearr-nova/internal/config"
)

type ConfigChangeResponse struct {
	Succeeded bool   `json:"succeeded"`
	Status    string `json:"status"`
}

// numericConfigFields must exactly match the integer fields of
// config.Config, addressed by their JSON keys (TestNumericConfigFieldsMatchesConfigIntFields
// enforces the exact-set invariant). Client-side counterpart: numericFields
// in web/src/pages/Config.svelte.
var numericConfigFields = []string{
	"PollBlackholeIntervalMinutes",
	"SimultaneousDownloads",
	"DownloadSpeedLimit",
	"ArrHistoryUpdateIntervalSeconds",
	"ErroredTransferDeleteGracePeriodSeconds",
}

// configJSONKeySet is the set of JSON keys of config.Config's exported
// fields, derived once from the struct's json tags.
var configJSONKeySet = configJSONKeysOf(config.Config{})

// configJSONKeysOf derives the JSON keys of a struct's exported fields from
// their json tags (first token; a "-" tag skips the field; an absent or
// empty tag falls back to the Go field name).
func configJSONKeysOf(v any) map[string]bool {
	t := reflect.TypeOf(v)
	keys := make(map[string]bool, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" {
			continue // unexported
		}
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		key := strings.Split(tag, ",")[0]
		if key == "" {
			key = f.Name
		}
		keys[key] = true
	}
	return keys
}

// numericConfigFieldFor resolves a payload key to the canonical numeric
// config field it decodes into. encoding/json matches field names
// case-insensitively, so the validation must match the same way.
func numericConfigFieldFor(key string) (string, bool) {
	for _, field := range numericConfigFields {
		if strings.EqualFold(key, field) {
			return field, true
		}
	}
	return "", false
}

// validateConfigPayload rejects a top-level JSON null payload, an incomplete
// payload (a missing config field would be silently zeroed by the
// whole-struct replace), and any payload carrying an explicit JSON null for a
// numeric field (encoding/json treats null as a no-op for non-pointer
// fields, so a cleared UI input would otherwise be silently saved as 0).
// All of these cases were reported as a success before (issue #89). An
// explicit 0 (a value the user typed) is still valid, e.g. speed limit
// 0 = unlimited.
func validateConfigPayload(body []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		// Not a JSON object; the struct decode in the caller reports it.
		return nil
	}
	if raw == nil {
		// A top-level null body decodes into a nil map here and into a
		// zero-value config.Config in the caller, wiping the config.
		return fmt.Errorf("payload is null; expected a JSON object of config fields")
	}
	var missing []string
	for key := range configJSONKeySet {
		if _, present := raw[key]; !present {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("missing config field(s): %s", strings.Join(missing, ", "))
	}
	for key, value := range raw {
		if field, ok := numericConfigFieldFor(key); ok && string(bytes.TrimSpace(value)) == "null" {
			return fmt.Errorf("field %q is null; numeric fields must be numbers, not empty", field)
		}
	}
	return nil
}

func (s *WebServerService) ConfigHandler(w http.ResponseWriter, r *http.Request) {

	switch r.Method {
	case http.MethodGet:
		data, err := json.Marshal(s.config)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Write(data)
	case http.MethodPost:
		// Cap body buffering: a legitimate payload is a few hundred bytes,
		// so 1 MiB leaves ample headroom while bounding the memory an
		// unauthenticated request can force into ReadAll.
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			EncodeAndWriteConfigChangeResponse(w, &ConfigChangeResponse{
				Succeeded: false,
				Status:    fmt.Sprintf("Config failed to update: %s", err.Error()),
			})
			return
		}
		if err := validateConfigPayload(body); err != nil {
			EncodeAndWriteConfigChangeResponse(w, &ConfigChangeResponse{
				Succeeded: false,
				Status:    fmt.Sprintf("Config failed to update: %s", err.Error()),
			})
			return
		}
		var newConfig config.Config
		if err := json.Unmarshal(body, &newConfig); err != nil {
			EncodeAndWriteConfigChangeResponse(w, &ConfigChangeResponse{
				Succeeded: false,
				Status:    fmt.Sprintf("Config failed to update: %s", err.Error()),
			})
			return
		}
		s.config.UpdateConfig(newConfig)
		EncodeAndWriteConfigChangeResponse(w, &ConfigChangeResponse{
			Succeeded: true,
			Status:    "Config updated",
		})
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}

}

func EncodeAndWriteConfigChangeResponse(w http.ResponseWriter, resp *ConfigChangeResponse) {
	data, err := json.Marshal(resp)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Write(data)
}
