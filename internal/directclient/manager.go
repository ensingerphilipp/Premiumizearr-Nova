package directclient

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ensingerphilipp/premiumizearr-nova/internal/config"
	"github.com/ensingerphilipp/premiumizearr-nova/internal/utils"
	"github.com/ensingerphilipp/premiumizearr-nova/pkg/premiumizeme"
	log "github.com/sirupsen/logrus"
)

// Job is the durable link between one *arr download ID, its Premiumize
// transfer, and the local directory from which *arr imports the result.
type Job struct {
	ID              string    `json:"id"`
	Kind            string    `json:"kind"`
	Name            string    `json:"name"`
	Category        string    `json:"category"`
	SourceName      string    `json:"source_name"`
	Phase           string    `json:"phase"`
	Progress        float64   `json:"progress"`
	TotalBytes      int64     `json:"total_bytes,omitempty"`
	Downloaded      int64     `json:"downloaded,omitempty"`
	TransferID      string    `json:"transfer_id"`
	CloudFolder     string    `json:"cloud_folder"`
	OutputPath      string    `json:"output_path"`
	Error           string    `json:"error"`
	DeleteRequested bool      `json:"delete_requested,omitempty"`
	DeleteFiles     bool      `json:"delete_files,omitempty"`
	Created         time.Time `json:"created"`
}

// Manager owns the direct-download namespace. The existing blackhole path
// uses a different Premiumize folder and remains independent.
type Manager struct {
	mu         sync.RWMutex
	pollMu     sync.Mutex
	pm         *premiumizeme.Premiumizeme
	config     *config.Config
	stateDir   string
	jobs       map[string]*Job
	categories map[string]bool
	active     map[string]context.CancelFunc
	rootMu     sync.Mutex // guards rootDir/rootID: submit may race the poll loop
	rootDir    string
	rootID     string
	stop       chan struct{}
}

func NewManager(pm *premiumizeme.Premiumizeme, cfg *config.Config, configDir string) (*Manager, error) {
	if configDir == "" {
		configDir = "."
	}
	stateDir := filepath.Join(configDir, "direct-jobs")
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return nil, err
	}
	m := &Manager{pm: pm, config: cfg, stateDir: stateDir, jobs: make(map[string]*Job), categories: make(map[string]bool), active: make(map[string]context.CancelFunc), stop: make(chan struct{})}
	data, err := os.ReadFile(filepath.Join(stateDir, "jobs.json"))
	if err == nil {
		var stored []Job
		if err := json.Unmarshal(data, &stored); err != nil {
			return nil, fmt.Errorf("invalid direct job registry: %w", err)
		}
		for i := range stored {
			j := stored[i]
			if j.ID == "" || !safeID(j.ID) || m.jobs[j.ID] != nil {
				return nil, errors.New("invalid or duplicated direct job ID")
			}
			if j.Phase == "submitting" {
				// An interrupted HTTP request might have reached Premiumize.
				// Never issue it a second time without a transfer ID.
				j.Phase = "failed"
				j.Error = "Submission outcome unknown after restart; inspect the Premiumize transfer before retrying"
			}
			m.jobs[j.ID] = &j
			if j.Category != "" {
				m.categories[j.Category] = true
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return m, nil
}

func safeID(id string) bool {
	if len(id) == 0 || len(id) > 80 {
		return false
	}
	for _, ch := range id {
		if (ch < 'a' || ch > 'z') && (ch < '0' || ch > '9') && ch != '-' && ch != '_' {
			return false
		}
	}
	return true
}

// safePathComponent reports whether s is usable as a single path
// component: non-empty, no path separator and no parent-directory
// reference.
func safePathComponent(s string) bool {
	if s == "" {
		return false
	}
	return !strings.Contains(s, "/") && !strings.Contains(s, "\\") && !strings.Contains(s, "..")
}

// jobSourcePath returns the on-disk path of a job's retained source
// payload. The job ID must be a safe single path component and the joined
// path must stay inside the state directory; otherwise the file operation
// could reach outside the direct namespace.
func (m *Manager) jobSourcePath(id string) (string, error) {
	if !safePathComponent(id) {
		return "", fmt.Errorf("invalid direct job ID %q", id)
	}
	path := filepath.Join(m.stateDir, id+".source")
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	base, err := filepath.Abs(m.stateDir)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(abs, base) {
		return "", fmt.Errorf("direct job source path escapes the state directory")
	}
	return path, nil
}

// saveLocked writes the entire registry atomically. Call with m.mu held.
func (m *Manager) saveLocked() error {
	jobs := make([]Job, 0, len(m.jobs))
	for _, job := range m.jobs {
		jobs = append(jobs, *job)
	}
	data, err := json.Marshal(jobs)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(m.stateDir, ".jobs-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(m.stateDir, "jobs.json"))
}

func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func (m *Manager) add(kind string, data []byte, filename, category string) (Job, error) {
	if len(data) == 0 || len(data) > 64<<20 {
		return Job{}, errors.New("invalid or oversized download request")
	}
	if len(category) > 128 {
		return Job{}, errors.New("category too long")
	}
	if m.config.TransferOnlyMode {
		return Job{}, errors.New("direct download requires TransferOnlyMode to be disabled")
	}
	var id string
	var err error
	switch kind {
	case "magnet":
		id, err = magnetHash(string(data))
	case "torrent":
		id, err = torrentHash(data)
	case "nzb":
		id, err = newID()
	default:
		return Job{}, errors.New("unsupported download type")
	}
	if err != nil {
		return Job{}, err
	}
	// The job ID becomes a file-name component of the on-disk source file
	// and the published directory; it is derived from request data, so it
	// must be a safe single path component before any path is built from it.
	if !safeID(id) || !safePathComponent(id) {
		return Job{}, errors.New("invalid or unsafe direct job ID")
	}
	name := filepath.Base(filename)
	if name == "." || name == string(filepath.Separator) || name == "" {
		name = id
	}
	if kind == "torrent" || kind == "nzb" {
		name = strings.TrimSuffix(name, filepath.Ext(name))
	}
	if kind == "magnet" {
		if u, parseErr := parseMagnetName(string(data)); parseErr == nil && u != "" {
			name = u
		}
	}
	// A torrent hash is the ID Sonarr and Radarr themselves expect. One
	// torrent cannot simultaneously occupy two qBittorrent categories.
	m.mu.Lock()
	defer m.mu.Unlock()
	if old := m.jobs[id]; old != nil {
		if old.Kind != "nzb" && old.Category == category {
			return *old, nil
		}
		return Job{}, errors.New("torrent is already assigned to a different category")
	}
	outputRoot := filepath.Join(m.outputRoot(), "direct")
	job := &Job{ID: id, Kind: kind, Name: name, Category: category, SourceName: filename, Phase: "queued", OutputPath: filepath.Join(outputRoot, id), Created: time.Now()}
	sourcePath, err := m.jobSourcePath(id)
	if err != nil {
		return Job{}, err
	}
	if err := os.WriteFile(sourcePath, data, 0600); err != nil {
		return Job{}, err
	}
	m.jobs[id] = job
	if category != "" {
		m.categories[category] = true
	}
	if err := m.saveLocked(); err != nil {
		delete(m.jobs, id)
		if sp, perr := m.jobSourcePath(id); perr == nil {
			os.Remove(sp)
		}
		return Job{}, err
	}
	return *job, nil
}

func parseMagnetName(magnet string) (string, error) {
	// Keep the visible release name separate from the opaque BTIH ID.
	return magnetDisplayName(magnet)
}

func (m *Manager) AddTorrent(_ context.Context, data []byte, filename, category string) error {
	_, err := m.add("torrent", data, filename, category)
	return err
}

func (m *Manager) AddMagnet(_ context.Context, magnet, category string) error {
	_, err := m.add("magnet", []byte(magnet), "", category)
	return err
}

func (m *Manager) AddNZB(_ context.Context, data []byte, filename, category string) (SABJobView, error) {
	job, err := m.add("nzb", data, filename, category)
	if err != nil {
		return SABJobView{}, err
	}
	return sabView(job), nil
}

func (m *Manager) outputRoot() string {
	if m.config.DownloadsDirectory != "" {
		return m.config.DownloadsDirectory
	}
	return filepath.Join(os.TempDir(), "premiumizearrd")
}

func (m *Manager) QBitOutputRoot() string { return filepath.Join(m.outputRoot(), "direct") }
func (m *Manager) SABOutputRoot() string  { return filepath.Join(m.outputRoot(), "direct") }

func (m *Manager) ListTorrents(category string) []TorrentView {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]TorrentView, 0, len(m.jobs))
	for _, j := range m.jobs {
		if j.Kind == "nzb" || category != "" && category != j.Category {
			continue
		}
		state := j.Phase
		if state == "submitting" || state == "cloud" {
			state = "downloading"
		}
		if state == "local" {
			state = "downloading"
		}
		savePath := filepath.Dir(j.OutputPath)
		remaining := j.TotalBytes - j.Downloaded
		if remaining < 0 {
			remaining = 0
		}
		out = append(out, TorrentView{Hash: j.ID, Name: j.Name, Category: j.Category, State: state, Progress: j.Progress, Size: j.TotalBytes, AmountLeft: remaining, ContentPath: j.OutputPath, SavePath: savePath, Error: j.Error})
	}
	return out
}

func sabView(j Job) SABJobView {
	state := j.Phase
	if state == "submitting" || state == "cloud" || state == "local" {
		state = "downloading"
	}
	remaining := j.TotalBytes - j.Downloaded
	if remaining < 0 {
		remaining = 0
	}
	return SABJobView{ID: j.ID, Name: j.Name, Category: j.Category, State: state, Progress: j.Progress * 100, SizeBytes: j.TotalBytes, RemainingBytes: remaining, OutputPath: j.OutputPath, Error: j.Error}
}

func (m *Manager) ListNZB(category string) []SABJobView {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]SABJobView, 0)
	for _, j := range m.jobs {
		if j.Kind == "nzb" && (category == "" || category == j.Category) {
			out = append(out, sabView(*j))
		}
	}
	return out
}

func (m *Manager) SetCategory(hash, category string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	j := m.jobs[strings.ToLower(hash)]
	if j == nil || j.Kind == "nzb" {
		return os.ErrNotExist
	}
	j.Category = category
	if category != "" {
		m.categories[category] = true
	}
	return m.saveLocked()
}

func (m *Manager) CreateCategory(category string) error {
	if category == "" || len(category) > 128 {
		return errors.New("invalid category")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.categories[category] = true
	return nil
}

func (m *Manager) ListCategories() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	seen := map[string]bool{"sonarr": true, "radarr": true, "lidarr": true, "tv": true, "movies": true, "music": true}
	for _, a := range m.config.Arrs {
		if a.Name != "" {
			seen[a.Name] = true
		}
	}
	for cat := range m.categories {
		seen[cat] = true
	}
	out := make([]string, 0, len(seen))
	for cat := range seen {
		out = append(out, cat)
	}
	return out
}

func (m *Manager) SABCategories() []string { return m.ListCategories() }

func (m *Manager) OwnsTransfer(id string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, j := range m.jobs {
		if j.TransferID != "" && j.TransferID == id {
			return true
		}
	}
	return false
}

func (m *Manager) RemoveTorrent(hash string, deleteFiles bool) error {
	return m.remove(strings.ToLower(hash), deleteFiles)
}
func (m *Manager) RemoveNZB(id string, deleteFiles bool) error { return m.remove(id, deleteFiles) }

func (m *Manager) remove(id string, deleteFiles bool) error {
	m.mu.Lock()
	j := m.jobs[id]
	if j == nil {
		m.mu.Unlock()
		return nil // Download clients treat repeated removal as idempotent.
	}
	if cancel := m.active[id]; cancel != nil {
		j.DeleteRequested = true
		j.DeleteFiles = deleteFiles
		if err := m.saveLocked(); err != nil {
			m.mu.Unlock()
			return err
		}
		cancel()
		m.mu.Unlock()
		return nil // Cleanup runs when the downloader releases this job.
	}
	job := *j
	m.mu.Unlock()

	// The remote transfer and folder are deleted BEFORE the durable row:
	// a failure in between must not leave a Premiumize object that the
	// registry no longer knows about and nothing will ever retry. While the
	// cleanup is pending the row stays, marked failed with a retry flag the
	// next poll's deletion pass (or a repeated *arr removal) re-attempts.
	if job.TransferID != "" {
		if err := m.pm.DeleteTransfer(job.TransferID); err != nil {
			log.Warnf("Could not delete direct transfer %s: %v", job.TransferID, err)
			m.markCleanupPending(id, deleteFiles, "Remote cleanup pending: the Premiumize transfer was not deleted; the removal will be retried")
			return err
		}
		job.TransferID = ""
	}
	if job.CloudFolder != "" {
		if err := m.pm.DeleteFolder(job.CloudFolder); err != nil {
			log.Warnf("Could not delete direct cloud folder for %s: %v", id, err)
			m.markCleanupPending(id, deleteFiles, "Remote cleanup pending: the Premiumize folder was not deleted; the removal will be retried")
			return err
		}
		job.CloudFolder = ""
	}
	m.mu.Lock()
	if cur := m.jobs[id]; cur != nil {
		cur.TransferID, cur.CloudFolder = "", ""
		delete(m.jobs, id)
	}
	if err := m.saveLocked(); err != nil {
		m.mu.Unlock()
		return err
	}
	m.mu.Unlock()
	if deleteFiles {
		// The only permissible deletion target is the dedicated job directory.
		if filepath.Base(job.OutputPath) == id && filepath.Base(filepath.Dir(job.OutputPath)) == "direct" {
			if err := os.RemoveAll(job.OutputPath); err != nil {
				return err
			}
			// The staging sibling holds the in-flight bytes of an
			// interrupted download; it is removed with the published tree.
			if err := os.RemoveAll(job.OutputPath + ".partial"); err != nil {
				return err
			}
		}
	}
	sourcePath, err := m.jobSourcePath(id)
	if err != nil {
		return err
	}
	if err := os.Remove(sourcePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// markCleanupPending keeps the durable row (with a retry flag) after a
// failed remote cleanup. Call with m.mu NOT held.
func (m *Manager) markCleanupPending(id string, deleteFiles bool, message string) {
	m.mu.Lock()
	if cur := m.jobs[id]; cur != nil {
		cur.Phase, cur.Error = "failed", message
		cur.DeleteRequested = true
		cur.DeleteFiles = deleteFiles
		_ = m.saveLocked()
	}
	m.mu.Unlock()
}

func (m *Manager) Start() {
	go func() {
		m.PollOnce(context.Background())
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				m.PollOnce(context.Background())
			case <-m.stop:
				return
			}
		}
	}()
}

func (m *Manager) Stop() { close(m.stop) }

// PollOnce advances queued and cloud jobs. The method is public to allow
// deterministic tests without waiting for the service ticker.
func (m *Manager) PollOnce(ctx context.Context) {
	m.pollMu.Lock()
	defer m.pollMu.Unlock()
	m.mu.RLock()
	deleting := make([]Job, 0)
	for _, j := range m.jobs {
		if j.DeleteRequested {
			deleting = append(deleting, *j)
		}
	}
	m.mu.RUnlock()
	for _, j := range deleting {
		_ = m.remove(j.ID, j.DeleteFiles)
	}
	m.mu.RLock()
	queued := make([]Job, 0)
	for _, j := range m.jobs {
		if j.Phase == "queued" {
			queued = append(queued, *j)
		}
	}
	m.mu.RUnlock()
	if len(queued) > 0 {
		account, err := m.pm.GetAccountInfo()
		// The quota gate conditions the queued-submission loop only: an
		// exhausted account must not submit new jobs, but the GetTransfers
		// pass below must still run or already-submitted jobs in the cloud
		// phase would never reach their downloads while the quota stays
		// exhausted (which, on a small account, is the normal state).
		if !(err == nil && account.QuotaExhausted()) {
			for _, j := range queued {
				if err := m.submit(ctx, j.ID); err != nil {
					log.Warnf("Direct transfer %s could not be submitted: %v", j.ID, err)
				}
			}
		}
	}
	transfers, err := m.pm.GetTransfers()
	if err != nil {
		log.Warnf("Could not refresh direct transfer status: %v", err)
		return
	}
	for _, transfer := range transfers {
		m.mu.Lock()
		var id string
		for _, j := range m.jobs {
			if j.TransferID == transfer.ID && j.TransferID != "" {
				id = j.ID
				if transfer.Status == "error" {
					j.Phase, j.Error = "failed", transfer.Message
				} else if j.Phase != "completed" && j.Phase != "local" {
					j.Phase = "cloud"
					j.Progress = clampProgress(transfer.Progress) * 0.9
				}
				if err := m.saveLocked(); err != nil {
					log.Errorf("Could not persist direct job %s: %v", j.ID, err)
				}
				break
			}
		}
		m.mu.Unlock()
		if id != "" && (transfer.Status == "finished" || transfer.Status == "seeding") {
			m.startDownload(id)
		}
	}
}

func clampProgress(p float64) float64 {
	if p < 0 {
		return 0
	}
	if p > 1 {
		return 1
	}
	return p
}

// resolveRootFolder returns the memoized direct-download root folder ID.
// The memo is a (directory, ID) pair under rootMu and is re-resolved when
// the runtime-mutable TransferDirectory setting changes: a setting change
// must land new jobs in the new <dir>-direct folder instead of silently
// reusing the old one forever. Resolution stays lazy — the config update
// fan-out issues no API calls (minimal-sync policy).
func (m *Manager) resolveRootFolder() (string, error) {
	dir := m.config.TransferDirectory
	m.rootMu.Lock()
	defer m.rootMu.Unlock()
	if m.rootID == "" || m.rootDir != dir {
		id := utils.GetDownloadsFolderIDFromPremiumizeme(m.pm, dir+"-direct")
		if id == "" {
			m.rootID, m.rootDir = "", ""
			return "", errors.New("direct cloud folder is unavailable")
		}
		m.rootID, m.rootDir = id, dir
	}
	return m.rootID, nil
}

func (m *Manager) submit(ctx context.Context, id string) error {
	m.mu.Lock()
	j := m.jobs[id]
	if j == nil || j.Phase != "queued" {
		m.mu.Unlock()
		return nil
	}
	job := *j
	m.mu.Unlock()

	rootID, err := m.resolveRootFolder()
	if err != nil {
		return err
	}
	if job.CloudFolder == "" {
		folder, err := m.pm.CreateFolder(id, &rootID)
		if err != nil {
			return err
		}
		m.mu.Lock()
		if j := m.jobs[id]; j != nil {
			j.CloudFolder = folder
			if err := m.saveLocked(); err != nil {
				m.mu.Unlock()
				return err
			}
		}
		m.mu.Unlock()
		job.CloudFolder = folder
	}
	sourcePath, err := m.jobSourcePath(id)
	var data []byte
	if err == nil {
		data, err = os.ReadFile(sourcePath)
	}
	if err != nil {
		// A concurrent removal may have taken the durable row between the
		// job folder creation above and now. The job cannot be submitted
		// without its source, so the cloud folder it reserved would
		// otherwise outlive the registry with nothing left to clean it.
		if job.CloudFolder != "" {
			if err := m.pm.DeleteFolder(job.CloudFolder); err != nil {
				log.Warnf("Could not delete cloud folder of un-submittable job %s: %v", id, err)
			}
		}
		m.fail(id, "Source data missing from direct job registry")
		return err
	}
	// The in-flight submission registers in active[] BEFORE the request
	// goes out: remove() must take the cancel path for a submitting job,
	// and the completion handling below deletes a transfer that a
	// concurrent removal outlived (otherwise the request races the row
	// deletion and Premiumize keeps a transfer nobody owns).
	subCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	m.mu.Lock()
	if j := m.jobs[id]; j == nil {
		// The job was removed between the snapshot above and now: do not
		// create a transfer for it, and do not let the job folder outlive it.
		m.mu.Unlock()
		if job.CloudFolder != "" {
			if err := m.pm.DeleteFolder(job.CloudFolder); err != nil {
				log.Warnf("Could not delete cloud folder of concurrently removed job %s: %v", id, err)
			}
		}
		return nil
	}
	j.Phase = "submitting"
	m.active[id] = cancel
	if err := m.saveLocked(); err != nil {
		m.mu.Unlock()
		return err
	}
	m.mu.Unlock()

	kind := premiumizeme.TransferSourceKind(job.Kind)
	res, err := m.pm.CreateTransferFromBytes(subCtx, kind, data, job.SourceName, job.CloudFolder)
	if err != nil {
		m.mu.Lock()
		delete(m.active, id)
		m.mu.Unlock()
		if strings.Contains(strings.ToLower(err.Error()), "limit of transfers reached") || strings.Contains(strings.ToLower(err.Error()), "account_limit_reached") {
			m.mu.Lock()
			if j := m.jobs[id]; j != nil {
				j.Phase = "queued"
				_ = m.saveLocked()
			}
			m.mu.Unlock()
			return err
		}
		m.fail(id, "Premiumize submission failed; check whether a transfer was created before retrying: "+err.Error())
		return err
	}
	m.mu.Lock()
	deleteRequested := false
	if j := m.jobs[id]; j != nil {
		j.TransferID = res.ID
		j.Phase = "cloud"
		deleteRequested = j.DeleteRequested
		if err := m.saveLocked(); err != nil {
			delete(m.active, id)
			m.mu.Unlock()
			return err
		}
	}
	delete(m.active, id)
	m.mu.Unlock()
	if deleteRequested {
		// remove() raced the in-flight submission: the row stayed marked
		// DeleteRequested (the active branch) while the transfer was
		// being created. Delete the transfer and the job folder now, and
		// drop the row, before anything can reference unowned objects.
		if err := m.pm.DeleteTransfer(res.ID); err != nil {
			log.Warnf("Could not delete transfer of concurrently removed job %s: %v", id, err)
		}
		if job.CloudFolder != "" {
			if err := m.pm.DeleteFolder(job.CloudFolder); err != nil {
				log.Warnf("Could not delete cloud folder of concurrently removed job %s: %v", id, err)
			}
		}
		m.mu.Lock()
		if j := m.jobs[id]; j != nil {
			delete(m.jobs, id)
			_ = m.saveLocked()
		}
		m.mu.Unlock()
		if sourcePath, perr := m.jobSourcePath(id); perr == nil {
			if err := os.Remove(sourcePath); err != nil && !errors.Is(err, os.ErrNotExist) {
				log.Warnf("Could not delete direct source %s: %v", id, err)
			}
		}
	}
	return nil
}

func (m *Manager) fail(id, message string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if j := m.jobs[id]; j != nil {
		j.Phase, j.Error = "failed", message
		if err := m.saveLocked(); err != nil {
			log.Errorf("Could not persist direct job %s failure: %v", id, err)
		}
	}
}

func (m *Manager) startDownload(id string) {
	m.mu.Lock()
	j := m.jobs[id]
	if j == nil || j.Phase == "completed" || j.Phase == "failed" || m.active[id] != nil || j.CloudFolder == "" {
		m.mu.Unlock()
		return
	}
	max := m.config.SimultaneousDownloads
	if max <= 0 || len(m.active) >= max {
		m.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.active[id] = cancel
	j.Phase = "local"
	if err := m.saveLocked(); err != nil {
		delete(m.active, id)
		m.mu.Unlock()
		cancel()
		log.Errorf("Could not persist direct job %s before download: %v", id, err)
		return
	}
	job := *j
	m.mu.Unlock()
	go func() {
		defer func() {
			m.mu.Lock()
			delete(m.active, id)
			j := m.jobs[id]
			deleteRequested := j != nil && j.DeleteRequested
			deleteFiles := j != nil && j.DeleteFiles
			m.mu.Unlock()
			cancel()
			if deleteRequested {
				_ = m.remove(id, deleteFiles)
			}
		}()
		err := DownloadCloudFolder(ctx, m.pm, job.CloudFolder, job.OutputPath, m.config.EnableTlsCheck, m.config.DownloadSpeedLimit, func(done, total int64) {
			m.mu.Lock()
			if j := m.jobs[id]; j != nil {
				j.TotalBytes, j.Downloaded = total, done
				if total > 0 {
					j.Progress = 0.9 + 0.1*clampProgress(float64(done)/float64(total))
				}
			}
			m.mu.Unlock()
		})
		if err != nil {
			if errors.Is(err, context.Canceled) {
				m.mu.Lock()
				if j := m.jobs[id]; j != nil {
					j.Phase = "cloud"
					_ = m.saveLocked()
				}
				m.mu.Unlock()
				return
			}
			m.fail(id, "Local download failed: "+err.Error())
			return
		}
		m.mu.Lock()
		if j := m.jobs[id]; j != nil {
			j.Phase, j.Progress, j.Error = "completed", 1, ""
			if err := m.saveLocked(); err != nil {
				log.Errorf("Could not persist completion of direct job %s: %v", id, err)
			}
		}
		m.mu.Unlock()
		if sourcePath, perr := m.jobSourcePath(id); perr == nil {
			if err := os.Remove(sourcePath); err != nil && !errors.Is(err, os.ErrNotExist) {
				log.Warnf("Could not delete direct source %s: %v", id, err)
			}
		}
		// The Premiumize folder is temporary once all files are safely on
		// disk. Preserve the completed local job until *arr imports it.
		if err := m.pm.DeleteFolder(job.CloudFolder); err != nil {
			log.Warnf("Could not clean cloud folder of direct job %s: %v", id, err)
		} else {
			m.mu.Lock()
			if j := m.jobs[id]; j != nil {
				j.CloudFolder = ""
				_ = m.saveLocked()
			}
			m.mu.Unlock()
		}
	}()
}
