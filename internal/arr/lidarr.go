package arr

import (
	"fmt"
	"time"

	"github.com/ensingerphilipp/premiumizearr-nova/pkg/premiumizeme"
	log "github.com/sirupsen/logrus"
	"golift.io/starr/lidarr"
)

//////
//Lidarr
//////

//Data Access

// GetHistory: Updates the history if it's been more than 15 seconds since last update
func (arr *LidarrArr) GetHistory() (lidarr.History, error) {
	arr.LastUpdateMutex.Lock()
	defer arr.LastUpdateMutex.Unlock()
	arr.HistoryMutex.Lock()
	defer arr.HistoryMutex.Unlock()
	arr.ClientMutex.Lock()
	defer arr.ClientMutex.Unlock()
	arr.LastUpdateCountMutex.Lock()
	defer arr.LastUpdateCountMutex.Unlock()

	if time.Since(arr.LastUpdate) > time.Duration(arr.Config.ArrHistoryUpdateIntervalSeconds)*time.Second || arr.History == nil {
		his, err := arr.Client.GetHistory(0, 1000)
		if err != nil {
			return lidarr.History{}, err
		}

		arr.History = his
		arr.LastUpdate = time.Now()
		arr.LastUpdateCount = his.TotalRecords
		log.Debugf("[Lidarr] [%s]: Updated history, next update in %d seconds", arr.Name, arr.Config.ArrHistoryUpdateIntervalSeconds)
	}

	log.Tracef("[Lidarr] [%s]: Returning from GetHistory", arr.Name)
	return *arr.History, nil
}

func (arr *LidarrArr) MarkHistoryItemAsFailed(id int64) error {
	arr.ClientMutex.Lock()
	defer arr.ClientMutex.Unlock()
	return arr.Client.Fail(id)

}

func (arr *LidarrArr) GetArrName() string {
	return "Lidarr"
}

//Functions

func (arr *LidarrArr) HistoryContains(name string) (int64, bool) {
	log.Tracef("Lidarr [%s]: Checking history for %s", arr.Name, name)
	his, err := arr.GetHistory()
	if err != nil {
		log.Errorf("Lidarr [%s]: Failed to get history: %+v", arr.Name, err)
		return -1, false
	}
	log.Tracef("Lidarr [%s]: Got History, now Locking History", arr.Name)
	arr.HistoryMutex.Lock()
	defer arr.HistoryMutex.Unlock()

	for _, item := range his.Records {
		if CompareFileNamesFuzzy(item.SourceTitle, name) {
			return item.ID, true
		}
	}

	return -1, false
}

func (arr *LidarrArr) HandleErrorTransfer(transfer *premiumizeme.Transfer, arrID int64, pm *premiumizeme.Premiumizeme) error {
	his, err := arr.GetHistory()
	if err != nil {
		return fmt.Errorf("failed to get history from lidarr: %+v", err)
	}

	arr.HistoryMutex.Lock()
	defer arr.HistoryMutex.Unlock()

	complete := false

	for _, queueItem := range his.Records {
		if queueItem.ID == arrID {
			if queueItem.EventType == "grabbed" {
				err := arr.MarkHistoryItemAsFailed(queueItem.ID)
				if err != nil {
					return fmt.Errorf("failed to blacklist item in lidarr: %+v", err)
				}
				err = pm.DeleteTransfer(transfer.ID)
				if err != nil {
					return fmt.Errorf("failed to delete transfer from premiumize.me: %+v", err)
				}
				complete = true
				break
			}
		}
	}

	if !complete {
		err := pm.DeleteTransfer(transfer.ID)
		if err != nil {
			return fmt.Errorf("failed to delete transfer from premiumize.me: %+v", err)
		}
	}

	return nil
}
