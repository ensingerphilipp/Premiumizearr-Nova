package service

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/ensingerphilipp/premiumizearr-nova/internal/config"
	"github.com/ensingerphilipp/premiumizearr-nova/internal/directory_watcher"
	"github.com/ensingerphilipp/premiumizearr-nova/internal/utils"
	"github.com/ensingerphilipp/premiumizearr-nova/pkg/premiumizeme"
	"github.com/ensingerphilipp/premiumizearr-nova/pkg/stringqueue"
	log "github.com/sirupsen/logrus"
)

type DirectoryWatcherService struct {
	mu                             sync.RWMutex
	premiumizemeClient             *premiumizeme.Premiumizeme
	config                         *config.Config
	Queue                          *stringqueue.StringQueue
	status                         string
	blackholeDirectory             string
	quotaBlocked                   bool
	quotaCheckFailed               bool
	activeTransferLimitBlocked     bool
	activeTransferCountCheckFailed bool
	downloadsFolderID              string
	watchDirectory                 *directory_watcher.WatchDirectory
	arrFolders                     map[string]string // Arr slug -> pme subfolder ID
}

const (
	ERROR_LIMIT_REACHED    = "Limit of transfers reached!"
	ERROR_ALREADY_UPLOADED = "You already added this job."
)

func NewDirectoryWatcherService() DirectoryWatcherService {
	return DirectoryWatcherService{
		premiumizemeClient: nil,
		config:             nil,
		Queue:              nil,
		status:             "",
		downloadsFolderID:  "",
	}
}

func (dw *DirectoryWatcherService) Init(premiumizemeClient *premiumizeme.Premiumizeme, config *config.Config) {
	dw.premiumizemeClient = premiumizemeClient
	dw.config = config
	dw.mu.Lock()
	dw.blackholeDirectory = config.BlackholeDirectory
	dw.mu.Unlock()
	// Register this service's lock as the config swap lock: UpdateConfig
	// takes it around the in-place struct replacement, and this service's
	// goroutines read config fields under the matching read lock.
	dw.config.SetUpdateMu(&dw.mu)
}

func (dw *DirectoryWatcherService) ConfigUpdatedCallback(currentConfig config.Config, newConfig config.Config) {
	blackholeChanged := currentConfig.BlackholeDirectory != newConfig.BlackholeDirectory
	transferChanged := currentConfig.TransferDirectory != newConfig.TransferDirectory
	arrsChanged := !reflect.DeepEqual(currentConfig.Arrs, newConfig.Arrs)
	toggleChanged := currentConfig.EnableArrSubfolders != newConfig.EnableArrSubfolders

	dw.mu.Lock()
	dw.blackholeDirectory = newConfig.BlackholeDirectory
	dw.mu.Unlock()

	if blackholeChanged {
		log.Info("Blackhole directory changed, restarting directory watcher...")
		log.Info("Running initial directory scan...")
		go dw.scanDirectory(newConfig.BlackholeDirectory)

		if dw.watchDirectory != nil {
			dw.watchDirectory.UpdatePath(newConfig.BlackholeDirectory)

			// Snapshot the keys under the lock: the map must not be
			// iterated while resolveSingleArrFolder writes to it.
			dw.mu.RLock()
			slugs := make([]string, 0, len(dw.arrFolders))
			for slug := range dw.arrFolders {
				slugs = append(slugs, slug)
			}
			dw.mu.RUnlock()
			for _, slug := range slugs {
				dw.watchDirectory.RemoveWatchPath(filepath.Join(currentConfig.BlackholeDirectory, slug))
			}
		}
	}

	if transferChanged {
		log.Info("TransferDirectory directory changed, changing directory watcher...")
		dw.setTransferDirectory(newConfig.TransferDirectory)
	}

	if currentConfig.PollBlackholeDirectory != newConfig.PollBlackholeDirectory {
		log.Info("Poll blackhole directory changed, restarting directory watcher...")
	}

	if currentConfig.MaxActivePremiumizeTransfers != newConfig.MaxActivePremiumizeTransfers {
		log.Infof("MaxActivePremiumizeTransfers changed from %d to %d; the new value takes effect on the next upload cycle", currentConfig.MaxActivePremiumizeTransfers, newConfig.MaxActivePremiumizeTransfers)
		if newConfig.MaxActivePremiumizeTransfers <= 0 {
			// Disabling the cap drops the next cycle's cap gate, so no
			// cap-owned status write happens; clear the pause state the cap
			// left behind, or the Paused status stays pinned and a stale
			// blocked flag suppresses the one-per-episode pause warning
			// when the cap is re-enabled at the limit.
			dw.resetActiveTransferLimitState()
		}
	}

	if blackholeChanged || transferChanged || arrsChanged || toggleChanged {
		dw.resolveArrFolders()
	}
}

func (dw *DirectoryWatcherService) GetStatus() string {
	dw.mu.RLock()
	defer dw.mu.RUnlock()
	return dw.status
}

// Start: This is the entrypoint for the directory watcher
func (dw *DirectoryWatcherService) Start() {
	log.Info("Starting directory watcher...")

	log.Info("Creating Queue...")
	dw.Queue = stringqueue.NewStringQueue()

	dw.mu.RLock()
	transferDir := dw.config.TransferDirectory
	blackholeDir := dw.config.BlackholeDirectory
	poll := dw.config.PollBlackholeDirectory
	dw.mu.RUnlock()

	newID := utils.GetDownloadsFolderIDFromPremiumizeme(dw.premiumizemeClient, transferDir)
	dw.mu.Lock()
	dw.downloadsFolderID = newID
	dw.mu.Unlock()

	log.Info("Starting uploads processor...")
	go dw.processUploads()

	log.Info("Running initial directory scan...")
	go dw.scanDirectory(blackholeDir)

	if dw.watchDirectory != nil {
		log.Info("Stopping directory watcher...")
		err := dw.watchDirectory.Stop()
		if err != nil {
			log.Errorf("Error stopping directory watcher: %s", err)
		}
	}

	if poll {
		log.Info("Starting directory poller...")
		go func() {
			for {
				dw.mu.RLock()
				polling := dw.config.PollBlackholeDirectory
				interval := dw.config.PollBlackholeIntervalMinutes
				dw.mu.RUnlock()
				if !polling {
					log.Info("Directory poller stopped")
					break
				}
				time.Sleep(time.Duration(interval) * time.Minute)
				directory := dw.getBlackholeDirectory()
				log.Infof("Running directory scan of %s", directory)
				dw.scanDirectory(directory)
				dw.scanArrFolders()
				log.Infof("Scan complete, next scan in %d minutes", interval)
			}
		}()
	} else {
		log.Info("Starting directory watcher...")
		dw.watchDirectory = directory_watcher.NewDirectoryWatcher(blackholeDir,
			true,
			dw.checkFile,
			dw.addFileToQueue,
		)
		if err := dw.watchDirectory.Watch(); err != nil {
			// A watcher that cannot be created degrades to the one-time
			// startup scan instead of crashing the daemon: nil the handle so
			// the AddWatchPath/RemoveWatchPath/UpdatePath call sites stay
			// no-ops.
			log.Errorf("Error starting directory watcher: %s", err)
			dw.watchDirectory = nil
		}
	}

	dw.resolveArrFolders()
}

// clearArrFolders stops watching all known Arr subfolders and forgets their
// pme IDs, without deleting anything on disk or on pme.
func (dw *DirectoryWatcherService) clearArrFolders(blackholeDir string) {
	dw.mu.Lock()
	oldFolders := dw.arrFolders
	dw.arrFolders = nil
	dw.mu.Unlock()

	if dw.watchDirectory != nil {
		for slug := range oldFolders {
			dw.watchDirectory.RemoveWatchPath(filepath.Join(blackholeDir, slug))
		}
	}
}

// resolveArrFolders ensures each configured Arr has a local blackhole subfolder
// and a matching pme subfolder, and watches/scans it for existing files.
func (dw *DirectoryWatcherService) resolveArrFolders() {
	// Snapshot everything under the read lock before any network/FS work:
	// UpdateConfig swaps the whole config struct on the web goroutine and
	// an RWMutex is not reentrant, so the loop below must not read
	// dw.config again.
	dw.mu.RLock()
	enabled := dw.config.EnableArrSubfolders
	blackholeDir := dw.config.BlackholeDirectory
	arrs := make([]config.ArrConfig, len(dw.config.Arrs))
	copy(arrs, dw.config.Arrs)
	mainFolderID := dw.downloadsFolderID
	oldFolders := make(map[string]string, len(dw.arrFolders))
	for slug, folderID := range dw.arrFolders {
		oldFolders[slug] = folderID
	}
	dw.mu.RUnlock()

	if !enabled {
		dw.clearArrFolders(blackholeDir)
		return
	}

	newFolders := make(map[string]string, len(arrs))

	for _, arr := range arrs {
		local := filepath.Join(blackholeDir, arr.Name)
		id, err := utils.GetOrCreateSubfolderID(dw.premiumizemeClient, mainFolderID, arr.Name)
		if err != nil {
			log.Errorf("Cannot resolve premiumize.me subfolder for Arr %s: %s", arr.Name, err)
			// A transient premiumize.me failure must not discard a
			// previously resolved subfolder ID: carry the mapping forward so
			// queued uploads keep their target folder.
			if prev, ok := oldFolders[arr.Name]; ok {
				newFolders[arr.Name] = prev
			}
			// The local subfolder is still created, watched and scanned:
			// the watch stays in place so an upload into it can retry the
			// resolution once premiumize.me recovers.
			if err := os.MkdirAll(local, os.ModePerm); err != nil {
				log.Errorf("Cannot create blackhole subfolder for Arr %s: %s", arr.Name, err)
			} else {
				dw.watchArrFolder(local)
				dw.directoryScan(local)
			}
			continue
		}

		if err := os.MkdirAll(local, os.ModePerm); err != nil {
			log.Errorf("Cannot create blackhole subfolder for Arr %s: %s", arr.Name, err)
			// Drop the entry: a slug whose local subfolder could not be
			// created is not fully resolved.
			continue
		}
		newFolders[arr.Name] = id
		dw.watchArrFolder(local)
		dw.directoryScan(local)
	}

	dw.mu.Lock()
	dw.arrFolders = newFolders
	dw.mu.Unlock()

	configuredSlugs := make(map[string]bool, len(arrs))
	for _, arr := range arrs {
		configuredSlugs[arr.Name] = true
	}

	for slug := range oldFolders {
		if configuredSlugs[slug] {
			continue
		}
		if dw.watchDirectory != nil {
			dw.watchDirectory.RemoveWatchPath(filepath.Join(blackholeDir, slug))
		}
		log.Infof("Arr %s no longer configured, local/premiumize subfolder is kept but no longer watched", slug)
	}
}

// watchArrFolder registers the watch on a local Arr subfolder when the
// watcher is available; a failed registration is logged, not fatal.
func (dw *DirectoryWatcherService) watchArrFolder(local string) {
	if dw.watchDirectory == nil {
		return
	}
	if err := dw.watchDirectory.AddWatchPath(local); err != nil {
		log.Errorf("Cannot watch blackhole subfolder %s: %s", local, err)
	}
}

func (dw *DirectoryWatcherService) scanArrFolders() {
	dw.mu.RLock()
	enabled := dw.config.EnableArrSubfolders
	blackholeDir := dw.config.BlackholeDirectory
	arrs := make([]config.ArrConfig, len(dw.config.Arrs))
	copy(arrs, dw.config.Arrs)
	dw.mu.RUnlock()

	if !enabled {
		return
	}

	for _, arr := range arrs {
		local := filepath.Join(blackholeDir, arr.Name)
		if _, err := os.Stat(local); err != nil {
			continue
		}
		dw.directoryScan(local)
	}
}

// ScanNow scans the configured blackhole directory and adds supported files to
// the upload queue. It returns after the scan is complete so callers can report
// whether the request succeeded.
func (dw *DirectoryWatcherService) ScanNow() (int, error) {
	if dw.config == nil {
		return 0, fmt.Errorf("directory watcher is not initialized")
	}

	return dw.directoryScan(dw.getBlackholeDirectory())
}

func (dw *DirectoryWatcherService) getBlackholeDirectory() string {
	dw.mu.RLock()
	defer dw.mu.RUnlock()
	return dw.blackholeDirectory
}

func (dw *DirectoryWatcherService) scanDirectory(p string) {
	if _, err := dw.directoryScan(p); err != nil {
		log.Error(err)
	}
}

func (dw *DirectoryWatcherService) directoryScan(p string) (int, error) {
	log.Trace("Running directory scan")
	files, err := ioutil.ReadDir(p)
	if err != nil {
		return 0, fmt.Errorf("error scanning blackhole directory: %w", err)
	}

	queued := 0
	for _, file := range files {
		filePath := filepath.Join(p, file.Name())
		if dw.checkFile(filePath) == 1 && dw.queueFile(filePath) {
			queued++
		}
	}

	return queued, nil
}

func (dw *DirectoryWatcherService) checkFile(path string) int {
	log.Tracef("Checking file %s", path)

	fi, err := os.Stat(path)
	if err != nil {
		log.Errorf("Error checking file %s", path)
		return 0
	}

	if fi.IsDir() {
		if dw.isConfiguredArrSlug(filepath.Base(path)) {
			log.Tracef("Directory %s is a configured Arr subfolder, handled separately", path)
			// (Re)establish the subfolder's own watch - fsnotify re-Add of an
			// already-watched path is idempotent - and rescan it, so a
			// subfolder that lost its watch (pme outage at resolution,
			// delete+recreate, failed AddWatchPath) self-heals in watch mode
			// and files that landed while it was down get queued.
			dw.watchArrFolder(path)
			go dw.directoryScan(path)
			return 0
		}
		log.Errorf("Directory created in blackhole %s ignoring (Warning premiumizearrd does not look in subfolders!)", path)
		return 2
	}

	ext := filepath.Ext(path)
	if ext == ".nzb" || ext == ".magnet" || ext == ".torrent" {
		return 1
	} else {
		return 0
	}
}

func (dw *DirectoryWatcherService) isConfiguredArrSlug(slug string) bool {
	dw.mu.RLock()
	defer dw.mu.RUnlock()
	if !dw.config.EnableArrSubfolders {
		return false
	}
	for _, arr := range dw.config.Arrs {
		if arr.Name == slug {
			return true
		}
	}
	return false
}

func (dw *DirectoryWatcherService) addFileToQueue(path string) {
	dw.queueFile(path)
}

func (dw *DirectoryWatcherService) queueFile(path string) bool {
	if !dw.Queue.AddIfAbsent(path) {
		log.Tracef("File %s is already in the blackhole queue", path)
		return false
	}

	log.Infof("File created in blackhole %s added to Queue. Queue length %d", path, dw.Queue.Len())
	return true
}

// resolveSingleArrFolder resolves (creating if needed) the pme subfolder for
// one Arr slug on demand, using the caller's snapshot of the main folder ID.
func (dw *DirectoryWatcherService) resolveSingleArrFolder(slug string, mainFolderID string) (string, bool) {
	dw.mu.RLock()
	enabled := dw.config.EnableArrSubfolders
	dw.mu.RUnlock()
	if !enabled {
		return "", false
	}

	id, err := utils.GetOrCreateSubfolderID(dw.premiumizemeClient, mainFolderID, slug)
	if err != nil {
		log.Errorf("Cannot resolve premiumize.me subfolder for Arr %s: %s", slug, err)
		return "", false
	}

	dw.mu.Lock()
	if dw.arrFolders == nil {
		dw.arrFolders = map[string]string{}
	}
	dw.arrFolders[slug] = id
	dw.mu.Unlock()

	return id, true
}

func resolveTargetFolderID(filePath, blackholeDir, mainFolderID string, arrFolders map[string]string) (folderID string, ok bool, slug string) {
	dir := filepath.Clean(filepath.Dir(filePath))
	cleanBlackhole := filepath.Clean(blackholeDir)
	if dir == cleanBlackhole {
		return mainFolderID, true, ""
	}
	// A file whose parent is not the current blackhole and not a direct
	// subfolder of it (a leftover of a previous blackhole location) goes to
	// the main downloads folder: reporting a slug for it would make
	// processUpload create a pme folder named after a directory the user
	// never defined.
	if !strings.HasPrefix(dir, cleanBlackhole+string(filepath.Separator)) {
		return mainFolderID, mainFolderID != "", ""
	}

	slug = filepath.Base(dir)
	id, found := arrFolders[slug]
	return id, found, slug
}

func (dw *DirectoryWatcherService) processUploads() {
	for {
		processed := dw.processUploadCycle()
		if processed == 0 {
			if dw.Queue.Len() == 0 {
				log.Trace("No files in queue, sleeping for 10 seconds")
			} else {
				log.Trace("Blackhole submissions are paused, checking quota again in 10 seconds")
			}
			time.Sleep(time.Second * time.Duration(10))
		} else {
			time.Sleep(2 * time.Second)
		}
	}
}

// processUploadCycle checks the account before each blackhole submission and
// fetches the active Premiumize transfer count once per cycle: each
// /transfer/list call downloads and decodes the account's complete unpaginated
// transfer history, so a per-file re-list would grow the cycle's request
// volume with queue depth. Successfully submitted transfers are also reserved
// locally so the cycle's own submissions count against the cap without
// re-fetching; external additions or completions are picked up by the next
// cycle's fetch, and a limit-reached create failure requeues the file as the
// backstop.
func (dw *DirectoryWatcherService) processUploadCycle() int {
	queuedFiles := dw.Queue.Len()
	if queuedFiles == 0 || !dw.submissionsAllowed() {
		return 0
	}

	maxActiveTransfers := dw.config.MaxActivePremiumizeTransfers
	reservedActiveTransfers := -1
	processed := 0
	var cycleActiveTransfers int
	if maxActiveTransfers > 0 {
		var err error
		cycleActiveTransfers, err = dw.activePremiumizeTransferCount()
		if err != nil {
			dw.pauseForTransferCountError(err)
			return 0
		}
	}
	for range queuedFiles {
		if processed > 0 {
			time.Sleep(2 * time.Second)
		}

		activeTransfers := 0
		if maxActiveTransfers > 0 {
			activeTransfers = cycleActiveTransfers
			if reservedActiveTransfers > activeTransfers {
				activeTransfers = reservedActiveTransfers
			}
			dw.updateActiveTransferLimitStatus(activeTransfers, maxActiveTransfers)
			if activeTransfers >= maxActiveTransfers {
				return processed
			}
		}

		isQueueFile, filePath := dw.Queue.PopTopOfQueue()
		if !isQueueFile {
			break
		}
		if filePath == "" {
			log.Error("Received an empty path from the blackhole queue")
			dw.Queue.Done(filePath)
			continue
		}

		processed++
		retry, submitted := dw.processUpload(filePath)
		dw.Queue.Done(filePath)
		if retry {
			// Premiumize may reach its own limit between the list check and the
			// submission. Keep the source queued and retry after the next poll.
			dw.Queue.Add(filePath)
			return 0
		}
		if submitted {
			reservedActiveTransfers = activeTransfers + 1
		}
	}

	return processed
}

func (dw *DirectoryWatcherService) activePremiumizeTransferCount() (int, error) {
	transfers, err := dw.premiumizemeClient.GetTransfers()
	if err != nil {
		return 0, err
	}

	active := 0
	for _, transfer := range transfers {
		switch strings.ToLower(strings.TrimSpace(transfer.Status)) {
		case "queued", "running":
			active++
		}
	}
	return active, nil
}

// setPausedState applies one pause/recovery transition of a pause mechanism
// (the fair-use quota or the active transfer count) under dw.mu: it
// snapshots the mechanism's check-failed and blocked flags, replaces the
// check-failed flag with failedValue, replaces the blocked flag with
// blockedValue unless blockedFlag is nil (which preserves its last value),
// and stores status. It returns the previous flag values so the caller can
// log the transition. Both failed-check paths (quota and transfer count)
// pass a nil blockedFlag on purpose: a failed lookup must preserve the last
// known block state so a transient error does not re-arm the pause warning
// or hide recovery; only the successful-check paths replace the blocked flag.
func (dw *DirectoryWatcherService) setPausedState(failedFlag, blockedFlag *bool, failedValue, blockedValue bool, status string) (previouslyFailed, wasBlocked bool) {
	dw.mu.Lock()
	previouslyFailed = *failedFlag
	if blockedFlag != nil {
		wasBlocked = *blockedFlag
		*blockedFlag = blockedValue
	}
	*failedFlag = failedValue
	dw.status = status
	dw.mu.Unlock()
	return previouslyFailed, wasBlocked
}

func (dw *DirectoryWatcherService) pauseForTransferCountError(err error) {
	// A failed count check preserves the block state (nil blockedFlag), as
	// the quota error path does, so a transient failure while at the cap does
	// not re-arm the pause warning when the check recovers still at the cap.
	alreadyFailed, _ := dw.setPausedState(&dw.activeTransferCountCheckFailed, nil, true, false, "Paused: Premiumize active transfer count unavailable")

	if !alreadyFailed {
		log.Warnf("Could not check active Premiumize transfers; blackhole submissions are paused: %s", err)
	}
}

// resetActiveTransferLimitState clears the cap-derived pause state after the
// cap has been disabled, restoring the status the quota branch still owns.
func (dw *DirectoryWatcherService) resetActiveTransferLimitState() {
	dw.mu.RLock()
	quotaUnknown := dw.quotaCheckFailed
	dw.mu.RUnlock()

	status := "Okay"
	if quotaUnknown {
		// The quota branch owns the quota-unknown status, mirroring the
		// healthy-cap branch of updateActiveTransferLimitStatus.
		status = "Premiumize fair-use quota unknown; continuing submissions"
	}
	dw.setPausedState(&dw.activeTransferCountCheckFailed, &dw.activeTransferLimitBlocked, false, false, status)
}

func (dw *DirectoryWatcherService) updateActiveTransferLimitStatus(active, limit int) {
	dw.mu.RLock()
	quotaUnknown := dw.quotaCheckFailed
	dw.mu.RUnlock()

	blocked := active >= limit
	status := "Okay"
	switch {
	case blocked:
		status = fmt.Sprintf("Paused: %d active Premiumize transfers (limit %d)", active, limit)
	case quotaUnknown:
		// The quota branch owns the quota-unknown status: a healthy cap
		// check must not clobber it with "Okay" while the fair-use quota is
		// unknown and the submission is failing, so the recovery write is
		// gated on the quota lookup being known-good. The cap-paused branch
		// stays unconditional: a real pause at the limit is always reported.
		status = "Premiumize fair-use quota unknown; continuing submissions"
	}
	checkRecovered, wasBlocked := dw.setPausedState(&dw.activeTransferCountCheckFailed, &dw.activeTransferLimitBlocked, false, blocked, status)

	if checkRecovered {
		log.Info("Premiumize active transfer count is available again; resuming transfer checks")
	}
	if blocked && !wasBlocked {
		log.Warnf("Premiumize has %d active transfers; pausing blackhole submissions at configured limit %d", active, limit)
	} else if !blocked && wasBlocked {
		log.Info("Premiumize active transfer count is below the configured limit; resuming blackhole submissions")
	}
}

func (dw *DirectoryWatcherService) submissionsAllowed() bool {
	accountInfo, err := dw.premiumizemeClient.GetAccountInfo()
	if err != nil {
		alreadyFailed, _ := dw.setPausedState(&dw.quotaCheckFailed, nil, true, false, "Premiumize fair-use quota unknown; continuing submissions")
		if !alreadyFailed {
			log.Warnf("Could not check Premiumize fair-use quota; continuing with existing submission behavior: %s", err)
		}
		return true
	}

	exhausted := accountInfo.QuotaExhausted()
	status := "Okay"
	if exhausted {
		status = "Paused: Premiumize fair-use quota exhausted"
	}
	_, wasBlocked := dw.setPausedState(&dw.quotaCheckFailed, &dw.quotaBlocked, false, exhausted, status)

	if exhausted && !wasBlocked {
		log.Warn("Premiumize fair-use quota is exhausted and no booster points are available; new blackhole submissions are paused and files will remain untouched")
	} else if !exhausted && wasBlocked {
		log.Info("Premiumize fair-use quota is available; resuming blackhole submissions")
	}

	return !exhausted
}

// processUpload returns whether the source needs a retry and whether a new transfer was submitted.
func (dw *DirectoryWatcherService) processUpload(filePath string) (retry, submitted bool) {
	log.Debugf("Processing %s", filePath)
	dw.mu.RLock()
	folderID, ok, slug := resolveTargetFolderID(filePath, dw.config.BlackholeDirectory, dw.downloadsFolderID, dw.arrFolders)
	mainFolderID := dw.downloadsFolderID
	dw.mu.RUnlock()
	if !ok && slug != "" {
		if dw.isConfiguredArrSlug(slug) {
			folderID, ok = dw.resolveSingleArrFolder(slug, mainFolderID)
		} else {
			// A file in a subfolder that is not a configured Arr (feature
			// off, or a name no Arr uses) keeps the pre-feature destination
			// instead of having a pme folder created for the arbitrary name.
			folderID, ok = mainFolderID, mainFolderID != ""
		}
	}
	if !ok {
		log.Errorf("No resolved target folder for %s, skipping upload", filePath)
		return false, false
	}

	err := dw.premiumizemeClient.CreateTransfer(filePath, folderID)
	if err != nil {
		switch err.Error() {
		case ERROR_LIMIT_REACHED:
			dw.mu.Lock()
			dw.status = "Limit of transfers reached!"
			dw.mu.Unlock()
			log.Trace("Transfer limit reached; the source file remains in the blackhole directory")
			return true, false
		case ERROR_ALREADY_UPLOADED:
			log.Trace("File already uploaded, removing from disk")
			if err := os.Remove(filePath); err != nil {
				log.Errorf("Could not delete %s: %+v", filePath, err)
			}
		default:
			log.Errorf("Error creating transfer: %s", err)
		}
		return false, false
	}

	dw.mu.Lock()
	dw.status = "Okay"
	dw.mu.Unlock()
	if err := os.Remove(filePath); err != nil {
		log.Errorf("Could not delete %s: %+v", filePath, err)
		return false, true
	}
	log.Infof("Removed %s from blackhole queue. Queue size: %d", filePath, dw.Queue.Len())
	return false, true
}

func (dw *DirectoryWatcherService) setTransferDirectory(newDir string) {
	newID := utils.GetDownloadsFolderIDFromPremiumizeme(dw.premiumizemeClient, newDir)

	dw.mu.Lock()
	defer dw.mu.Unlock()

	dw.downloadsFolderID = newID
}
