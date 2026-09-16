package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/ensingerphilipp/premiumizearr-nova/internal/config"
)

type ConfigChangeResponse struct {
	Succeeded bool   `json:"succeeded"`
	Status    string `json:"status"`
}

// numericConfigFields are the int fields of config.Config that the web UI
// exposes as number inputs. Keep in sync with config.Config: any new int
// field backed by a number input must be listed here.
var numericConfigFields = []string{
	"PollBlackholeIntervalMinutes",
	"SimultaneousDownloads",
	"DownloadSpeedLimit",
	"ArrHistoryUpdateIntervalSeconds",
	"ErroredTransferDeleteGracePeriodSeconds",
}

// validateConfigPayload rejects a payload that carries an explicit JSON null
// for a numeric field. encoding/json treats null for a non-pointer field as a
// no-op, so a cleared UI input would otherwise be silently saved as 0 and
// reported as a success (issue #89). An explicit 0 (a value the user typed)
// is still valid, e.g. speed limit 0 = unlimited.
func validateConfigPayload(body []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		// Not a JSON object; the struct decode in the caller reports it.
		return nil
	}
	for _, field := range numericConfigFields {
		value, present := raw[field]
		if present && string(bytes.TrimSpace(value)) == "null" {
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
		body, err := io.ReadAll(r.Body)
		if err != nil {
			EncodeAndWriteConfigChangeResponse(w, &ConfigChangeResponse{
				Succeeded: false,
				Status:    fmt.Sprintf("Config failed to update %s", err.Error()),
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
				Status:    fmt.Sprintf("Config failed to update %s", err.Error()),
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
