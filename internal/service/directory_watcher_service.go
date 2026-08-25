package service

import (
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"reflect"
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
	arrFolders         map[string]string // Arr slug -> pme subfolder ID
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
	blackholeChanged := currentConfig.BlackholeDirectory != newConfig.BlackholeDirectory
	transferChanged := currentConfig.TransferDirectory != newConfig.TransferDirectory
	arrsChanged := !reflect.DeepEqual(currentConfig.Arrs, newConfig.Arrs)
	toggleChanged := currentConfig.EnableArrSubfolders != newConfig.EnableArrSubfolders

	if blackholeChanged {
		log.Info("Blackhole directory changed, restarting directory watcher...")
		log.Info("Running initial directory scan...")
		go dw.directoryScan(dw.config.BlackholeDirectory)
		dw.watchDirectory.UpdatePath(newConfig.BlackholeDirectory)

		if dw.watchDirectory != nil {
			for slug := range dw.arrFolders {
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

	if blackholeChanged || transferChanged || arrsChanged || toggleChanged {
		dw.resolveArrFolders()
	}
}

func (dw *DirectoryWatcherService) GetStatus() string {
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
				dw.scanArrFolders()
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

	dw.resolveArrFolders()
}

// clearArrFolders stops watching all known Arr subfolders and forgets their
// pme IDs, without deleting anything on disk or on pme.
func (dw *DirectoryWatcherService) clearArrFolders() {
	dw.mu.Lock()
	oldFolders := dw.arrFolders
	dw.arrFolders = nil
	dw.mu.Unlock()

	if dw.watchDirectory != nil {
		for slug := range oldFolders {
			dw.watchDirectory.RemoveWatchPath(filepath.Join(dw.config.BlackholeDirectory, slug))
		}
	}
}

// resolveArrFolders ensures each configured Arr has a local blackhole subfolder
// and a matching pme subfolder, and watches/scans it for existing files.
func (dw *DirectoryWatcherService) resolveArrFolders() {
	if !dw.config.EnableArrSubfolders {
		dw.clearArrFolders()
		return
	}

	newFolders := make(map[string]string, len(dw.config.Arrs))

	for _, arr := range dw.config.Arrs {
		id, err := utils.GetOrCreateSubfolderID(dw.premiumizemeClient, dw.downloadsFolderID, arr.Name)
		if err != nil {
			log.Errorf("Cannot resolve premiumize.me subfolder for Arr %s: %s", arr.Name, err)
			continue
		}
		newFolders[arr.Name] = id

		local := filepath.Join(dw.config.BlackholeDirectory, arr.Name)
		if err := os.MkdirAll(local, os.ModePerm); err != nil {
			log.Errorf("Cannot create blackhole subfolder for Arr %s: %s", arr.Name, err)
			continue
		}

		if dw.watchDirectory != nil {
			if err := dw.watchDirectory.AddWatchPath(local); err != nil {
				log.Errorf("Cannot watch blackhole subfolder %s: %s", local, err)
			}
		}
		dw.directoryScan(local)
	}

	dw.mu.Lock()
	oldFolders := dw.arrFolders
	dw.arrFolders = newFolders
	dw.mu.Unlock()

	configuredSlugs := make(map[string]bool, len(dw.config.Arrs))
	for _, arr := range dw.config.Arrs {
		configuredSlugs[arr.Name] = true
	}

	for slug := range oldFolders {
		if configuredSlugs[slug] {
			continue
		}
		if dw.watchDirectory != nil {
			dw.watchDirectory.RemoveWatchPath(filepath.Join(dw.config.BlackholeDirectory, slug))
		}
		log.Infof("Arr %s no longer configured, local/premiumize subfolder is kept but no longer watched", slug)
	}
}

func (dw *DirectoryWatcherService) scanArrFolders() {
	if !dw.config.EnableArrSubfolders {
		return
	}

	for _, arr := range dw.config.Arrs {
		local := filepath.Join(dw.config.BlackholeDirectory, arr.Name)
		if _, err := os.Stat(local); err != nil {
			continue
		}
		dw.directoryScan(local)
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
		if dw.isConfiguredArrSlug(filepath.Base(path)) {
			log.Tracef("Directory %s is a configured Arr subfolder, handled separately", path)
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
	dw.Queue.Add(path)
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

		sleepTimeSeconds := 2
		if filePath != "" {
			log.Debugf("Processing %s", filePath)
			dw.mu.RLock()
			folderID, ok, slug := resolveTargetFolderID(filePath, dw.config.BlackholeDirectory, dw.downloadsFolderID, dw.arrFolders)
			dw.mu.RUnlock()
			if !ok && slug != "" {
				folderID, ok = dw.resolveSingleArrFolder(slug)
			}
			if !ok {
				log.Errorf("No resolved target folder for %s, skipping upload", filePath)
				time.Sleep(time.Second * time.Duration(sleepTimeSeconds))
				continue
			}
			err := dw.premiumizemeClient.CreateTransfer(filePath, folderID)
			if err != nil {
				switch err.Error() {
				case ERROR_LIMIT_REACHED:
					dw.status = "Limit of transfers reached!"
					log.Trace("Transfer limit reached waiting 10 seconds and retrying")
					sleepTimeSeconds = 10
				case ERROR_ALREADY_UPLOADED:
					log.Trace("File already uploaded, removing from Disk")
					os.Remove(filePath)
				default:
					log.Error("Error creating transfer: %s", err)
				}
			} else {
				dw.status = "Okay"
				os.Remove(filePath)
				if err != nil {
					log.Errorf("Error could not delete %s Error: %+v", filePath, err)
				}
				log.Infof("Removed %s from blackhole Queue. Queue Size: %d", filePath, dw.Queue.Len())
			}
			time.Sleep(time.Second * time.Duration(sleepTimeSeconds))
		} else {
			log.Errorf("Received %s from blackhole Queue. Appears to be an empty path.")
		}
	}
}

func (dw *DirectoryWatcherService) resolveSingleArrFolder(slug string) (string, bool) {
	if !dw.config.EnableArrSubfolders {
		return "", false
	}

	id, err := utils.GetOrCreateSubfolderID(dw.premiumizemeClient, dw.downloadsFolderID, slug)
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
	if filepath.Dir(filePath) == filepath.Clean(blackholeDir) {
		return mainFolderID, true, ""
	}

	slug = filepath.Base(filepath.Dir(filePath))
	id, found := arrFolders[slug]
	return id, found, slug
}

func (dw *DirectoryWatcherService) setTransferDirectory(newDir string) {
	newID := utils.GetDownloadsFolderIDFromPremiumizeme(dw.premiumizemeClient, newDir)

	dw.mu.Lock()
	defer dw.mu.Unlock()

	dw.downloadsFolderID = newID
}
