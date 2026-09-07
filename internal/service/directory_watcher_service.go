package service

import (
	"errors"
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
	downloadsFolderID  string
	watchDirectory     *directory_watcher.WatchDirectory
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
		go func(file os.FileInfo) {
			file_path := path.Join(p, file.Name())
			if dw.checkFile(file_path) == 1 {
				dw.addFileToQueue(file_path)
			}
		}(file)
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
		if dw.Queue.Len() < 1 {
			log.Trace("No files in Queue, sleeping for 10 seconds")
			time.Sleep(time.Second * time.Duration(10))
		}

		isQueueFile, filePath := dw.Queue.PopTopOfQueue()
		if !isQueueFile {
			time.Sleep(time.Second * time.Duration(10))
			continue
		}

		if filePath == "" {
			continue
		}
		time.Sleep(dw.processUpload(filePath))
	}
}

// processUpload handles one queue entry and returns the delay before the next.
func (dw *DirectoryWatcherService) processUpload(filePath string) time.Duration {
	log.Debugf("Processing %s", filePath)
	dw.mu.RLock()
	folderID := dw.downloadsFolderID
	dw.mu.RUnlock()
	err := dw.premiumizemeClient.CreateTransfer(filePath, folderID)
	if err != nil && !errors.Is(err, premiumizeme.ErrTransferAlreadyExists) {
		// A removed file cannot be retried. Every other failure stays queued,
		// including unfamiliar vendor messages and transient transport failures.
		if errors.Is(err, os.ErrNotExist) {
			return 2 * time.Second
		}
		dw.Queue.AddIfAbsent(filePath)
		dw.mu.Lock()
		if errors.Is(err, premiumizeme.ErrTransferLimitReached) {
			dw.status = ERROR_LIMIT_REACHED
		} else {
			dw.status = "Transfer failed; retrying"
		}
		dw.mu.Unlock()
		log.Warnf("Could not create transfer; retaining source and retrying in 10 seconds: %s", err)
		return 10 * time.Second
	}
	// Success and already-existing jobs are both terminal.
	if removeErr := os.Remove(filePath); removeErr != nil {
		log.Errorf("Could not delete %s: %s", filePath, removeErr)
	}
	dw.mu.Lock()
	dw.status = "Okay"
	dw.mu.Unlock()
	log.Infof("Removed %s from blackhole queue. Queue size: %d", filePath, dw.Queue.Len())
	return 2 * time.Second
}

func (dw *DirectoryWatcherService) setTransferDirectory(newDir string) {
	newID := utils.GetDownloadsFolderIDFromPremiumizeme(dw.premiumizemeClient, newDir)

	dw.mu.Lock()
	defer dw.mu.Unlock()

	dw.downloadsFolderID = newID
}
