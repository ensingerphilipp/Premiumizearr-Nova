package service

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sync"
	"time"

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
}

type TransferManagerService struct {
	premiumizemeClient *premiumizeme.Premiumizeme
	arrsManager        *ArrsManagerService
	config             *config.Config
	lastUpdated        int64
	transfers          []premiumizeme.Transfer
	runningTask        bool
	downloadListMutex  *sync.Mutex
	downloadList       map[string]*DownloadDetails
	status             string
	downloadsFolderID  string
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
}

func (manager *TransferManagerService) Run(interval time.Duration) {
	manager.downloadsFolderID = utils.GetDownloadsFolderIDFromPremiumizeme(manager.premiumizemeClient, manager.config.TransferDirectory)
	for {
		manager.runningTask = true
		manager.TaskUpdateTransfersList()
		manager.TaskCheckPremiumizeDownloadsFolder()
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
	for _, transfer := range transfers {
		found := false
		for _, arr := range manager.arrsManager.GetArrs() {
			if found {
				break
			}
			if transfer.Status == "error" {
				log.Tracef("Checking errored transfer %s against %s history", transfer.Name, arr.GetArrName())
				arrID, contains := arr.HistoryContains(transfer.Name)
				if !contains {
					log.Tracef("%s history doesn't contain %s", arr.GetArrName(), transfer.Name)
					continue
				}
				log.Tracef("Found %s in %s history", transfer.Name, arr.GetArrName())
				found = true
				log.Debugf("Processing transfer that has errored: %s", transfer.Name)
				go arr.HandleErrorTransfer(&transfer, arrID, manager.premiumizemeClient)

			}
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

func (manager *TransferManagerService) addDownload(item *premiumizeme.Item) {
	manager.downloadListMutex.Lock()
	defer manager.downloadListMutex.Unlock()

	manager.downloadList[item.Name] = &DownloadDetails{
		Added:              time.Now(),
		Name:               item.Name,
		ProgressDownloader: progress_downloader.NewWriteCounter(),
	}
}

func (manager *TransferManagerService) countDownloads() int {
	manager.downloadListMutex.Lock()
	defer manager.downloadListMutex.Unlock()
	// Calculate len(manager.downloadList) / 2 as every download also has a Parent Folder in manager.downloadList
	return (len(manager.downloadList) / 2)
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

	manager.addDownload(&item)
	go func() {
		defer manager.removeDownload(item.Name)
		err := manager.downloadFolderRecursively(item, downloadDirectory)
		if err != nil {
			log.Errorf("Error downloading item %s: %s", item.Name, err)
			manager.removeDownload(item.Name)
			return
		}

		err = manager.premiumizemeClient.DeleteFolder(item.ID)
		if err != nil {
			manager.removeDownload(item.Name)
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

	for _, item := range items {
		if manager.downloadExists(item.Name) {
			log.Tracef("Transfer %s is already downloading", item.Name)
			return nil
		}
		if item.Type == "file" {
			manager.addDownload(&item)
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
				return fmt.Errorf("error downloading file %s: %w", item.Name, err)
			}
			manager.removeDownload(item.Name)
		} else if item.Type == "folder" {
			err = manager.downloadFolderRecursively(item, savePath)
			if err != nil {
				return fmt.Errorf("error downloading folder %s: %w", item.Name, err)
			}
		}
	}
	return nil
}
