package service

import (
	"encoding/json"
	"net/http"
	"path"
	"sort"

	"github.com/ensingerphilipp/premiumizearr-nova/internal/config"
	"github.com/ensingerphilipp/premiumizearr-nova/pkg/premiumizeme"
)

type TransfersResponse struct {
	Transfers []premiumizeme.Transfer `json:"data"`
	Status    string                  `json:"status"`
}

func (s *WebServerService) TransfersHandler(w http.ResponseWriter, r *http.Request) {
	var resp TransfersResponse
	resp.Transfers = *s.transferManager.GetTransfers()
	resp.Status = s.transferManager.GetStatus()
	data, err := json.Marshal(resp)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Write(data)
}

type BlackholeFile struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
	Arr  string `json:"arr"`
}
type BlackholeResponse struct {
	BlackholeFiles []BlackholeFile `json:"data"`
	Status         string          `json:"status"`
}

type Download struct {
	Added    int64  `json:"added"`
	Name     string `json:"name"`
	Progress string `json:"progress"`
	Speed    string `json:"speed"`
}
type DownloadsResponse struct {
	Downloads []Download `json:"data"`
	Status    string     `json:"status"`
}

func (s *WebServerService) DownloadsHandler(w http.ResponseWriter, r *http.Request) {
	var resp DownloadsResponse

	if s.transferManager == nil {
		resp.Status = "Not Initialized"
	} else {
		downloads := s.transferManager.GetDownloads()

		// Collect downloads into a slice for sorting
		sortedDownloads := make([]*DownloadDetails, 0, len(downloads))
		for _, v := range downloads {
			sortedDownloads = append(sortedDownloads, v)
		}

		// Sort downloads by Name
		sort.Slice(sortedDownloads, func(i, j int) bool {
			return sortedDownloads[i].Name < sortedDownloads[j].Name
		})

		// Build the response
		for _, v := range sortedDownloads {
			resp.Downloads = append(resp.Downloads, Download{
				Added:    v.Added.Unix(),
				Name:     v.Name,
				Progress: v.ProgressDownloader.GetProgress(),
				Speed:    v.ProgressDownloader.GetSpeed(),
			})
		}
		resp.Status = ""
	}

	data, err := json.Marshal(resp)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Write(data)
}

func (s *WebServerService) BlackholeHandler(w http.ResponseWriter, r *http.Request) {
	var resp BlackholeResponse

	if s.directoryWatcherService == nil {
		resp.Status = "Not Initialized"
	} else {
		for i, n := range s.directoryWatcherService.Queue.GetQueue() {
			name := path.Base(n)
			arr := ""
			if path.Dir(n) != path.Clean(s.config.BlackholeDirectory) {
				arr = path.Base(path.Dir(n))
			}
			resp.BlackholeFiles = append(resp.BlackholeFiles, BlackholeFile{
				ID:   i,
				Name: name,
				Arr:  arr,
			})
		}

		resp.Status = s.directoryWatcherService.GetStatus()
	}

	data, err := json.Marshal(resp)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Write(data)
}

type TestArrResponse struct {
	Status    string `json:"status"`
	Succeeded bool   `json:"succeeded"`
}

func (s *WebServerService) TestArrHandler(w http.ResponseWriter, r *http.Request) {
	var arr config.ArrConfig
	err := json.NewDecoder(r.Body).Decode(&arr)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	err = TestArrConnection(arr)

	var resp TestArrResponse
	if err != nil {
		resp.Status = err.Error()
		resp.Succeeded = false
	} else {
		resp.Succeeded = true
	}

	data, err := json.Marshal(resp)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Write(data)
}
