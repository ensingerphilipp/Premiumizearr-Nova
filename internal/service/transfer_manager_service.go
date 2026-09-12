package service

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sync"
	"time"

	"github.com/ensingerphilipp/premiumizearr-nova/internal/arr"
	"github.com/ensingerphilipp/premiumizearr-nova/internal/config"
	"github.com/ensingerphilipp/premiumizearr-nova/internal/progress_downloader"
	"github.com/ensingerphilipp/premiumizearr-nova/internal/utils"
	"github.com/ensingerphilipp/premiumizearr-nova/pkg/premiumizeme"
	log "github.com/sirupsen/logrus"
)

type DownloadDetails struct {
	Added              time.Time
	Name               string
	ProgressDownloader *progress_downloader.WriteCounter
	// topLevel marks entries for top-level folder jobs admitted by
	// HandleFinishedItem; only these count against SimultaneousDownloads.
	topLevel bool
}

// erroredTransferState tracks one errored premiumize.me transfer during the
// grace period before it is either reported to a matched *arr and deleted,
// or deleted as unmatched.
type erroredTransferState struct {
	firstSeen  time.Time
	processing bool
}

type TransferManagerService struct {
	premiumizemeClient    *premiumizeme.Premiumizeme
	arrsManager           *ArrsManagerService
	config                *config.Config
	lastUpdated           int64
	transfers             []premiumizeme.Transfer
	runningTask           bool
	downloadListMutex     *sync.Mutex
	downloadList          map[string]*DownloadDetails
	status                string
	downloadsFolderID     string
	failedDownloadsMutex  *sync.Mutex
	failedDownloads       map[string]time.Time // Maps item name to failure timestamp
	erroredTransfersMutex *sync.Mutex
	erroredTransfers      map[string]*erroredTransferState // Maps premiumize.me transfer ID to grace-period tracking state
	nowFunc               func() time.Time
}

// Handle
func (t TransferManagerService) New() TransferManagerService {
	t.premiumizemeClient = nil
	t.arrsManager = nil
	t.config = nil
	t.lastUpdated = time.Now().Unix()
	t.transfers = make([]premiumizeme.Transfer, 0)
	t.runningTask = false
	t.downloadListMutex = &sync.Mutex{}
	t.downloadList = make(map[string]*DownloadDetails, 0)
	t.status = ""
	t.downloadsFolderID = ""
	t.failedDownloadsMutex = &sync.Mutex{}
	t.failedDownloads = make(map[string]time.Time, 0)
	t.erroredTransfersMutex = &sync.Mutex{}
	t.erroredTransfers = make(map[string]*erroredTransferState, 0)
	t.nowFunc = time.Now
	return t
}

func (t *TransferManagerService) Init(pme *premiumizeme.Premiumizeme, arrsManager *ArrsManagerService, config *config.Config) {
	t.premiumizemeClient = pme
	t.arrsManager = arrsManager
	t.config = config
	t.CleanUpDownloadDirPeriod()
}

func (t *TransferManagerService) CleanUpDownloadDirPeriod() {
	log.Info("Cleaning download directory - deleting files older than 4 days")

	downloadBase, err := t.config.GetDownloadsBaseLocation()
	if err != nil {
		log.Errorf("Error getting download base location: %s", err.Error())
		return
	}

	// Define the threshold for deletion: 4 days
	threshold := time.Now().AddDate(0, 0, -4)

	err = filepath.Walk(downloadBase, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			log.Warnf("Error accessing path %s: %s", path, err.Error())
			return nil // Continue processing other files/directories
		}

		// Skip the base directory itself
		if path == downloadBase {
			return nil
		}

		// Check if the file/directory is older than 4 days
		if info.ModTime().Before(threshold) {
			log.Infof("Deleting %s (last modified: %s)", path, info.ModTime())

			// Remove the directory/file
			err = os.RemoveAll(path)
			if err != nil {
				log.Errorf("Error deleting %s: %s", path, err.Error())
			}
		}
		return nil
	})

	if err != nil {
		log.Errorf("Error cleaning download directory: %s", err.Error())
	}
}

func (t *TransferManagerService) CleanUpDownloadDir() {
	log.Info("Cleaning download directory")

	downloadBase, err := t.config.GetDownloadsBaseLocation()
	if err != nil {
		log.Errorf("Error getting download base location: %s", err.Error())
		return
	}

	err = utils.RemoveContents(downloadBase)
	if err != nil {
		log.Errorf("Error cleaning download directory: %s", err.Error())
		return
	}

}

func (manager *TransferManagerService) ConfigUpdatedCallback(currentConfig config.Config, newConfig config.Config) {
	if currentConfig.DownloadsDirectory != newConfig.DownloadsDirectory {
		log.Trace("Inside ConfigUpdatedCallback")
		manager.CleanUpDownloadDir()
	}

	if currentConfig.TransferDirectory != newConfig.TransferDirectory {
		log.Trace("Updating Transfer Directory in TransferManagerService")
		newDir := newConfig.TransferDirectory
		newID := utils.GetDownloadsFolderIDFromPremiumizeme(manager.premiumizemeClient, newDir)
		manager.downloadsFolderID = newID
	}
}

func (manager *TransferManagerService) Run(interval time.Duration) {
	manager.downloadsFolderID = utils.GetDownloadsFolderIDFromPremiumizeme(manager.premiumizemeClient, manager.config.TransferDirectory)
	for {
		manager.runningTask = true
		manager.TaskUpdateTransfersList()
		if !manager.config.TransferOnlyMode {
			manager.TaskCheckPremiumizeDownloadsFolder()
		} else {
			log.Info("TransferOnlyMode is enabled, skipping Download")
		}
		manager.runningTask = false
		manager.lastUpdated = time.Now().Unix()
		time.Sleep(interval)
	}
}

func (manager *TransferManagerService) GetDownloads() map[string]*DownloadDetails {
	return manager.downloadList
}

func (manager *TransferManagerService) GetTransfers() *[]premiumizeme.Transfer {
	return &manager.transfers
}
func (manager *TransferManagerService) GetStatus() string {
	return manager.status
}

func (manager *TransferManagerService) TaskUpdateTransfersList() {
	log.Debug("Running Task UpdateTransfersList")
	transfers, err := manager.premiumizemeClient.GetTransfers()
	if err != nil {
		log.Errorf("Error getting transfers: %s", err.Error())
		return
	}
	manager.updateTransfers(transfers)

	log.Tracef("Checking %d transfers against %d Arr clients", len(transfers), len(manager.arrsManager.GetArrs()))
	currentTransferIDs := make(map[string]bool, len(transfers))
	for i := range transfers {
		transfer := &transfers[i]
		currentTransferIDs[transfer.ID] = true
		if transfer.Status != "error" {
			// No longer errored: stop tracking so the transfer is not
			// carried over into a later grace period.
			manager.forgetErroredTransfer(transfer.ID)
			continue
		}
		manager.processErroredTransfer(transfer)
	}
	manager.pruneErroredTransfers(currentTransferIDs)
}

// processErroredTransfer tracks one errored transfer by its premiumize.me
// ID and drives its grace-period handling. While the configured grace
// period has not elapsed, every poll re-checks all configured *arrs for a
// matching grabbed history record; a match is handled immediately (the
// *arr history item is marked failed first, then the transfer is deleted).
// Only an authoritative no-match on every reachable *arr past the grace
// period leads to deletion. A failed history lookup is never treated as a
// no-match, so an *arr outage cannot cause automatic deletion.
func (manager *TransferManagerService) processErroredTransfer(transfer *premiumizeme.Transfer) {
	now := manager.now()
	state := manager.getOrTrackErroredTransfer(transfer.ID, now)

	var matched arr.IArr
	var matchedID int64
	allLookupsSucceeded := true
	for _, arrClient := range manager.arrsManager.GetArrs() {
		log.Tracef("Checking errored transfer %s against %s history", transfer.Name, arrClient.GetArrName())
		arrID, found, err := arrClient.HistoryContains(transfer.Name)
		if err != nil {
			allLookupsSucceeded = false
			log.Warnf("History lookup for errored transfer %s (id %s) against %s failed: %s - keeping the transfer, a lookup failure is not a no-match", transfer.Name, transfer.ID, arrClient.GetArrName(), err.Error())
			continue
		}
		if !found {
			log.Tracef("%s history doesn't contain %s", arrClient.GetArrName(), transfer.Name)
			continue
		}
		log.Tracef("Found %s in %s history", transfer.Name, arrClient.GetArrName())
		matched = arrClient
		matchedID = arrID
		break
	}

	if matched != nil {
		if !manager.beginErroredTransferProcessing(transfer.ID) {
			log.Debugf("Errored transfer %s (id %s) is already being processed, skipping this poll", transfer.Name, transfer.ID)
			return
		}
		go manager.reportErroredTransferToArr(matched, matchedID, transfer)
		return
	}

	if !allLookupsSucceeded {
		// At least one *arr could not be checked, so the no-match is not
		// authoritative: an *arr outage must never lead to automatic
		// deletion. Keep the transfer and re-check on the next poll.
		log.Debugf("Errored transfer %s (id %s) could not be checked against every *arr yet, keeping it for the next poll", transfer.Name, transfer.ID)
		return
	}

	grace := manager.erroredTransferGracePeriod()
	if now.Sub(state.firstSeen) < grace {
		log.Debugf("Errored transfer %s (id %s) matched no *arr history yet, first seen %s ago, grace period %s", transfer.Name, transfer.ID, now.Sub(state.firstSeen).Round(time.Second), grace)
		return
	}

	log.Debugf("Errored transfer %s (id %s) still unmatched after the grace period of %s, deleting it", transfer.Name, transfer.ID, grace)
	if !manager.beginErroredTransferProcessing(transfer.ID) {
		log.Debugf("Errored transfer %s (id %s) is already being processed, skipping this poll", transfer.Name, transfer.ID)
		return
	}
	go manager.deleteUnmatchedErroredTransfer(transfer)
}

// reportErroredTransferToArr marks the matched *arr history record as
// failed first, then deletes the premiumize.me transfer (the existing
// IArr.HandleErrorTransfer contract). The per-transfer processing slot is
// always released; on error the tracking state is kept so the next poll
// retries the report.
func (manager *TransferManagerService) reportErroredTransferToArr(arrClient arr.IArr, arrID int64, transfer *premiumizeme.Transfer) {
	defer manager.finishErroredTransferProcessing(transfer.ID)
	log.Debugf("Processing transfer that has errored: %s", transfer.Name)
	if err := arrClient.HandleErrorTransfer(transfer, arrID, manager.premiumizemeClient); err != nil {
		log.Errorf("Error reporting errored transfer %s (id %s) to %s, retrying on next poll: %s", transfer.Name, transfer.ID, arrClient.GetArrName(), err.Error())
		return
	}
	log.Infof("Errored transfer %s (id %s) reported as failed in %s and deleted from premiumize.me", transfer.Name, transfer.ID, arrClient.GetArrName())
	manager.forgetErroredTransfer(transfer.ID)
}

// deleteUnmatchedErroredTransfer deletes an errored transfer that stayed
// unmatched on every reachable *arr for the whole grace period. The
// warning carries the transfer ID, name and error so the deletion stays
// auditable. The per-transfer processing slot is always released; the
// tracking state is removed only after a successful deletion, so a failed
// deletion is retried on the next poll.
func (manager *TransferManagerService) deleteUnmatchedErroredTransfer(transfer *premiumizeme.Transfer) {
	defer manager.finishErroredTransferProcessing(transfer.ID)
	if err := manager.premiumizemeClient.DeleteTransfer(transfer.ID); err != nil {
		log.Errorf("Failed to delete unmatched errored transfer %s (id %s) from premiumize.me, retrying on next poll: %s", transfer.Name, transfer.ID, err.Error())
		return
	}
	log.Warnf("Deleted errored transfer %q (id %s) from premiumize.me after the grace period without a *arr history match; transfer error: %s", transfer.Name, transfer.ID, transfer.Message)
	manager.forgetErroredTransfer(transfer.ID)
}

// erroredTransferGracePeriod returns the configured grace period for
// unmatched errored transfers, falling back to the 5 minute default when
// the config value is unset or invalid (e.g. a client that does not know
// the field posted it as zero) so a zero value can never make transfers
// delete immediately.
func (manager *TransferManagerService) erroredTransferGracePeriod() time.Duration {
	seconds := manager.config.ErroredTransferDeleteGracePeriodSeconds
	if seconds <= 0 {
		return 5 * time.Minute
	}
	return time.Duration(seconds) * time.Second
}

// now returns the current time from the injectable clock, so tests can
// advance time without sleeping.
func (manager *TransferManagerService) now() time.Time {
	if manager.nowFunc != nil {
		return manager.nowFunc()
	}
	return time.Now()
}

// getOrTrackErroredTransfer remembers an errored transfer ID with its
// first-seen time and returns a copy of the current tracking state.
func (manager *TransferManagerService) getOrTrackErroredTransfer(id string, now time.Time) erroredTransferState {
	manager.erroredTransfersMutex.Lock()
	defer manager.erroredTransfersMutex.Unlock()
	state, ok := manager.erroredTransfers[id]
	if !ok {
		state = &erroredTransferState{firstSeen: now}
		manager.erroredTransfers[id] = state
		log.Debugf("Tracking errored transfer id %s (first seen %s)", id, now.Format(time.RFC3339))
	}
	return *state
}

// beginErroredTransferProcessing claims the per-transfer processing slot so
// a transfer is never reported or deleted by two goroutines at once.
// Returns false when processing is already in flight or the transfer is no
// longer tracked.
func (manager *TransferManagerService) beginErroredTransferProcessing(id string) bool {
	manager.erroredTransfersMutex.Lock()
	defer manager.erroredTransfersMutex.Unlock()
	state, ok := manager.erroredTransfers[id]
	if !ok || state.processing {
		return false
	}
	state.processing = true
	return true
}

// finishErroredTransferProcessing releases the per-transfer processing slot
// after a report/delete attempt finished, whatever its outcome.
func (manager *TransferManagerService) finishErroredTransferProcessing(id string) {
	manager.erroredTransfersMutex.Lock()
	defer manager.erroredTransfersMutex.Unlock()
	if state, ok := manager.erroredTransfers[id]; ok {
		state.processing = false
	}
}

// forgetErroredTransfer removes the tracking state for a transfer.
func (manager *TransferManagerService) forgetErroredTransfer(id string) {
	manager.erroredTransfersMutex.Lock()
	defer manager.erroredTransfersMutex.Unlock()
	delete(manager.erroredTransfers, id)
}

// pruneErroredTransfers removes the tracking state of transfers that
// disappeared from the premiumize.me transfer list.
func (manager *TransferManagerService) pruneErroredTransfers(currentTransferIDs map[string]bool) {
	manager.erroredTransfersMutex.Lock()
	defer manager.erroredTransfersMutex.Unlock()
	for id := range manager.erroredTransfers {
		if !currentTransferIDs[id] {
			log.Debugf("Errored transfer id %s disappeared from the transfer list, stopping tracking", id)
			delete(manager.erroredTransfers, id)
		}
	}
}

func (manager *TransferManagerService) TaskCheckPremiumizeDownloadsFolder() {
	log.Debug("Running Task CheckPremiumizeDownloadsFolder")

	if manager.downloadsFolderID == "" {
		log.Errorf("Premiumize-Download-Folder ID is empty, cannot check Folder - aborting (This could be due to a Premiumize or CDN Outage)")
		return
	}

	items, err := manager.premiumizemeClient.ListFolder(manager.downloadsFolderID)
	if err != nil {
		log.Errorf("Error listing downloads folder: %s", err.Error())
		return
	}

	for _, item := range items {
		// Skip items that are currently downloading
		if manager.downloadExists(item.Name) {
			log.Tracef("Item %s is already downloading", item.Name)
			continue
		}

		// Skip items in cooldown period after failed download
		if manager.isDownloadInCooldown(item.Name) {
			log.Debugf("Skipping item %s - in cooldown period after previous failure", item.Name)
			continue
		}

		if manager.countDownloads() < manager.config.SimultaneousDownloads {
			log.Debugf("Processing completed item: %s", item.Name)
			manager.HandleFinishedItem(item, manager.config.DownloadsDirectory)
			//Sleep for one Second to let Asynchronous Downloads Start and Update
			time.Sleep(time.Second * 1)
		} else {
			log.Debugf("Not processing any more transfers, %d are running and cap is %d", manager.countDownloads(), manager.config.SimultaneousDownloads)
			break
		}
	}
}

func (manager *TransferManagerService) updateTransfers(transfers []premiumizeme.Transfer) {
	manager.transfers = transfers
}

func (manager *TransferManagerService) addDownload(item *premiumizeme.Item, topLevel bool) {
	manager.downloadListMutex.Lock()
	defer manager.downloadListMutex.Unlock()

	manager.downloadList[item.Name] = &DownloadDetails{
		Added:              time.Now(),
		Name:               item.Name,
		ProgressDownloader: progress_downloader.NewWriteCounter(),
		topLevel:           topLevel,
	}
}

func (manager *TransferManagerService) countDownloads() int {
	manager.downloadListMutex.Lock()
	defer manager.downloadListMutex.Unlock()
	// Count active top-level jobs only: each is counted for its full
	// lifetime - listing, link generation, child downloads, and gaps
	// between children - until its deferred removal.
	count := 0
	for _, dl := range manager.downloadList {
		if dl.topLevel {
			count++
		}
	}
	return count
}

func (manager *TransferManagerService) removeDownload(name string) {
	manager.downloadListMutex.Lock()
	defer manager.downloadListMutex.Unlock()

	delete(manager.downloadList, name)
}

func (manager *TransferManagerService) downloadExists(itemName string) bool {
	manager.downloadListMutex.Lock()
	defer manager.downloadListMutex.Unlock()

	for _, dl := range manager.downloadList {
		if dl.Name == itemName {
			return true
		}
	}

	return false
}

func (manager *TransferManagerService) markDownloadFailed(itemName string) {
	manager.failedDownloadsMutex.Lock()
	defer manager.failedDownloadsMutex.Unlock()
	manager.failedDownloads[itemName] = time.Now()
	log.Warnf("Marked %s as failed, will retry after cooldown period", itemName)
}

func (manager *TransferManagerService) isDownloadInCooldown(itemName string) bool {
	manager.failedDownloadsMutex.Lock()
	defer manager.failedDownloadsMutex.Unlock()

	if failureTime, exists := manager.failedDownloads[itemName]; exists {
		// 30 minute cooldown period before retrying
		if time.Since(failureTime) < 30*time.Minute {
			log.Tracef("Item %s is in cooldown period (failed at %v)", itemName, failureTime)
			return true
		}
		// Cooldown expired, remove from failed list
		delete(manager.failedDownloads, itemName)
	}
	return false
}

func (manager *TransferManagerService) HandleFinishedItem(item premiumizeme.Item, downloadDirectory string) {
	if manager.downloadExists(item.Name) {
		log.Tracef("Transfer %s is already downloading", item.Name)
		return
	}

	// If single Item is encountered (Torrent Download) it is moved into a new Folder with the Name of the Item to be downloaded during next refresh
	if item.Type == "file" {
		log.Tracef("Handling Item Type File in finished Transfer %s", item.Name)

		id, err := manager.premiumizemeClient.CreateFolder(item.Name+".folder", &manager.downloadsFolderID)
		if err != nil {
			log.Errorf("cannot create Folder for Single File Download! %+v", err)
			return
		}
		var singleFileFolderID string = id

		err = manager.premiumizemeClient.MoveItem(item.ID, singleFileFolderID)
		if err != nil {
			log.Errorf("cannot move Single File to Folder for Download!  %+v", err)
			return
		}

		log.Infof("Single File moved to Folder for Download %s", item.Name)
		return
	}

	if item.Type != "folder" {
		log.Errorf("Item Type mismatch when trying to handle finished Transfer %s | %s", item.Name, item.Type)
		return
	}

	manager.addDownload(&item, true)
	go func() {
		defer manager.removeDownload(item.Name)
		err := manager.downloadFolderRecursively(item, downloadDirectory)
		if err != nil {
			log.Errorf("Error downloading item %s: %s", item.Name, err)
			// Mark parent folder as failed so it respects cooldown and doesn't block queue
			manager.markDownloadFailed(item.Name)
			return
		}

		err = manager.premiumizemeClient.DeleteFolder(item.ID)
		if err != nil {
			log.Errorf("Error deleting folder on premiumize.me: %s", err)
			return
		}

	}()
}

func (manager *TransferManagerService) downloadFolderRecursively(item premiumizeme.Item, downloadDirectory string) error {
	if item.ID == "" {
		return fmt.Errorf("Premiumize-Download-Folder ID is empty, cannot check Folder - aborting (This could be due to a Premiumize Outage)")
	}

	items, err := manager.premiumizemeClient.ListFolder(item.ID)
	if err != nil {
		return fmt.Errorf("error listing folder items: %w", err)
	}
	savePath := path.Join(downloadDirectory, (item.Name + "/"))
	log.Trace("Downloading to: ", savePath)
	err = os.Mkdir(savePath, os.ModePerm)
	if err != nil {
		log.Errorf("could not create save path: %s", err)
		//		manager.removeDownload(item.Name)
		//		return fmt.Errorf("error creating save path: %w", err)
		//		no return due to os permissions sometime inaccurately throwing errors on different configurations
	}

	var folderHasErrors bool = false
	var parentItemName = item.Name // Store parent folder name before loop to avoid variable shadowing
	for _, item := range items {
		if manager.downloadExists(item.Name) {
			log.Tracef("Transfer %s is already downloading", item.Name)
			continue
		}

		// Check cooldown for any item (file or folder) that previously failed
		if manager.isDownloadInCooldown(item.Name) {
			log.Debugf("Skipping item %s - in cooldown period after previous failure", item.Name)
			folderHasErrors = true // Still mark as error to prevent folder deletion
			continue
		}

		if item.Type == "file" {
			manager.addDownload(&item, false)
			link, err := manager.premiumizemeClient.GenerateFileLink(item.ID)
			if err != nil {
				log.Debugf("File Link Generation err: %s", err)
			}
			var fileSavePath = path.Join(savePath, item.Name)
			log.Trace("Downloading to: ", fileSavePath)
			// Add Option to Disable / Enable checking download certificate as certain CDNs have invalid / self-signed certificates
			var checkcertificate bool = manager.config.EnableTlsCheck
			var ratelimit int = manager.config.DownloadSpeedLimit
			err = progress_downloader.DownloadFile(checkcertificate, ratelimit, link, fileSavePath, manager.downloadList[item.Name].ProgressDownloader)
			if err != nil {
				manager.removeDownload(item.Name)
				manager.markDownloadFailed(item.Name)
				log.Errorf("Error downloading file %s: %s, continuing with other files", item.Name, err)
				folderHasErrors = true
				continue // Continue with next file instead of aborting
			}
			manager.removeDownload(item.Name)
		} else if item.Type == "folder" {
			err = manager.downloadFolderRecursively(item, savePath)
			if err != nil {
				manager.markDownloadFailed(item.Name)
				log.Errorf("Error downloading folder %s: %s, continuing with other items", item.Name, err)
				folderHasErrors = true
				continue // Continue with next item instead of aborting
			}
		}
	}

	// Return error if any items failed or are in cooldown to prevent parent folder deletion
	if folderHasErrors {
		return fmt.Errorf("folder %s had errors downloading some items, but completed others", parentItemName)
	}
	return nil
}
