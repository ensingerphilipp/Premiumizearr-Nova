package service

import (
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
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
	mu                 sync.RWMutex
	premiumizemeClient *premiumizeme.Premiumizeme
	config             *config.Config
	Queue              *stringqueue.StringQueue
	status             string
	quotaBlocked       bool
	downloadsFolderID  string
	watchDirectory     *directory_watcher.WatchDirectory
}

const (
	ERROR_LIMIT_REACHED    = "Limit of transfers reached!"
	ERROR_ALREADY_UPLOADED = "You already added this job."
)

func (DirectoryWatcherService) New() DirectoryWatcherService {
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

	dw.downloadsFolderID = utils.GetDownloadsFolderIDFromPremiumizeme(dw.premiumizemeClient, dw.config.TransferDirectory)

	log.Info("Creating Queue...")
	dw.Queue = stringqueue.NewStringQueue()

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
	dw.Queue.Add(path)
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

// processUploadCycle checks the account once and processes the files that were
// queued at the start of the cycle. Files added during processing wait for the
// next cycle and its fresh quota check.
func (dw *DirectoryWatcherService) processUploadCycle() int {
	queuedFiles := dw.Queue.Len()
	if queuedFiles == 0 || !dw.submissionsAllowed() {
		return 0
	}

	processed := 0
	for range queuedFiles {
		isQueueFile, filePath := dw.Queue.PopTopOfQueue()
		if !isQueueFile {
			break
		}
		if filePath == "" {
			log.Error("Received an empty path from the blackhole queue")
			continue
		}

		processed++
		if dw.processUpload(filePath) {
			// The account check and transfer submission are separate requests, so
			// the limit can be reached between them. Put the file back in the
			// queue and stop this batch so watcher mode retries it after backoff.
			dw.Queue.Add(filePath)
			return 0
		}
	}

	return processed
}

func (dw *DirectoryWatcherService) submissionsAllowed() bool {
	accountInfo, err := dw.premiumizemeClient.GetAccountInfo()
	if err != nil {
		log.Warnf("Could not check Premiumize fair-use quota; continuing with existing submission behavior: %s", err)
		dw.mu.Lock()
		dw.quotaBlocked = false
		dw.mu.Unlock()
		return true
	}

	exhausted := accountInfo.QuotaExhausted()
	dw.mu.Lock()
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

// processUpload returns true when the source should be queued for a later retry.
func (dw *DirectoryWatcherService) processUpload(filePath string) bool {
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
			return true
		case ERROR_ALREADY_UPLOADED:
			log.Trace("File already uploaded, removing from disk")
			if err := os.Remove(filePath); err != nil {
				log.Errorf("Could not delete %s: %+v", filePath, err)
			}
		default:
			log.Errorf("Error creating transfer: %s", err)
		}
		return false
	}

	dw.mu.Lock()
	dw.status = "Okay"
	dw.mu.Unlock()
	if err := os.Remove(filePath); err != nil {
		log.Errorf("Could not delete %s: %+v", filePath, err)
		return false
	}
	log.Infof("Removed %s from blackhole queue. Queue size: %d", filePath, dw.Queue.Len())
	return false
}

func (dw *DirectoryWatcherService) setTransferDirectory(newDir string) {
	newID := utils.GetDownloadsFolderIDFromPremiumizeme(dw.premiumizemeClient, newDir)

	dw.mu.Lock()
	defer dw.mu.Unlock()

	dw.downloadsFolderID = newID
}
