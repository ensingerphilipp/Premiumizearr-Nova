package service

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"reflect"
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
	// FolderID is the premiumize.me folder the item was admitted from.
	// It scopes download/cooldown dedup: same-named items in different
	// folders are independent jobs and must not block, overwrite, or
	// cause premature folder deletion of each other.
	FolderID string
	// topLevel marks entries for top-level folder jobs admitted by
	// HandleFinishedItem; only these count against SimultaneousDownloads.
	topLevel bool
}

// downloadKey is the composite key for downloadList and failedDownloads:
// the premiumize.me folder ID an item was admitted from, plus the item
// name. The NUL separator keeps keys unique for any folder ID and name.
func downloadKey(folderID, name string) string {
	return folderID + "\x00" + name
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
	arrFoldersMutex       *sync.Mutex
	arrFolders            map[string]string // Arr slug -> premiumize.me subfolder ID
	erroredTransfersMutex *sync.Mutex
	erroredTransfers      map[string]*erroredTransferState // Maps premiumize.me transfer ID to grace-period tracking state
	processingWG          *sync.WaitGroup                  // Tracks in-flight report/delete goroutines so callers can synchronize with them
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
	t.arrFoldersMutex = &sync.Mutex{}
	t.arrFolders = make(map[string]string, 0)
	t.erroredTransfersMutex = &sync.Mutex{}
	t.erroredTransfers = make(map[string]*erroredTransferState, 0)
	t.processingWG = &sync.WaitGroup{}
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
	downloadsDirChanged := currentConfig.DownloadsDirectory != newConfig.DownloadsDirectory
	transferChanged := currentConfig.TransferDirectory != newConfig.TransferDirectory
	arrsChanged := !reflect.DeepEqual(currentConfig.Arrs, newConfig.Arrs)
	toggleChanged := currentConfig.EnableArrSubfolders != newConfig.EnableArrSubfolders

	if downloadsDirChanged {
		log.Trace("Inside ConfigUpdatedCallback")
		manager.CleanUpDownloadDir()
	}

	if transferChanged {
		log.Trace("Updating Transfer Directory in TransferManagerService")
		newDir := newConfig.TransferDirectory
		newID := utils.GetDownloadsFolderIDFromPremiumizeme(manager.premiumizemeClient, newDir)
		// The field is also read by the poll loop; both sides take
		// arrFoldersMutex, the service-state lock.
		manager.arrFoldersMutex.Lock()
		manager.downloadsFolderID = newID
		manager.arrFoldersMutex.Unlock()
	}

	if downloadsDirChanged || transferChanged || arrsChanged || toggleChanged {
		manager.resolveArrFolders()
	}
}

func (manager *TransferManagerService) clearArrFolders() {
	manager.arrFoldersMutex.Lock()
	defer manager.arrFoldersMutex.Unlock()
	manager.arrFolders = make(map[string]string, 0)
}

func (manager *TransferManagerService) resolveArrFolders() {
	// Snapshot the config fields under the config swap read lock: the web
	// goroutine replaces the whole config struct in place, and this
	// function's loop must not read the live struct.
	enabled := false
	var arrs []config.ArrConfig
	downloadsDir := ""
	if mu := config.UpdateMu(); mu != nil {
		mu.RLock()
		enabled = manager.config.EnableArrSubfolders
		arrs = append([]config.ArrConfig(nil), manager.config.Arrs...)
		downloadsDir = manager.config.DownloadsDirectory
		mu.RUnlock()
	} else {
		enabled = manager.config.EnableArrSubfolders
		arrs = append([]config.ArrConfig(nil), manager.config.Arrs...)
		downloadsDir = manager.config.DownloadsDirectory
	}

	if !enabled {
		// Keep the last tracked folder IDs instead of clearing them: the
		// root-scan exclusion layers must still exclude the pme containers
		// this client created while the feature was on, and their
		// unprocessed remainder must still be downloaded into the local
		// subfolder - otherwise the next scan downloads the container's
		// content into the main downloads directory and deletes the
		// container from premiumize.me. A fresh install never had the
		// feature on, so its map is empty and the scan excludes nothing
		// (exactly pre-feature behavior).
		return
	}

	manager.arrFoldersMutex.Lock()
	// Seed the new map with the previously tracked slugs as empty-ID entries:
	// an Arr that is renamed away or that fails re-resolution must stay in the
	// map, because the map doubles as the root-scan exclusion list - a slug
	// that leaves the map makes its pme folder downloadable and deletable
	// again. An empty ID means "tracked, excluded, not processed".
	oldSlugs := make([]string, 0, len(manager.arrFolders))
	for slug := range manager.arrFolders {
		oldSlugs = append(oldSlugs, slug)
	}
	mainFolderID := manager.downloadsFolderID
	manager.arrFoldersMutex.Unlock()

	newFolders := make(map[string]string, len(oldSlugs)+len(arrs))
	for _, slug := range oldSlugs {
		newFolders[slug] = ""
	}

	for _, arr := range arrs {
		// No pme round trip without a resolved main folder: an empty
		// parent ID would list/create at the account root instead of the
		// transfers directory. Unresolved means "keep" (empty ID) and the
		// next resolution run retries.
		var id string
		var err error
		if mainFolderID != "" {
			id, err = utils.GetOrCreateSubfolderID(manager.premiumizemeClient, mainFolderID, arr.Name)
		}
		if mainFolderID == "" || err != nil {
			if mainFolderID != "" {
				log.Errorf("Cannot resolve premiumize.me subfolder for Arr %s: %s", arr.Name, err)
			}
			// Unresolved means "keep": the slug stays tracked with an empty
			// ID so its pme folder remains excluded from the root scan's
			// download-and-delete path until resolution succeeds again.
			newFolders[arr.Name] = ""
			continue
		}
		newFolders[arr.Name] = id

		local := filepath.Join(downloadsDir, arr.Name)
		if err := os.MkdirAll(local, os.ModePerm); err != nil {
			log.Errorf("Cannot create downloads subfolder for Arr %s: %s", arr.Name, err)
			// Keep the resolved pme ID (finding S-4): a local filesystem
			// hiccup must not discard a successful premiumize.me
			// resolution - overwriting it with "" would make the per-slug
			// pass skip the slug forever and the slug would never be
			// processed again. The slug stays tracked with its real ID:
			// excluded from the root scan as before, the download path
			// recreates missing parents itself, and the next poll retries
			// the MkdirAll.
			continue
		}
	}

	manager.arrFoldersMutex.Lock()
	manager.arrFolders = newFolders
	manager.arrFoldersMutex.Unlock()
}

// retryUnresolvedArrFolders heals tracked Arr slugs whose pme folder is
// still unresolved (empty ID) after a failed resolution run - a transient
// premiumize.me failure or a failed local MkdirAll (finding S-22). A slug
// is retried during the normal poll only while it is still a configured
// Arr; a slug that was renamed away or removed stays tracked with its
// empty ID (excluded from the root scan) and is never re-resolved, because
// re-creating the pme folder for a name the user no longer configures
// would resurrect a folder the user abandoned. Any failure keeps the empty
// ID so the next poll retries.
func (manager *TransferManagerService) retryUnresolvedArrFolders() {
	// Snapshot the config fields under the config swap read lock: the web
	// goroutine replaces the whole config struct in place.
	enabled := false
	var arrs []config.ArrConfig
	downloadsDir := ""
	if mu := config.UpdateMu(); mu != nil {
		mu.RLock()
		enabled = manager.config.EnableArrSubfolders
		arrs = append([]config.ArrConfig(nil), manager.config.Arrs...)
		downloadsDir = manager.config.DownloadsDirectory
		mu.RUnlock()
	} else {
		enabled = manager.config.EnableArrSubfolders
		arrs = append([]config.ArrConfig(nil), manager.config.Arrs...)
		downloadsDir = manager.config.DownloadsDirectory
	}
	if !enabled {
		return
	}

	manager.arrFoldersMutex.Lock()
	mainFolderID := manager.downloadsFolderID
	if mainFolderID == "" {
		manager.arrFoldersMutex.Unlock()
		return
	}
	var unresolved []string
	for slug, folderID := range manager.arrFolders {
		if folderID != "" {
			continue
		}
		configured := false
		for _, arr := range arrs {
			if arr.Name == slug {
				configured = true
				break
			}
		}
		if configured {
			unresolved = append(unresolved, slug)
		}
	}
	manager.arrFoldersMutex.Unlock()

	for _, slug := range unresolved {
		id, err := utils.GetOrCreateSubfolderID(manager.premiumizemeClient, mainFolderID, slug)
		if err != nil {
			log.Errorf("Retrying premiumize.me subfolder for Arr %s failed, retrying on next poll: %s", slug, err)
			continue
		}
		local := filepath.Join(downloadsDir, slug)
		if err := os.MkdirAll(local, os.ModePerm); err != nil {
			log.Errorf("Retrying downloads subfolder for Arr %s failed, retrying on next poll: %s", slug, err)
			continue
		}

		// Re-validate under the config swap read lock after the pme round
		// trip: the feature may have been toggled off or the slug renamed
		// away in the meantime - in both cases the slug must not be
		// upgraded, it stays tracked (and excluded) with its empty ID.
		stillEnabled := false
		stillConfigured := false
		if mu := config.UpdateMu(); mu != nil {
			mu.RLock()
			stillEnabled = manager.config.EnableArrSubfolders
			for _, arr := range manager.config.Arrs {
				if arr.Name == slug {
					stillConfigured = true
					break
				}
			}
			mu.RUnlock()
		} else {
			stillEnabled = manager.config.EnableArrSubfolders
			for _, arr := range manager.config.Arrs {
				if arr.Name == slug {
					stillConfigured = true
					break
				}
			}
		}
		if !stillEnabled || !stillConfigured {
			continue
		}

		manager.arrFoldersMutex.Lock()
		// The main folder may have changed while the pme call was in
		// flight (transfer directory switch): a folder created under the
		// old parent does not belong to the new state.
		if manager.downloadsFolderID != mainFolderID {
			manager.arrFoldersMutex.Unlock()
			continue
		}
		// Upgrade only from an empty ID: a concurrent resolution that
		// already resolved the slug keeps its result.
		if current, ok := manager.arrFolders[slug]; ok && current == "" {
			manager.arrFolders[slug] = id
		}
		manager.arrFoldersMutex.Unlock()
	}
}

func (manager *TransferManagerService) Run(interval time.Duration) {
	manager.arrFoldersMutex.Lock()
	manager.downloadsFolderID = utils.GetDownloadsFolderIDFromPremiumizeme(manager.premiumizemeClient, manager.config.TransferDirectory)
	manager.arrFoldersMutex.Unlock()
	manager.resolveArrFolders()
	for {
		manager.runningTask = true
		manager.TaskUpdateTransfersList()
		// Snapshot under the config swap read lock: the web goroutine
		// replaces the whole config struct in place.
		transferOnlyMode := false
		if mu := config.UpdateMu(); mu != nil {
			mu.RLock()
			transferOnlyMode = manager.config.TransferOnlyMode
			mu.RUnlock()
		} else {
			transferOnlyMode = manager.config.TransferOnlyMode
		}
		if !transferOnlyMode {
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
			manager.stopTrackingErroredTransfer(transfer.ID)
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
// Only an authoritative no-match on every configured *arr past the grace
// period leads to deletion - and even then only after a fresh history
// lookup, because the cached history may predate a recent grab. A failed
// history lookup is never treated as a no-match, and with no *arr
// configured there is nothing to notify, so neither case can cause
// automatic deletion.
func (manager *TransferManagerService) processErroredTransfer(transfer *premiumizeme.Transfer) {
	now := manager.now()
	state := manager.getOrTrackErroredTransfer(transfer.ID, now)
	arrClients := manager.arrsManager.GetArrs()

	if len(arrClients) == 0 {
		// No *arr is configured, so the transfer cannot be verified
		// against any history and no *arr could be notified about the
		// failed download: keep it instead of deleting it.
		log.Debugf("Errored transfer %s (id %s) is kept because no *arr client is configured", transfer.Name, transfer.ID)
		return
	}

	matched, matchedID, allLookupsSucceeded := manager.searchArrHistory(arrClients, transfer, false)
	if matched != nil {
		if !manager.beginErroredTransferProcessing(transfer.ID) {
			log.Debugf("Errored transfer %s (id %s) is already being processed, skipping this poll", transfer.Name, transfer.ID)
			return
		}
		manager.spawnErroredTransferProcessing(func() {
			manager.reportErroredTransferToArr(matched, matchedID, transfer)
		})
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

	// The grace period has elapsed with a no-match on every *arr, but the
	// cached history may be older than a recent grab (the cache refreshes
	// on ArrHistoryUpdateIntervalSeconds, independent of the grace
	// period), so force a fresh lookup before deleting: a fresh match is
	// reported to the *arr like any other match, a fresh lookup failure
	// keeps the transfer, and only a fresh authoritative no-match deletes.
	// The *arr set is re-fetched for the deletion decision because a
	// config update may have changed it since the poll started: an *arr
	// added mid-poll must be consulted before anything is deleted, and an
	// *arr set that became empty makes the no-match non-authoritative.
	freshClients := manager.arrsManager.GetArrs()
	if len(freshClients) == 0 {
		log.Debugf("Errored transfer %s (id %s) is kept because no *arr client is configured for the deletion decision", transfer.Name, transfer.ID)
		return
	}
	matched, matchedID, allLookupsSucceeded = manager.searchArrHistory(freshClients, transfer, true)
	if matched != nil {
		if !manager.beginErroredTransferProcessing(transfer.ID) {
			log.Debugf("Errored transfer %s (id %s) is already being processed, skipping this poll", transfer.Name, transfer.ID)
			return
		}
		manager.spawnErroredTransferProcessing(func() {
			manager.reportErroredTransferToArr(matched, matchedID, transfer)
		})
		return
	}
	if !allLookupsSucceeded {
		log.Debugf("Errored transfer %s (id %s) fresh history lookup did not succeed on every *arr, keeping it for the next poll", transfer.Name, transfer.ID)
		return
	}

	log.Debugf("Errored transfer %s (id %s) still unmatched after the grace period of %s, deleting it", transfer.Name, transfer.ID, grace)
	if !manager.beginErroredTransferProcessing(transfer.ID) {
		log.Debugf("Errored transfer %s (id %s) is already being processed, skipping this poll", transfer.Name, transfer.ID)
		return
	}
	manager.spawnErroredTransferProcessing(func() {
		manager.deleteUnmatchedErroredTransfer(transfer)
	})
}

// searchArrHistory checks the transfer name against the history of every
// configured *arr and returns the first matching arr client plus its
// grabbed record ID, or (nil, 0) when no *arr matched. When fresh is set,
// each lookup refetches the history from the *arr instead of using the
// cached copy, so a stale cache cannot hide a recent grab. The third
// return value is true only when every *arr answered without an error,
// which is what makes a no-match authoritative.
func (manager *TransferManagerService) searchArrHistory(arrClients []arr.IArr, transfer *premiumizeme.Transfer, fresh bool) (arr.IArr, int64, bool) {
	allLookupsSucceeded := true
	var matched arr.IArr
	var matchedID int64
	for _, arrClient := range arrClients {
		log.Tracef("Checking errored transfer %s against %s history", transfer.Name, arrClient.GetArrName())
		var arrID int64
		var found bool
		var err error
		if fresh {
			arrID, found, err = arrClient.HistoryContainsFresh(transfer.Name)
		} else {
			arrID, found, err = arrClient.HistoryContains(transfer.Name)
		}
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
	return matched, matchedID, allLookupsSucceeded
}

// spawnErroredTransferProcessing runs fn in a goroutine counted in
// processingWG, so callers can wait until all in-flight report/delete work
// has settled.
func (manager *TransferManagerService) spawnErroredTransferProcessing(fn func()) {
	manager.processingWG.Add(1)
	go func() {
		defer manager.processingWG.Done()
		fn()
	}()
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
	log.Infof("Errored transfer %s (id %s) handed to %s for the failure report and deleted from premiumize.me", transfer.Name, transfer.ID, arrClient.GetArrName())
	manager.completeAndForgetErroredTransfer(transfer.ID)
}

// deleteUnmatchedErroredTransfer deletes an errored transfer that stayed
// unmatched on every reachable *arr for the whole grace period. Because
// the delete is irreversible, the transfer is re-listed first and only
// deleted while it is still errored: a transfer premiumize repaired (or
// removed) in the meantime is settled instead of deleted, and a failed
// re-list keeps the state for a retry. The warning carries the transfer
// ID, name and error so the deletion stays auditable. The per-transfer
// processing slot is always released; the tracking state is removed only
// after a successful deletion, so a failed deletion is retried on the
// next poll.
func (manager *TransferManagerService) deleteUnmatchedErroredTransfer(transfer *premiumizeme.Transfer) {
	defer manager.finishErroredTransferProcessing(transfer.ID)
	stillErrored, err := manager.transferStillErrored(transfer.ID)
	if err != nil {
		log.Errorf("Failed to re-check errored transfer %s (id %s) before deleting it, retrying on next poll: %s", transfer.Name, transfer.ID, err.Error())
		return
	}
	if !stillErrored {
		log.Infof("Errored transfer %s (id %s) is no longer errored or no longer listed, not deleting it", transfer.Name, transfer.ID)
		manager.completeAndForgetErroredTransfer(transfer.ID)
		return
	}
	if err := manager.premiumizemeClient.DeleteTransfer(transfer.ID); err != nil {
		log.Errorf("Failed to delete unmatched errored transfer %s (id %s) from premiumize.me, retrying on next poll: %s", transfer.Name, transfer.ID, err.Error())
		return
	}
	log.Warnf("Deleted errored transfer %q (id %s) from premiumize.me after the grace period without a *arr history match; transfer error: %s", transfer.Name, transfer.ID, transfer.Message)
	manager.completeAndForgetErroredTransfer(transfer.ID)
}

// transferStillErrored re-fetches the premiumize.me transfer list and
// reports whether the transfer with id is still present and still errored
// (the status captured at poll start may be stale by the time the delete
// runs). It returns (false, nil) when the transfer is gone or no longer
// errored and (false, err) when the list could not be fetched.
func (manager *TransferManagerService) transferStillErrored(id string) (bool, error) {
	transfers, err := manager.premiumizemeClient.GetTransfers()
	if err != nil {
		return false, err
	}
	for _, t := range transfers {
		if t.ID == id {
			return t.Status == "error", nil
		}
	}
	return false, nil
}

// maxErroredTransferGraceSeconds caps the configured grace period so an
// out-of-range value cannot overflow the duration arithmetic into a
// negative grace (which would delete immediately); 10 years is
// effectively "keep forever" for this purpose.
const maxErroredTransferGraceSeconds = 10 * 365 * 24 * 60 * 60

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
	if seconds > maxErroredTransferGraceSeconds {
		seconds = maxErroredTransferGraceSeconds
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
// after a report/delete attempt finished, whatever its outcome. On a
// failed attempt the tracking state is kept (same first-seen time) so the
// next poll retries; on a successful attempt the state was already removed
// by completeAndForgetErroredTransfer.
func (manager *TransferManagerService) finishErroredTransferProcessing(id string) {
	manager.erroredTransfersMutex.Lock()
	defer manager.erroredTransfersMutex.Unlock()
	if state, ok := manager.erroredTransfers[id]; ok {
		state.processing = false
	}
}

// completeAndForgetErroredTransfer atomically releases the per-transfer
// processing slot and removes the tracking state after a successful
// report/delete. Both steps happen under one lock acquisition so no poll
// can observe a released-but-not-yet-removed entry and start a duplicate
// goroutine.
func (manager *TransferManagerService) completeAndForgetErroredTransfer(id string) {
	manager.erroredTransfersMutex.Lock()
	defer manager.erroredTransfersMutex.Unlock()
	if state, ok := manager.erroredTransfers[id]; ok {
		state.processing = false
	}
	delete(manager.erroredTransfers, id)
}

// stopTrackingErroredTransfer is the poller-side way of dropping the
// tracking state for a transfer that is no longer errored. An entry whose
// report/delete goroutine is still in flight is left alone: that goroutine
// still owns the processing slot and settles the state itself, and any
// leftover state is dropped by the next poll or by pruneErroredTransfers.
func (manager *TransferManagerService) stopTrackingErroredTransfer(id string) {
	manager.erroredTransfersMutex.Lock()
	defer manager.erroredTransfersMutex.Unlock()
	if state, ok := manager.erroredTransfers[id]; ok && state.processing {
		return
	}
	delete(manager.erroredTransfers, id)
}

// pruneErroredTransfers removes the tracking state of transfers that
// disappeared from the premiumize.me transfer list. Entries with an
// in-flight report/delete are kept: the goroutine still owns the
// processing slot and settles the state, the next poll prunes the
// leftover.
func (manager *TransferManagerService) pruneErroredTransfers(currentTransferIDs map[string]bool) {
	manager.erroredTransfersMutex.Lock()
	defer manager.erroredTransfersMutex.Unlock()
	for id, state := range manager.erroredTransfers {
		if currentTransferIDs[id] {
			continue
		}
		if state.processing {
			continue
		}
		log.Debugf("Errored transfer id %s disappeared from the transfer list, stopping tracking", id)
		delete(manager.erroredTransfers, id)
	}
}

// rootExclusions is the exclusion state for one downloads-folder scan pass.
// Folders in it are client-managed Arr routing folders that the scan must
// keep - never download, never delete.
type rootExclusions struct {
	// byName: folders named like a tracked slug whose pme folder is not
	// (or no longer) resolved - the name is the only signal left for
	// those. A resolved slug's container is NOT name-excluded: excluding
	// by name would also skip a legitimate content folder that happens to
	// be named like the slug.
	byName map[string]bool
	// byID: folders this client created and still tracks, plus the
	// destination folders of non-error transfers. Keyed by ID, so a
	// premiumize.me-side rename cannot un-exclude them.
	byID map[string]bool
}

func (manager *TransferManagerService) TaskCheckPremiumizeDownloadsFolder() {
	log.Debug("Running Task CheckPremiumizeDownloadsFolder")

	// Snapshot the config fields under the config swap read lock: the web
	// goroutine replaces the whole config struct in place.
	enabled := false
	downloadsDir := ""
	if mu := config.UpdateMu(); mu != nil {
		mu.RLock()
		enabled = manager.config.EnableArrSubfolders
		downloadsDir = manager.config.DownloadsDirectory
		mu.RUnlock()
	} else {
		enabled = manager.config.EnableArrSubfolders
		downloadsDir = manager.config.DownloadsDirectory
	}

	manager.arrFoldersMutex.Lock()
	downloadsFolderID := manager.downloadsFolderID
	manager.arrFoldersMutex.Unlock()
	if downloadsFolderID == "" {
		log.Errorf("Premiumize-Download-Folder ID is empty, cannot check Folder - aborting (This could be due to a Premiumize or CDN Outage)")
		return
	}

	// Heal tracked slugs whose pme folder is still unresolved before the
	// exclusion layers are snapshotted: a slug healed here is excluded by
	// ID and processed by the per-slug pass below in this same poll
	// (finding S-22).
	manager.retryUnresolvedArrFolders()

	// The exclusion layers apply when the feature is on - or when it was
	// on at some point for this manager: the tracked map still holds the
	// IDs of the pme containers this client created, and those containers
	// must stay excluded (with their unprocessed remainder still
	// downloaded into the local subfolder), otherwise the next scan
	// downloads the container's content into the main downloads directory
	// and deletes the container from premiumize.me. A fresh install never
	// had the feature on, so its map is empty and the scan excludes
	// nothing (exactly pre-feature behavior).
	var arrFolders map[string]string
	var transferDestinations map[string]bool
	manager.arrFoldersMutex.Lock()
	if enabled || len(manager.arrFolders) > 0 {
		arrFolders = make(map[string]string, len(manager.arrFolders))
		for slug, folderID := range manager.arrFolders {
			arrFolders[slug] = folderID
		}

		// Identity layer: the transfer/list endpoint reports, for every
		// non-error transfer, the destination folder (folder_id) it was
		// placed into once finished. A folder whose own ID appears here is
		// therefore a client-managed routing folder - including old or
		// renamed Arr subfolders whose slug no longer matches the
		// configured Arr names, where the name-based layer can no longer
		// recognize them. Such folders are kept, never downloaded and
		// deleted. manager.transfers is the last successful fetch: Run()
		// runs TaskUpdateTransfersList and this task sequentially in one
		// goroutine. If the list endpoint has never succeeded the set is
		// empty and the slug layers below are the remaining protection.
		for _, t := range manager.transfers {
			if t.FolderID != "" && t.Status != "error" {
				if transferDestinations == nil {
					transferDestinations = make(map[string]bool)
				}
				transferDestinations[t.FolderID] = true
			}
		}
	}
	manager.arrFoldersMutex.Unlock()

	exclusions := rootExclusions{
		byName: make(map[string]bool, len(arrFolders)),
		byID:   make(map[string]bool, len(arrFolders)+len(transferDestinations)),
	}
	for slug, folderID := range arrFolders {
		if folderID == "" {
			exclusions.byName[slug] = true
		} else {
			exclusions.byID[folderID] = true
		}
	}
	for id := range transferDestinations {
		exclusions.byID[id] = true
	}

	if !manager.checkFolder(downloadsFolderID, downloadsDir, exclusions) {
		return
	}

	for slug, folderID := range arrFolders {
		if !enabled {
			// Feature off: the retained containers stay excluded from the
			// root scan, but their content is not re-processed into local
			// subfolders either - that would change the destination of
			// files the user may already have moved.
			continue
		}
		if folderID == "" {
			// Tracked but unresolved: the pme folder stays excluded from the
			// root scan, and there is nothing to process for this slug yet.
			continue
		}
		localDir := filepath.Join(downloadsDir, slug)
		if !manager.checkFolder(folderID, localDir, rootExclusions{}) {
			return // SimultaneousDownloads cap reached
		}
	}
}

func (manager *TransferManagerService) checkFolder(premiumizeFolderID, localDir string, exclusions rootExclusions) bool {
	items, err := manager.premiumizemeClient.ListFolder(premiumizeFolderID)
	if err != nil {
		log.Errorf("Error listing downloads folder: %s", err.Error())
		return true
	}

	// Snapshot under the config swap read lock: SimultaneousDownloads is a
	// config field and the web goroutine replaces the struct in place.
	simultaneousDownloads := 0
	if mu := config.UpdateMu(); mu != nil {
		mu.RLock()
		simultaneousDownloads = manager.config.SimultaneousDownloads
		mu.RUnlock()
	} else {
		simultaneousDownloads = manager.config.SimultaneousDownloads
	}

	for _, item := range items {
		// Client-managed routing folders are kept - never downloaded and
		// deleted. Only folders count: a file named like a configured
		// slug is a regular download, not an Arr subfolder. The two
		// layers are combined: a slug without a resolved pme folder is
		// recognizable by name only, while a resolved or transfer-
		// destination folder is recognized by ID (rename-proof).
		if item.Type == "folder" && (exclusions.byName[item.Name] || exclusions.byID[item.ID]) {
			log.Debugf("Keeping folder %s (id %s): client-managed Arr routing folder, not transfer content", item.Name, item.ID)
			continue
		}

		// Skip items that are currently downloading
		if manager.downloadExists(premiumizeFolderID, item.Name) {
			log.Tracef("Item %s is already downloading", item.Name)
			continue
		}

		// Skip items in cooldown period after failed download
		if manager.isDownloadInCooldown(premiumizeFolderID, item.Name) {
			log.Debugf("Skipping item %s - in cooldown period after previous failure", item.Name)
			continue
		}

		if manager.countDownloads() >= simultaneousDownloads {
			log.Debugf("Not processing any more transfers, %d are running and cap is %d", manager.countDownloads(), simultaneousDownloads)
			return false
		}

		log.Debugf("Processing completed item: %s", item.Name)
		manager.HandleFinishedItem(item, localDir, premiumizeFolderID, exclusions)
		time.Sleep(time.Second * 1)
	}
	return true
}

func (manager *TransferManagerService) updateTransfers(transfers []premiumizeme.Transfer) {
	manager.transfers = transfers
}

func (manager *TransferManagerService) addDownload(item *premiumizeme.Item, folderID string, topLevel bool) {
	manager.downloadListMutex.Lock()
	defer manager.downloadListMutex.Unlock()

	manager.downloadList[downloadKey(folderID, item.Name)] = &DownloadDetails{
		Added:              time.Now(),
		Name:               item.Name,
		FolderID:           folderID,
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

func (manager *TransferManagerService) removeDownload(folderID, name string) {
	manager.downloadListMutex.Lock()
	defer manager.downloadListMutex.Unlock()

	delete(manager.downloadList, downloadKey(folderID, name))
}

func (manager *TransferManagerService) downloadExists(folderID, itemName string) bool {
	manager.downloadListMutex.Lock()
	defer manager.downloadListMutex.Unlock()

	for _, dl := range manager.downloadList {
		if dl.FolderID == folderID && dl.Name == itemName {
			return true
		}
	}

	return false
}

func (manager *TransferManagerService) markDownloadFailed(folderID, itemName string) {
	manager.failedDownloadsMutex.Lock()
	defer manager.failedDownloadsMutex.Unlock()
	manager.failedDownloads[downloadKey(folderID, itemName)] = time.Now()
	// The folder ID is part of the tracking key: without it, a failed item
	// and its cooldown are indistinguishable from a same-named item in
	// another folder.
	log.Warnf("Marked item %s (folder %s) as failed, will retry after cooldown period", itemName, folderID)
}

func (manager *TransferManagerService) isDownloadInCooldown(folderID, itemName string) bool {
	manager.failedDownloadsMutex.Lock()
	defer manager.failedDownloadsMutex.Unlock()

	if failureTime, exists := manager.failedDownloads[downloadKey(folderID, itemName)]; exists {
		// 30 minute cooldown period before retrying
		if time.Since(failureTime) < 30*time.Minute {
			log.Tracef("Item %s (folder %s) is in cooldown period (failed at %v)", itemName, folderID, failureTime)
			return true
		}
		// Cooldown expired, remove from failed list
		delete(manager.failedDownloads, downloadKey(folderID, itemName))
	}
	return false
}

func (manager *TransferManagerService) HandleFinishedItem(item premiumizeme.Item, downloadDirectory string, premiumizeParentFolderID string, exclusions rootExclusions) {
	if manager.downloadExists(premiumizeParentFolderID, item.Name) {
		log.Tracef("Transfer %s is already downloading", item.Name)
		return
	}

	// If single Item is encountered (Torrent Download) it is moved into a new Folder with the Name of the Item to be downloaded during next refresh
	if item.Type == "file" {
		log.Tracef("Handling Item Type File in finished Transfer %s", item.Name)

		id, err := manager.premiumizemeClient.CreateFolder(item.Name+".folder", &premiumizeParentFolderID)
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

	manager.addDownload(&item, premiumizeParentFolderID, true)
	go func() {
		defer manager.removeDownload(premiumizeParentFolderID, item.Name)
		err := manager.downloadFolderRecursively(item, downloadDirectory, exclusions)
		if err != nil {
			log.Errorf("Error downloading item %s: %s", item.Name, err)
			// Mark parent folder as failed so it respects cooldown and doesn't block queue
			manager.markDownloadFailed(premiumizeParentFolderID, item.Name)
			return
		}

		err = manager.premiumizemeClient.DeleteFolder(item.ID)
		if err != nil {
			log.Errorf("Error deleting folder on premiumize.me: %s", err)
			return
		}

	}()
}

func (manager *TransferManagerService) downloadFolderRecursively(item premiumizeme.Item, downloadDirectory string, exclusions rootExclusions) error {
	if item.ID == "" {
		return fmt.Errorf("Premiumize-Download-Folder ID is empty, cannot check Folder - aborting (This could be due to a Premiumize Outage)")
	}

	items, err := manager.premiumizemeClient.ListFolder(item.ID)
	if err != nil {
		return fmt.Errorf("error listing folder items: %w", err)
	}
	savePath := path.Join(downloadDirectory, (item.Name + "/"))
	log.Trace("Downloading to: ", savePath)
	// MkdirAll (not Mkdir): a missing intermediate directory - e.g. an Arr
	// subfolder whose creation failed at resolution time - is recreated
	// instead of failing every download into a permanent cooldown loop.
	err = os.MkdirAll(savePath, os.ModePerm)
	if err != nil {
		log.Errorf("could not create save path: %s", err)
		//		manager.removeDownload(item.ID, item.Name)
		//		return fmt.Errorf("error creating save path: %w", err)
		//		no return due to os permissions sometime inaccurately throwing errors on different configurations
	}

	var folderHasErrors bool = false
	var parentItemName = item.Name // Store parent folder name before loop to avoid variable shadowing
	// parentFolderID scopes child download/cooldown dedup to this folder:
	// same-named children in different folders are independent jobs.
	parentFolderID := item.ID
	for _, child := range items {
		if manager.downloadExists(parentFolderID, child.Name) {
			log.Tracef("Transfer %s is already downloading", child.Name)
			continue
		}

		// Check cooldown for any item (file or folder) that previously failed
		if manager.isDownloadInCooldown(parentFolderID, child.Name) {
			log.Debugf("Skipping item %s - in cooldown period after previous failure", child.Name)
			folderHasErrors = true // Still mark as error to prevent folder deletion
			continue
		}

		if child.Type == "file" {
			manager.addDownload(&child, parentFolderID, false)
			link, err := manager.premiumizemeClient.GenerateFileLink(child.ID)
			if err != nil {
				log.Debugf("File Link Generation err: %s", err)
			}
			var fileSavePath = path.Join(savePath, child.Name)
			log.Trace("Downloading to: ", fileSavePath)
			// Add Option to Disable / Enable checking download certificate as certain CDNs have invalid / self-signed certificates
			var checkcertificate bool = manager.config.EnableTlsCheck
			var ratelimit int = manager.config.DownloadSpeedLimit
			// Read the progress counter under the downloadList lock: an
			// unlocked map read here would race with the locked writes
			// made by other top-level download goroutines.
			manager.downloadListMutex.Lock()
			progress := manager.downloadList[downloadKey(parentFolderID, child.Name)].ProgressDownloader
			manager.downloadListMutex.Unlock()
			err = progress_downloader.DownloadFile(checkcertificate, ratelimit, link, fileSavePath, progress)
			if err != nil {
				manager.removeDownload(parentFolderID, child.Name)
				manager.markDownloadFailed(parentFolderID, child.Name)
				log.Errorf("Error downloading file %s: %s, continuing with other files", child.Name, err)
				folderHasErrors = true
				continue // Continue with next file instead of aborting
			}
			manager.removeDownload(parentFolderID, child.Name)
		} else if child.Type == "folder" {
			if exclusions.byID[child.ID] {
				// A client-managed routing folder nested inside the
				// content folder must not be downloaded and deleted with
				// its parent: failing the parent job keeps the whole
				// subtree on premiumize.me.
				log.Debugf("Keeping nested folder %s (id %s): client-managed Arr routing folder, not transfer content", child.Name, child.ID)
				folderHasErrors = true
				continue
			}
			err = manager.downloadFolderRecursively(child, savePath, exclusions)
			if err != nil {
				manager.markDownloadFailed(parentFolderID, child.Name)
				log.Errorf("Error downloading folder %s: %s, continuing with other items", child.Name, err)
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
