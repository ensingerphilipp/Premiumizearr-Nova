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
	// configSnapshot is this service's synchronized view of the shared
	// config: the web route rewrites the App's config struct IN PLACE
	// (PersistUpdate's *c = _newConfig), so long-lived loops read the
	// snapshot under the mutex instead of the struct — the same
	// value-snapshot fix the directclient manager got for the identical
	// race.
	configSnapshot    config.Config
	Queue             *stringqueue.StringQueue
	status            string
	quotaBlocked      bool
	quotaCheckFailed  bool
	downloadsFolderID string
	watchDirectory    *directory_watcher.WatchDirectory
	polling           bool
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
	dw.mu.Lock()
	dw.premiumizemeClient = premiumizemeClient
	dw.config = config
	dw.configSnapshot = *config
	dw.mu.Unlock()
}

func (dw *DirectoryWatcherService) ConfigUpdatedCallback(currentConfig config.Config, newConfig config.Config) {
	// The shared config struct is rewritten in place by the web route;
	// refresh the synchronized snapshot first so every consumer (the
	// poll loop, the watch start below) reads the new values under the
	// mutex.
	dw.mu.Lock()
	dw.config = &newConfig
	dw.configSnapshot = newConfig
	dw.mu.Unlock()

	if currentConfig.BlackholeDirectory != newConfig.BlackholeDirectory {
		newDir := newConfig.BlackholeDirectory
		if _, err := os.Stat(newDir); err != nil {
			// A configured directory that does not exist yet must not
			// leave the PREVIOUS directory being processed: a watcher on
			// it still consumes its create events (and keeps uploading
			// into the queue) while the new path is never watched —
			// stopping it here mirrors the pre-PR unconditioned
			// UpdatePath, which removed the old directory even on a
			// change to a not-yet-existing one. The directory starts
			// through this same path once a later update points at an
			// existing one.
			dw.mu.Lock()
			if dw.watchDirectory != nil {
				log.Info("Stopping directory watcher for the removed blackhole directory...")
				if err := dw.watchDirectory.Stop(); err != nil {
					log.Errorf("Error stopping directory watcher: %s", err)
				}
				dw.watchDirectory = nil
			}
			dw.mu.Unlock()
			log.Info("Blackhole directory is unavailable; the directory watcher will start once it exists")
		} else {
			log.Info("Blackhole directory changed, restarting directory watcher...")
			dw.mu.RLock()
			watcher := dw.watchDirectory
			dw.mu.RUnlock()
			if watcher == nil {
				// The directory was missing at Start, so no watcher ever
				// ran; now that it exists, run the usual initial scan
				// plus watcher (or poller) start.
				dw.startBlackholeWatch(newDir)
			} else {
				log.Info("Running initial directory scan...")
				go dw.directoryScan(newDir)
				if err := watcher.UpdatePath(newDir); err != nil {
					log.Warnf("Could not update blackhole watcher: %v", err)
				}
			}
		}
	}
	// A directory that comes into existence at the ALREADY-CONFIGURED
	// path (missing at Start, so the watcher is nil and no watcher ever
	// ran) must start watching even when this callback was triggered by
	// an unrelated config field: the path-change arm above is the only
	// other re-arm point, and a config save that leaves the path
	// untouched can otherwise leave the directory unwatched forever.
	dw.mu.RLock()
	watcherNil := dw.watchDirectory == nil
	polling := dw.polling
	dw.mu.RUnlock()
	if watcherNil && !polling {
		if _, err := os.Stat(newConfig.BlackholeDirectory); err == nil {
			log.Info("Configured blackhole directory appeared, starting directory watcher...")
			dw.startBlackholeWatch(newConfig.BlackholeDirectory)
		}
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
	if _, err := os.Stat(dw.config.BlackholeDirectory); err != nil {
		log.Info("Blackhole directory is unavailable; direct *arr clients can operate without it")
		return
	}

	dw.startBlackholeWatch(dw.config.BlackholeDirectory)
}

// startBlackholeWatch runs the initial scan of dir and then starts either
// the inotify watcher or the poll-mode scan loop for it. Start uses it at
// boot and ConfigUpdatedCallback uses it for a blackhole directory that
// only appears later, so the two stay in lockstep.
func (dw *DirectoryWatcherService) startBlackholeWatch(dir string) {
	dw.mu.Lock()
	defer dw.mu.Unlock()

	log.Info("Running initial directory scan...")
	go dw.directoryScan(dir)

	if dw.watchDirectory != nil {
		log.Info("Stopping directory watcher...")
		err := dw.watchDirectory.Stop()
		if err != nil {
			log.Errorf("Error stopping directory watcher: %s", err)
		}
	}

	if dw.configSnapshot.PollBlackholeDirectory {
		if !dw.polling {
			dw.polling = true
			log.Info("Starting directory poller...")
			go func() {
				defer func() {
					dw.mu.Lock()
					dw.polling = false
					dw.mu.Unlock()
				}()
				for {
					// The shared config struct is rewritten in place by
					// the web route; the snapshot is refreshed under the
					// mutex by Init and ConfigUpdatedCallback, so read it
					// under the same lock instead of the unsynchronized
					// struct.
					dw.mu.RLock()
					poll := dw.configSnapshot.PollBlackholeDirectory
					interval := dw.configSnapshot.PollBlackholeIntervalMinutes
					pollDir := dw.configSnapshot.BlackholeDirectory
					dw.mu.RUnlock()
					if !poll {
						log.Info("Directory poller stopped")
						break
					}
					time.Sleep(time.Duration(interval) * time.Minute)
					log.Infof("Running directory scan of %s", pollDir)
					dw.directoryScan(pollDir)
					log.Infof("Scan complete, next scan in %d minutes", interval)
				}
			}()
		}
	} else {
		log.Info("Starting directory watcher...")
		dw.watchDirectory = directory_watcher.NewDirectoryWatcher(dir,
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

		if processed > 0 {
			time.Sleep(2 * time.Second)
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
