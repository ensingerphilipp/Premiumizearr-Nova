package service

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/ensingerphilipp/premiumizearr-nova/internal/arr"
	"github.com/ensingerphilipp/premiumizearr-nova/internal/config"
	"github.com/ensingerphilipp/premiumizearr-nova/internal/directclient"
	log "github.com/sirupsen/logrus"
	"golift.io/starr"
	"golift.io/starr/lidarr"
	"golift.io/starr/radarr"
	"golift.io/starr/sonarr"
)

type ArrsManagerService struct {
	mu             *sync.RWMutex
	arrs           []arr.IArr
	config         *config.Config
	failureTargets []directFailureTarget
	// failureTargetStrikes counts CONSECUTIVE erroring report passes per
	// (job, target) pair: once a target reaches two, it is skipped for
	// that job's remaining passes so its error can no longer pin the
	// pass's error set (see ReportDirectTorrentFailure). Entries are
	// bounded in practice: the manager stops calling the reporter for a
	// dead-lettered job, and a target that queries cleanly again has its
	// strike removed.
	failureTargetStrikes map[string]int
}

type directFailureTarget struct {
	key    string
	client arr.IArr
}

func (am ArrsManagerService) New() ArrsManagerService {
	am.mu = &sync.RWMutex{}
	am.arrs = []arr.IArr{}
	am.failureTargets = nil
	am.failureTargetStrikes = make(map[string]int)
	return am
}

func (am *ArrsManagerService) Init(_config *config.Config) {
	am.config = _config
}

func (am *ArrsManagerService) Start() {
	clients := []arr.IArr{}
	targets := []directFailureTarget{}
	log.Debugf("Starting ArrsManagerService")
	for _, arr_config := range am.config.Arrs {
		switch arr_config.Type {
		case config.Sonarr:
			c := starr.New(arr_config.APIKey, arr_config.URL, 0)
			wrapper := arr.SonarrArr{
				Name:       arr_config.Name,
				Client:     sonarr.New(c),
				History:    nil,
				LastUpdate: time.Now(),
				Config:     am.config,
			}
			clients = append(clients, &wrapper)
			log.Tracef("Added Sonarr arr: %s", arr_config.Name)
		case config.Radarr:
			c := starr.New(arr_config.APIKey, arr_config.URL, 0)
			wrapper := arr.RadarrArr{
				Name:       arr_config.Name,
				Client:     radarr.New(c),
				History:    nil,
				LastUpdate: time.Now(),
				Config:     am.config,
			}
			clients = append(clients, &wrapper)
			log.Tracef("Added Radarr arr: %s", arr_config.Name)
		case config.Lidarr:
			c := starr.New(arr_config.APIKey, arr_config.URL, 0)
			wrapper := arr.LidarrArr{
				Name:       arr_config.Name,
				Client:     lidarr.New(c),
				History:    nil,
				LastUpdate: time.Now(),
				Config:     am.config,
			}
			clients = append(clients, &wrapper)
			log.Tracef("Added Lidarr arr: %s", arr_config.Name)
		default:
			log.Errorf("Unknown arr type: %s, not adding Arr %s", arr_config.Type, arr_config.Name)
			continue
		}
		key := fmt.Sprintf("%x", sha256.Sum256([]byte(string(arr_config.Type)+"\x00"+arr_config.URL)))
		targets = append(targets, directFailureTarget{key: key, client: clients[len(clients)-1]})
	}
	am.mu.Lock()
	am.arrs, am.failureTargets = clients, targets
	am.mu.Unlock()
	log.Debugf("Created %d Arrs", len(clients))
}

func (am *ArrsManagerService) Stop() {
	//noop
}

func (am *ArrsManagerService) ConfigUpdatedCallback(currentConfig config.Config, newConfig config.Config) {
	if len(currentConfig.Arrs) != len(newConfig.Arrs) {
		am.Start()
		return
	}
	for i, arr_config := range newConfig.Arrs {
		if currentConfig.Arrs[i].Type != arr_config.Type ||
			currentConfig.Arrs[i].APIKey != arr_config.APIKey ||
			currentConfig.Arrs[i].URL != arr_config.URL {
			am.Start()
			return
		}
	}
}

func (am *ArrsManagerService) GetArrs() []arr.IArr {
	am.mu.RLock()
	defer am.mu.RUnlock()
	return append([]arr.IArr(nil), am.arrs...)
}

// qBittorrent's error state is a warning in *arr, so terminal direct torrent
// failures must explicitly mark the corresponding grabbed history record.
//
// The per-target strike isolation is what keeps the manager's report cap
// reachable: the manager only advances its no-error-pass counter on a
// pass that returns no error, so a permanently erroring *arr (a revoked
// API key, a black-holed instance) would otherwise make EVERY pass fail
// and the failedReportPollCap dead-letter would never fire — while each
// pass still forces a full-history refetch against every healthy *arr.
// A target that errors on two consecutive passes is therefore skipped
// for this job's remaining passes, which restores the no-error pass; a
// target that queries cleanly again has its strike reset and is
// re-reported.
func (am *ArrsManagerService) ReportDirectTorrentFailure(job directclient.Job) ([]string, error) {
	am.mu.RLock()
	targets := append([]directFailureTarget(nil), am.failureTargets...)
	am.mu.RUnlock()
	var acknowledged []string
	var failures []error
	for _, target := range targets {
		alreadyReported := false
		for _, key := range job.ReportedFailures {
			if key == target.key {
				alreadyReported = true
				break
			}
		}
		if alreadyReported {
			continue
		}
		strikeKey := job.ID + "\x00" + target.key
		am.mu.RLock()
		strikes := am.failureTargetStrikes[strikeKey]
		am.mu.RUnlock()
		if strikes >= 2 {
			continue
		}
		id, found, err := target.client.HistoryContainsDownloadIDFresh(job.ID)
		if err == nil && found {
			err = target.client.MarkHistoryItemAsFailed(id)
			if err == nil {
				acknowledged = append(acknowledged, target.key)
			}
		}
		am.mu.Lock()
		if err != nil {
			am.failureTargetStrikes[strikeKey] = strikes + 1
		} else {
			delete(am.failureTargetStrikes, strikeKey)
		}
		am.mu.Unlock()
		if err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", target.client.GetArrName(), err))
		}
	}
	return acknowledged, errors.Join(failures...)
}

func TestArrConnection(arr config.ArrConfig) error {
	c := starr.New(arr.APIKey, arr.URL, 0)

	switch arr.Type {
	case config.Sonarr:
		_, err := sonarr.New(c).GetSystemStatus()
		return err
	case config.Radarr:
		_, err := radarr.New(c).GetSystemStatus()
		return err
	case config.Lidarr:
		_, err := lidarr.New(c).GetSystemStatus()
		return err
	default:
		return nil
	}
}
