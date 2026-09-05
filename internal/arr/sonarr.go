package arr

import (
	"fmt"
	"time"

	"github.com/ensingerphilipp/premiumizearr-nova/pkg/premiumizeme"
	log "github.com/sirupsen/logrus"
	"golift.io/starr/sonarr"
)

//////
//Sonarr
//////

//Data Access

// GetHistory: Updates the history if it's been more than 15 seconds since last update
func (arr *SonarrArr) GetHistory() (sonarr.History, error) {
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
			return sonarr.History{}, err
		}

		arr.History = his
		arr.LastUpdate = time.Now()
		arr.LastUpdateCount = his.TotalRecords
		log.Debugf("[Sonarr] [%s]: Updated history, next update in %d seconds", arr.Name, arr.Config.ArrHistoryUpdateIntervalSeconds)
	}

	log.Tracef("[Sonarr] [%s]: Returning from GetHistory", arr.Name)
	return *arr.History, nil
}

func (arr *SonarrArr) MarkHistoryItemAsFailed(id int64) error {
	arr.ClientMutex.Lock()
	defer arr.ClientMutex.Unlock()
	return arr.Client.Fail(id)
}

func (arr *SonarrArr) GetArrName() string {
	return "Sonarr"
}

// Functions

func (arr *SonarrArr) HistoryContains(name string) (int64, bool) {
	log.Tracef("Sonarr [%s]: Checking history for %s", arr.Name, name)
	his, err := arr.GetHistory()
	if err != nil {
		return 0, false
	}
	log.Tracef("Sonarr [%s]: Got History, now Locking History", arr.Name)
	arr.HistoryMutex.Lock()
	defer arr.HistoryMutex.Unlock()

	// The history is returned oldest first and a release name can appear in
	// several records (e.g. a previous download failure plus the current
	// grab). Only grabbed records can be marked failed, and history ids are
	// auto-incrementing, so remember the newest (highest ID) fuzzy-matching
	// grab; resolving any other record type would let the caller delete the
	// transfer without Sonarr ever learning about the failed download
	// (issue #22).
	var grabbedID int64 = -1
	for _, item := range his.Records {
		if item.EventType == grabbedEventType && item.ID > grabbedID && CompareFileNamesFuzzy(item.SourceTitle, name) {
			grabbedID = item.ID
		}
	}

	if grabbedID == -1 {
		log.Tracef("Sonarr [%s]: %s Not in History", arr.Name, name)
		return -1, false
	}

	log.Tracef("Sonarr [%s]: Found grabbed record %d in History for %s", arr.Name, grabbedID, name)

	return grabbedID, true
}

func (arr *SonarrArr) HandleErrorTransfer(transfer *premiumizeme.Transfer, arrID int64, pm *premiumizeme.Premiumizeme) error {
	his, err := arr.GetHistory()
	if err != nil {
		return fmt.Errorf("failed to get history from sonarr: %+v", err)
	}

	arr.HistoryMutex.Lock()
	defer arr.HistoryMutex.Unlock()

	complete := false

	for _, queueItem := range his.Records {
		if queueItem.ID == arrID {
			if queueItem.EventType == grabbedEventType {
				err := arr.MarkHistoryItemAsFailed(queueItem.ID)
				if err != nil {
					return fmt.Errorf("failed to blacklist item in sonarr: %+v", err)
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
