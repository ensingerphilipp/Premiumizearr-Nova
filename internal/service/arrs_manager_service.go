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
	mu                   *sync.RWMutex
	arrs                 []arr.IArr
	config               *config.Config
	failureTargets       []directFailureTarget
	failureTargetRetries map[string]failureTargetRetry
	failureReportNow     func() time.Time
}

type failureTargetRetry struct {
	attempt    uint64
	created    time.Time
	errors     int
	misses     int
	retryAfter time.Time
}

const directFailureHistoryPollCap = 8

type directFailureTarget struct {
	key    string
	client arr.IArr
}

func (am ArrsManagerService) New() ArrsManagerService {
	am.mu = &sync.RWMutex{}
	am.arrs = []arr.IArr{}
	am.failureTargets = nil
	am.failureTargetRetries = make(map[string]failureTargetRetry)
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
	am.failureTargetRetries = make(map[string]failureTargetRetry)
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
// Failed targets back off but remain eligible for recovery probes. A
// deferred retry is still an error to the manager, so an outage cannot
// consume its no-error report budget. Each healthy target without a
// matching grab has its own finite lookup budget, preventing an offline
// sibling from causing unlimited history refetches against healthy apps.
func (am *ArrsManagerService) ReportDirectTorrentFailure(job directclient.Job) ([]string, error) {
	am.mu.RLock()
	targets := append([]directFailureTarget(nil), am.failureTargets...)
	am.mu.RUnlock()
	var acknowledged []string
	var failures []error
	now := time.Now()
	if am.failureReportNow != nil {
		now = am.failureReportNow()
	}
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
		retry := am.failureTargetRetries[strikeKey]
		am.mu.RUnlock()
		if retry.attempt != job.Attempt || !retry.created.Equal(job.Created) {
			retry = failureTargetRetry{attempt: job.Attempt, created: job.Created}
		}
		if retry.misses >= directFailureHistoryPollCap {
			continue
		}
		if now.Before(retry.retryAfter) {
			failures = append(failures, fmt.Errorf("%s: failure report retry deferred", target.client.GetArrName()))
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
		if am.failureTargetRetries == nil {
			am.failureTargetRetries = make(map[string]failureTargetRetry)
		}
		if err != nil {
			retry.errors++
			if retry.errors >= 2 {
				delay := 15 * time.Second
				for n := 2; n < retry.errors && delay < 5*time.Minute; n++ {
					delay *= 2
				}
				if delay > 5*time.Minute {
					delay = 5 * time.Minute
				}
				retry.retryAfter = now.Add(delay)
			}
		} else {
			retry.errors, retry.retryAfter = 0, time.Time{}
			if !found {
				retry.misses++
			}
		}
		am.failureTargetRetries[strikeKey] = retry
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
