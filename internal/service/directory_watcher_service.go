package service

import (
	"io/ioutil"
	"os"
	"path"
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
	mu sync.RWMutex
	// resolveMu serializes the bulk resolveArrFolders runs (startup and
	// config callback): each run does network/FS work and commits the
	// (arrFolders map, subfolder watch) pair at the end, and interleaved
	// runs can commit a map the other run's watch operations do not match.
	resolveMu          sync.Mutex
	premiumizemeClient *premiumizeme.Premiumizeme
	config             *config.Config
	Queue              *stringqueue.StringQueue
	status             string
	quotaBlocked       bool
	quotaCheckFailed   bool
	downloadsFolderID  string
	watchDirectory     *directory_watcher.WatchDirectory
	arrFolders         map[string]string // Arr slug -> pme subfolder ID
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

func (dw *DirectoryWatcherService) Init(premiumizemeClient *premiumizeme.Premiumizeme, cfg *config.Config) {
	dw.premiumizemeClient = premiumizemeClient
	dw.config = cfg
	// Register this service's lock as the process-wide config swap lock:
	// UpdateConfig takes it around the in-place struct replacement, and the
	// services' goroutines read config fields under the matching read lock.
	config.SetUpdateMu(&dw.mu)
}

func (dw *DirectoryWatcherService) ConfigUpdatedCallback(currentConfig config.Config, newConfig config.Config) {
	blackholeChanged := currentConfig.BlackholeDirectory != newConfig.BlackholeDirectory
	transferChanged := currentConfig.TransferDirectory != newConfig.TransferDirectory
	arrsChanged := !reflect.DeepEqual(currentConfig.Arrs, newConfig.Arrs)
	toggleChanged := currentConfig.EnableArrSubfolders != newConfig.EnableArrSubfolders

	if blackholeChanged {
		log.Info("Blackhole directory changed, restarting directory watcher...")
		// Apply the LIVE committed blackhole directory, not this
		// callback's argument snapshot: the reconfiguration worker serializes
		// callbacks, but a newer save may have committed while this one was
		// still queued. Re-applying the argument value would move the watcher
		// and the subfolder watches below back to the stale location, on top
		// of the newer commit (finding S-28).
		dw.mu.RLock()
		blackholeDir := dw.config.BlackholeDirectory
		dw.mu.RUnlock()
		log.Info("Running initial directory scan...")
		go dw.directoryScan(blackholeDir)

		// The watcher handle is read and written under dw.mu: this
		// goroutine and the Start() goroutine may both replace it.
		dw.mu.RLock()
		wd := dw.watchDirectory
		dw.mu.RUnlock()
		created := false
		if wd == nil {
			// The startup watcher never came up (Watch failed at Start):
			// UpdatePath needs a live watcher, so try a fresh Watch on the
			// new path instead of leaving watch mode dead for the process
			// lifetime.
			log.Info("Watcher unavailable, (re)starting the directory watcher...")
			wd = directory_watcher.NewDirectoryWatcher(blackholeDir,
				true,
				dw.checkFile,
				dw.addFileToQueue,
			)
			if err := wd.Watch(); err != nil {
				// A failed Watch already created the fsnotify instance and
				// its event-loop goroutine: stop it so the partial watcher
				// does not leak. The nil handle stays in place so a later
				// config change re-attempts Watch (failed-startup recovery).
				wd.Stop()
				log.Errorf("Error (re)starting directory watcher: %s", err)
			} else {
				dw.mu.Lock()
				dw.watchDirectory = wd
				dw.mu.Unlock()
				created = true
			}
		}
		if wd != nil {
			// A freshly created watcher already watches the live directory;
			// only move an existing one, and move it to the live directory.
			if !created {
				wd.UpdatePath(blackholeDir)
			}

			// The old subfolder watches live at the PRE-update blackhole
			// location (the argument snapshot); they must be removed from
			// there, while the re-watches added by resolveArrFolders below
			// use the live one.
			dw.mu.RLock()
			slugs := make([]string, 0, len(dw.arrFolders))
			for slug := range dw.arrFolders {
				slugs = append(slugs, slug)
			}
			dw.mu.RUnlock()
			for _, slug := range slugs {
				wd.RemoveWatchPath(filepath.Join(currentConfig.BlackholeDirectory, slug))
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
	go dw.directoryScan(blackholeDir)

	// The watcher handle is read and written under dw.mu: this goroutine
	// and the web callback goroutine may both replace it.
	dw.mu.RLock()
	oldWatcher := dw.watchDirectory
	dw.mu.RUnlock()
	if oldWatcher != nil {
		log.Info("Stopping directory watcher...")
		err := oldWatcher.Stop()
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
				dw.mu.RLock()
				directory := dw.config.BlackholeDirectory
				dw.mu.RUnlock()
				log.Infof("Running directory scan of %s", directory)
				// The root scan alone covers the Arr subfolders: checkFile
				// fans a scan out into each configured subfolder it meets,
				// so a separate per-subfolder scan pass here would just
				// re-scan the same directories (finding S-2).
				dw.directoryScan(directory)
				log.Infof("Scan complete, next scan in %d minutes", interval)
			}
		}()
	} else {
		log.Info("Starting directory watcher...")
		// Re-read the live blackhole directory right before creating the
		// watcher: the snapshot above was taken before the downloads-folder
		// network round trip, and a config change in that window must not
		// leave the watcher on the old path (the callback saw no watcher to
		// update yet, and this assignment would overwrite its work).
		dw.mu.RLock()
		liveBlackholeDir := dw.config.BlackholeDirectory
		dw.mu.RUnlock()
		if liveBlackholeDir != blackholeDir {
			log.Infof("Blackhole directory changed to %s during startup, watching the new value", liveBlackholeDir)
			blackholeDir = liveBlackholeDir
		}
		wd := directory_watcher.NewDirectoryWatcher(blackholeDir,
			true,
			dw.checkFile,
			dw.addFileToQueue,
		)
		if err := wd.Watch(); err != nil {
			// A failed Watch already created the fsnotify instance and its
			// event-loop goroutine: stop it. A watcher that cannot be
			// created degrades to the one-time startup scan instead of
			// crashing the daemon: nil the handle so the
			// AddWatchPath/RemoveWatchPath/UpdatePath call sites stay
			// no-ops until a later config change re-attempts Watch.
			wd.Stop()
			log.Errorf("Error starting directory watcher: %s", err)
			dw.mu.Lock()
			dw.watchDirectory = nil
			dw.mu.Unlock()
		} else {
			dw.mu.Lock()
			previous := dw.watchDirectory
			dw.watchDirectory = wd
			dw.mu.Unlock()
			if previous != nil && previous != oldWatcher {
				previous.Stop()
			}
			// Re-read the live blackhole directory after the assignment: the
			// re-read above ran before Watch(), so a save committed in the
			// window between it and the assignment applies the newest value
			// instead of the stale one the watcher was created with
			// (finding S-28).
			dw.mu.RLock()
			liveBlackholeDir := dw.config.BlackholeDirectory
			dw.mu.RUnlock()
			if liveBlackholeDir != blackholeDir {
				log.Infof("Blackhole directory changed to %s while starting the watcher, updating the watch", liveBlackholeDir)
				wd.UpdatePath(liveBlackholeDir)
			}
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
	wd := dw.watchDirectory
	dw.mu.Unlock()

	if wd != nil {
		for slug := range oldFolders {
			wd.RemoveWatchPath(filepath.Join(blackholeDir, slug))
		}
	}
}

// resolveArrFolders ensures each configured Arr has a local blackhole subfolder
// and a matching pme subfolder, and watches/scans it for existing files.
func (dw *DirectoryWatcherService) resolveArrFolders() {
	// Serialize bulk resolve runs: each run does network/FS work and
	// commits the (arrFolders map, subfolder watch) pair at the end.
	// Interleaved runs (startup and config callback) can commit a map the
	// other run's watch operations do not match - a subfolder watched but
	// missing from the map, or present but unwatched - so a file dropped
	// into it is never queued.
	dw.resolveMu.Lock()
	defer dw.resolveMu.Unlock()

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

	// The startup path and the config API reject an empty blackhole
	// directory (findings S-19/S-29); if one still ends up here, the local
	// subfolders would resolve to relative paths, so keep the current map
	// and skip instead of acting on it.
	if blackholeDir == "" {
		log.Warn("Blackhole directory is not configured, skipping per-Arr subfolder resolution")
		return
	}

	if mainFolderID == "" {
		log.Warn("Transfers folder not resolved yet, skipping pme subfolder resolution for this round")
	}

	newFolders := make(map[string]string, len(arrs))

	for _, arr := range arrs {
		local := filepath.Join(blackholeDir, arr.Name)
		var id string
		var err error
		// No pme round trip without a resolved main folder: an empty
		// parent ID would list/create at the account root instead of the
		// transfers directory. Keep the previous mapping (or an empty-ID
		// placeholder) and retry on the next resolve.
		if mainFolderID != "" {
			id, err = utils.GetOrCreateSubfolderID(dw.premiumizemeClient, mainFolderID, arr.Name)
		}
		if mainFolderID == "" || err != nil {
			if mainFolderID != "" {
				log.Errorf("Cannot resolve premiumize.me subfolder for Arr %s: %s", arr.Name, err)
			}
			// A transient premiumize.me failure must not discard a
			// previously resolved subfolder ID: carry the mapping forward so
			// queued uploads keep their target folder. Without a previous
			// ID, track the slug with an empty ID so the watch added below
			// is removed by the same map-keyed loop as every other watch
			// (blackhole change, feature-off toggle, slug removal) instead
			// of leaking.
			if prev, ok := oldFolders[arr.Name]; ok {
				newFolders[arr.Name] = prev
			} else {
				newFolders[arr.Name] = ""
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
			// The slug stays tracked with an empty ID: a watch registered
			// for it in a previous run must be removable via the same
			// map-keyed loop as every other watch.
			newFolders[arr.Name] = ""
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

	// Un-watch removed slugs using the watcher handle snapshotted under the
	// same lock as the map commit, so the (map, watch) pair is always
	// observed consistently.
	dw.mu.RLock()
	wd := dw.watchDirectory
	dw.mu.RUnlock()
	for slug := range oldFolders {
		if configuredSlugs[slug] {
			continue
		}
		if wd != nil {
			wd.RemoveWatchPath(filepath.Join(blackholeDir, slug))
		}
		log.Infof("Arr %s no longer configured, local/premiumize subfolder is kept but no longer watched", slug)
	}
}

// watchArrFolder registers the watch on a local Arr subfolder when the
// watcher is available; a failed registration is logged, not fatal. The
// handle is read under dw.mu: it is replaced by the Start() goroutine and
// the config callback.
func (dw *DirectoryWatcherService) watchArrFolder(local string) {
	dw.mu.RLock()
	wd := dw.watchDirectory
	dw.mu.RUnlock()
	if wd == nil {
		return
	}
	if err := wd.AddWatchPath(local); err != nil {
		log.Errorf("Cannot watch blackhole subfolder %s: %s", local, err)
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
	if !dw.Queue.AddIfAbsent(path) {
		return
	}
	log.Infof("File created in blackhole %s added to Queue. Queue length %d", path, dw.Queue.Len())
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
	// Without a resolved main folder the list/create round trip would hit
	// the account root instead of the transfers directory: refuse before
	// any pme traffic; the caller re-queues the file and retries.
	if mainFolderID == "" {
		return "", false
	}

	id, err := utils.GetOrCreateSubfolderID(dw.premiumizemeClient, mainFolderID, slug)
	if err != nil {
		log.Errorf("Cannot resolve premiumize.me subfolder for Arr %s: %s", slug, err)
		return "", false
	}

	dw.mu.Lock()
	// Re-validate under the write lock: the entry checks above ran under a
	// separate read lock, and the pme round trip in between can interleave
	// with a config swap (feature toggled off, slug removed). Without the
	// re-check, a stale write resurrects an entry a concurrent resolve just
	// dropped, and its watch leaks.
	if !dw.config.EnableArrSubfolders {
		dw.mu.Unlock()
		return "", false
	}
	slugConfigured := false
	for _, arr := range dw.config.Arrs {
		if arr.Name == slug {
			slugConfigured = true
			break
		}
	}
	if !slugConfigured {
		dw.mu.Unlock()
		return "", false
	}
	if dw.arrFolders == nil {
		dw.arrFolders = map[string]string{}
	}
	dw.arrFolders[slug] = id
	dw.mu.Unlock()

	return id, true
}

// configuredBlackholeSlug returns the Arr slug for a file that lies
// DIRECTLY inside a currently configured Arr subfolder of the given
// blackhole directory, or "" when no such slug applies: the file is in the
// blackhole root itself, in a subfolder that is not a configured Arr slug
// (feature off, arbitrary name, or a leftover of a previous blackhole
// location), or nested deeper than one level below the root. Upload routing
// and the web queue view share it so they never disagree about which file
// belongs to which Arr (finding S-23).
func configuredBlackholeSlug(filePath, blackholeDir string, enabled bool, arrs []config.ArrConfig) string {
	if !enabled {
		return ""
	}
	dir := filepath.Clean(filepath.Dir(filePath))
	cleanBlackhole := filepath.Clean(blackholeDir)
	if dir == cleanBlackhole {
		return ""
	}
	// The parent directory must be directly inside the current blackhole.
	// A blackhole set to the working directory has direct children that are
	// bare directory names (no separator).
	inside := strings.HasPrefix(dir, cleanBlackhole+string(filepath.Separator))
	if cleanBlackhole == "." {
		inside = !strings.ContainsRune(dir, filepath.Separator)
	}
	if !inside {
		return ""
	}
	// Exactly one level below the root: a nested directory is not an Arr
	// subfolder even when its own name matches a configured slug.
	if filepath.Dir(dir) != cleanBlackhole {
		return ""
	}
	slug := filepath.Base(dir)
	for _, arr := range arrs {
		if arr.Name == slug {
			return slug
		}
	}
	return ""
}

func resolveTargetFolderID(filePath, blackholeDir, mainFolderID string, arrFolders map[string]string, enabled bool, arrs []config.ArrConfig) (folderID string, ok bool, slug string) {
	slug = configuredBlackholeSlug(filePath, blackholeDir, enabled, arrs)
	if slug == "" {
		// A file in the blackhole root, in a subfolder that is not a
		// configured Arr slug, or outside the current blackhole keeps the
		// pre-feature destination: an empty folder ID would submit the file
		// to the account root, and a slug here would make processUpload
		// create a pme folder named after a directory the user never
		// defined.
		return mainFolderID, mainFolderID != "", ""
	}
	id, found := arrFolders[slug]
	// An empty ID (an unresolved slug) is not a routable target: the
	// caller resolves it on demand and retries.
	return id, found && id != "", slug
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
	// All three fields are read under the same read lock as the swap:
	// the slug classification must come from one consistent config state.
	folderID, ok, slug := resolveTargetFolderID(filePath, dw.config.BlackholeDirectory, dw.downloadsFolderID, dw.arrFolders, dw.config.EnableArrSubfolders, dw.config.Arrs)
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
		// No resolved target yet (pme outage, or the transfers folder was
		// never resolved): the file stays in the blackhole and is retried
		// on a later cycle instead of being dropped, so a transient
		// failure never loses an upload.
		log.Warnf("No resolved target folder for %s yet, will retry", filePath)
		return true
	}

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
