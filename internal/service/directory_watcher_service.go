package service

import (
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
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
	quotaBlocked                   bool
	quotaCheckFailed               bool
	activeTransferLimitBlocked     bool
	activeTransferCountCheckFailed bool
	downloadsFolderID              string
	watchDirectory                 *directory_watcher.WatchDirectory
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
}

func (dw *DirectoryWatcherService) ConfigUpdatedCallback(currentConfig config.Config, newConfig config.Config) {
	if currentConfig.BlackholeDirectory != newConfig.BlackholeDirectory {
		log.Info("Blackhole directory changed, restarting directory watcher...")
		log.Info("Running initial directory scan...")
		go dw.directoryScan(dw.config.BlackholeDirectory)
		dw.watchDirectory.UpdatePath(newConfig.BlackholeDirectory)
	}

	if currentConfig.TransferDirectory != newConfig.TransferDirectory {
		log.Info("TransferDirectory directory changed, changing directory watcher...")
		dw.setTransferDirectory(newConfig.TransferDirectory)
	}

	if currentConfig.PollBlackholeDirectory != newConfig.PollBlackholeDirectory {
		log.Info("Poll blackhole directory changed, restarting directory watcher...")
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

	dw.downloadsFolderID = utils.GetDownloadsFolderIDFromPremiumizeme(dw.premiumizemeClient, dw.config.TransferDirectory)

	log.Info("Starting uploads processor...")
	go dw.processUploads()

	log.Info("Running initial directory scan...")
	go dw.directoryScan(dw.config.BlackholeDirectory)

	if dw.watchDirectory != nil {
		log.Info("Stopping directory watcher...")
		err := dw.watchDirectory.Stop()
		if err != nil {
			log.Errorf("Error stopping directory watcher: %s", err)
		}
	}

	if dw.config.PollBlackholeDirectory {
		log.Info("Starting directory poller...")
		go func() {
			for {
				if !dw.config.PollBlackholeDirectory {
					log.Info("Directory poller stopped")
					break
				}
				time.Sleep(time.Duration(dw.config.PollBlackholeIntervalMinutes) * time.Minute)
				log.Infof("Running directory scan of %s", dw.config.BlackholeDirectory)
				dw.directoryScan(dw.config.BlackholeDirectory)
				log.Infof("Scan complete, next scan in %d minutes", dw.config.PollBlackholeIntervalMinutes)
			}
		}()
	} else {
		log.Info("Starting directory watcher...")
		dw.watchDirectory = directory_watcher.NewDirectoryWatcher(dw.config.BlackholeDirectory,
			true,
			dw.checkFile,
			dw.addFileToQueue,
		)
		dw.watchDirectory.Watch()
	}
}

func (dw *DirectoryWatcherService) directoryScan(p string) {
	log.Trace("Running directory scan")
	files, err := ioutil.ReadDir(p)
	if err != nil {
		log.Errorf("Error with directory scan %+v", err)
		return
	}

	for _, file := range files {
		filePath := path.Join(p, file.Name())
		if dw.checkFile(filePath) == 1 {
			dw.addFileToQueue(filePath)
		}
	}
}

func (dw *DirectoryWatcherService) checkFile(path string) int {
	log.Tracef("Checking file %s", path)

	fi, err := os.Stat(path)
	if err != nil {
		log.Errorf("Error checking file %s", path)
		return 0
	}

	if fi.IsDir() {
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

func (dw *DirectoryWatcherService) addFileToQueue(path string) {
	if !dw.Queue.AddIfAbsent(path) {
		return
	}
	log.Infof("File created in blackhole %s added to Queue. Queue length %d", path, dw.Queue.Len())
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

// processUploadCycle checks the account and Premiumize's active transfer count
// before each blackhole submission. Successfully submitted transfers are also
// reserved locally so a delayed transfer/list response cannot overfill the cap.
func (dw *DirectoryWatcherService) processUploadCycle() int {
	queuedFiles := dw.Queue.Len()
	if queuedFiles == 0 || !dw.submissionsAllowed() {
		return 0
	}

	maxActiveTransfers := dw.config.MaxActivePremiumizeTransfers
	reservedActiveTransfers := -1
	processed := 0
	for range queuedFiles {
		if processed > 0 {
			time.Sleep(2 * time.Second)
		}

		activeTransfers := 0
		if maxActiveTransfers > 0 {
			var err error
			activeTransfers, err = dw.activePremiumizeTransferCount()
			if err != nil {
				dw.pauseForTransferCountError(err)
				return processed
			}
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
			continue
		}

		processed++
		retry, submitted := dw.processUpload(filePath)
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

func (dw *DirectoryWatcherService) pauseForTransferCountError(err error) {
	dw.mu.Lock()
	alreadyFailed := dw.activeTransferCountCheckFailed
	dw.activeTransferCountCheckFailed = true
	dw.activeTransferLimitBlocked = false
	dw.status = "Paused: Premiumize active transfer count unavailable"
	dw.mu.Unlock()

	if !alreadyFailed {
		log.Warnf("Could not check active Premiumize transfers; blackhole submissions are paused: %s", err)
	}
}

func (dw *DirectoryWatcherService) updateActiveTransferLimitStatus(active, limit int) {
	dw.mu.Lock()
	wasBlocked := dw.activeTransferLimitBlocked
	checkRecovered := dw.activeTransferCountCheckFailed
	dw.activeTransferCountCheckFailed = false
	blocked := active >= limit
	dw.activeTransferLimitBlocked = blocked
	if blocked {
		dw.status = fmt.Sprintf("Paused: %d active Premiumize transfers (limit %d)", active, limit)
	} else {
		dw.status = "Okay"
	}
	dw.mu.Unlock()

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
		dw.mu.Lock()
		alreadyFailed := dw.quotaCheckFailed
		dw.quotaCheckFailed = true
		dw.status = "Premiumize fair-use quota unknown; continuing submissions"
		// Preserve the last known quota state across failed lookups so a
		// transient error does not duplicate pause logs or hide recovery.
		dw.mu.Unlock()
		if !alreadyFailed {
			log.Warnf("Could not check Premiumize fair-use quota; continuing with existing submission behavior: %s", err)
		}
		return true
	}

	exhausted := accountInfo.QuotaExhausted()
	dw.mu.Lock()
	dw.quotaCheckFailed = false
	wasBlocked := dw.quotaBlocked
	dw.quotaBlocked = exhausted
	if exhausted {
		dw.status = "Paused: Premiumize fair-use quota exhausted"
	} else {
		dw.status = "Okay"
	}
	dw.mu.Unlock()

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
	folderID := dw.downloadsFolderID
	dw.mu.RUnlock()

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
