package arr

import (
	"fmt"
	"time"

	"github.com/ensingerphilipp/premiumizearr-nova/pkg/premiumizeme"
	log "github.com/sirupsen/logrus"
	"golift.io/starr/radarr"
)

//////
//Radarr
//////

//Data Access

// GetHistory: Updates the history if it's been more than 15 seconds since last update
func (arr *RadarrArr) GetHistory() (radarr.History, error) {
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
			return radarr.History{}, err
		}

		arr.History = his
		arr.LastUpdate = time.Now()
		arr.LastUpdateCount = his.TotalRecords
		log.Debugf("[Radarr] [%s]: Updated history, next update in %d seconds", arr.Name, arr.Config.ArrHistoryUpdateIntervalSeconds)
	}

	log.Tracef("[Radarr] [%s]: Returning from GetHistory", arr.Name)
	return *arr.History, nil
}

func (arr *RadarrArr) MarkHistoryItemAsFailed(id int64) error {
	arr.ClientMutex.Lock()
	defer arr.ClientMutex.Unlock()
	return arr.Client.Fail(id)

}

func (arr *RadarrArr) GetArrName() string {
	return "Radarr"
}

//Functions

func (arr *RadarrArr) HistoryContains(name string) (int64, bool) {
	log.Tracef("Radarr [%s]: Checking history for %s", arr.Name, name)
	his, err := arr.GetHistory()
	if err != nil {
		log.Errorf("Radarr [%s]: Failed to get history: %+v", arr.Name, err)
		return -1, false
	}
	log.Tracef("Radarr [%s]: Got History, now Locking History", arr.Name)
	arr.HistoryMutex.Lock()
	defer arr.HistoryMutex.Unlock()

	// The history is returned oldest first and a release name can appear in
	// several records (e.g. a previous download failure plus the current
	// grab). Only grabbed records can be marked failed, and history ids are
	// auto-incrementing, so remember the newest (highest ID) fuzzy-matching
	// grab; resolving any other record type would let the caller delete the
	// transfer without Radarr ever learning about the failed download
	// (issue #22).
	var grabbedID int64 = -1
	for _, item := range his.Records {
		if item.EventType == grabbedEventType && item.ID > grabbedID && CompareFileNamesFuzzy(item.SourceTitle, name) {
			grabbedID = item.ID
		}
	}

	if grabbedID == -1 {
		return -1, false
	}

	log.Tracef("Radarr [%s]: Found grabbed record %d in History for %s", arr.Name, grabbedID, name)

	return grabbedID, true
}

func (arr *RadarrArr) HandleErrorTransfer(transfer *premiumizeme.Transfer, arrID int64, pm *premiumizeme.Premiumizeme) error {
	his, err := arr.GetHistory()
	if err != nil {
		return fmt.Errorf("failed to get history from radarr: %+v", err)
	}

	arr.HistoryMutex.Lock()
	defer arr.HistoryMutex.Unlock()

	complete := false

	for _, queueItem := range his.Records {
		if queueItem.ID == arrID {
			if queueItem.EventType == grabbedEventType {
				err := arr.MarkHistoryItemAsFailed(queueItem.ID)
				if err != nil {
					return fmt.Errorf("failed to blacklist item in radarr: %+v", err)
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
