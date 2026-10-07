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
	ID               string   `json:"id"`
	Kind             string   `json:"kind"`
	Name             string   `json:"name"`
	Category         string   `json:"category"`
	SourceName       string   `json:"source_name"`
	Phase            string   `json:"phase"`
	Progress         float64  `json:"progress"`
	TotalBytes       int64    `json:"total_bytes,omitempty"`
	Downloaded       int64    `json:"downloaded,omitempty"`
	TransferID       string   `json:"transfer_id"`
	CloudFolder      string   `json:"cloud_folder"`
	OutputPath       string   `json:"output_path"`
	Error            string   `json:"error"`
	DeleteRequested  bool     `json:"delete_requested,omitempty"`
	DeleteFiles      bool     `json:"delete_files,omitempty"`
	ReportedFailures []string `json:"reported_failures,omitempty"`
	// CleanupPending marks a completed job whose Premiumize folder could
	// not be deleted at completion time. The poll deletion pass retries
	// the folder deletion for such jobs until it succeeds, without
	// deleting the job row itself.
	CleanupPending bool `json:"cleanup_pending,omitempty"`
	// TransferUnknown marks a job whose submission outcome is unknown: the
	// create request failed after the server may have committed the
	// transfer (a cancelled or timed-out response), or the process died
	// between the "submitting" save and the transfer-ID save. The row
	// carries no transfer ID, so no deletion path can target the orphan
	// directly; the flag lets the cleanup pass reconcile it by listing the
	// account's transfers and deleting the one committed into the job's
	// just-cleared folder. Reconciling against an EMPTY listing is
	// inconclusive — it cannot distinguish "the transfer never committed"
	// from "the commit has not landed yet" — so a row is only cleared of
	// the flag once a matching transfer is positively deleted; an empty
	// listing leaves the row pending for a later retry.
	TransferUnknown bool      `json:"transfer_unknown,omitempty"`
	Created         time.Time `json:"created"`
}

// Manager owns the direct-download namespace. The existing blackhole path
// uses a different Premiumize folder and remains independent.
type Manager struct {
	mu              sync.RWMutex
	pollMu          sync.Mutex
	pm              *premiumizeme.Premiumizeme
	config          config.Config
	stateDir        string
	jobs            map[string]*Job
	categories      map[string]bool
	active          map[string]context.CancelFunc
	removing        map[string]bool
	failureReporter func(Job) ([]string, error)
	// failureReportPolls counts failed-report passes per job in memory;
	// it bounds the reporter calls so a job no configured *arr ever
	// acknowledges cannot pin the *arr stack with a full-history refetch
	// every poll forever.
	failureReportPolls map[string]int
	rootMu             sync.Mutex // guards rootDir/rootID: submit may race the poll loop
	rootDir            string
	rootID             string
	stop               chan struct{}
}

func NewManager(pm *premiumizeme.Premiumizeme, cfg *config.Config, configDir string) (*Manager, error) {
	if configDir == "" {
		configDir = "."
	}
	stateDir := filepath.Join(configDir, "direct-jobs")
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return nil, err
	}
	m := &Manager{pm: pm, config: *cfg, stateDir: stateDir, jobs: make(map[string]*Job), categories: make(map[string]bool), active: make(map[string]context.CancelFunc), removing: make(map[string]bool), failureReportPolls: make(map[string]int), stop: make(chan struct{})}
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
				// The create request may have committed a transfer the row
				// never recorded: the cleanup pass reconciles the orphan
				// once the folder deletion converges.
				j.TransferUnknown = true
				if j.CloudFolder != "" {
					// The reserved folder would otherwise outlive the
					// recovery and sit on the account with nothing left
					// to clean it: the poll deletion pass retries it.
					j.CleanupPending = true
				}
			}
			m.jobs[j.ID] = &j
			if j.Category != "" {
				m.categories[j.Category] = true
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	// Categories the *arr clients created through the compat endpoints do
	// not belong to any job, so the jobs registry alone loses them on a
	// restart and the next *arr category save re-conflicts. Persist them
	// separately. A corrupt or missing file degrades to the same state a
	// fresh install has: the clients re-create the categories on their next
	// save, so load failures are non-fatal here (unlike the job registry).
	if data, err := os.ReadFile(m.categoriesPath()); err == nil {
		var storedCats []string
		if err := json.Unmarshal(data, &storedCats); err != nil {
			log.Warnf("Could not parse direct category registry %s: %v", m.categoriesPath(), err)
		} else {
			for _, c := range storedCats {
				m.categories[c] = true
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		log.Warnf("Could not read direct category registry %s: %v", m.categoriesPath(), err)
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

// jobSourcePath returns the on-disk path of a job's retained source
// payload. The job ID is derived from request data and becomes a file-name
// component, so it must be a safe single path component (no path separator,
// no parent-directory reference) and the joined path must stay inside the
// state directory; otherwise the file operation could reach outside the
// direct namespace.
func (m *Manager) jobSourcePath(id string) (string, error) {
	if len(id) == 0 || len(id) > 80 ||
		strings.Contains(id, "/") || strings.Contains(id, "\\") || strings.Contains(id, "..") {
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

// folderDeleteGone reports whether a DeleteFolder failure means the folder
// no longer exists on Premiumize (already deleted by the completion path,
// out-of-band, or by the account holder) rather than a transient or
// permission fault. A missing folder must converge to success on the
// retry side: re-issuing the delete against an absent folder can never
// succeed, so treating the absence as success is the only exit.
func folderDeleteGone(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	if strings.Contains(msg, " (404)") {
		return true
	}
	lower := strings.ToLower(msg)
	return strings.Contains(lower, "not found") ||
		strings.Contains(lower, "does not exist") ||
		strings.Contains(lower, "no such folder") ||
		strings.Contains(lower, "unknown folder")
}

func transferDeleteGone(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, " (404)") || strings.Contains(msg, "not found") ||
		strings.Contains(msg, "does not exist") || strings.Contains(msg, "no such transfer") ||
		strings.Contains(msg, "unknown transfer")
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

func (m *Manager) categoriesPath() string {
	return filepath.Join(m.stateDir, "categories.json")
}

// saveCategoriesLocked persists the category set atomically so a restart
// does not drop categories the clients created through the compat
// endpoints. Call with m.mu held.
func (m *Manager) saveCategoriesLocked() error {
	cats := make([]string, 0, len(m.categories))
	for c := range m.categories {
		cats = append(cats, c)
	}
	data, err := json.Marshal(cats)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(m.stateDir, ".categories-*")
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
	return os.Rename(tmp.Name(), m.categoriesPath())
}

func (m *Manager) add(kind string, data []byte, filename, category string) (Job, error) {
	if len(data) == 0 || len(data) > 64<<20 {
		return Job{}, errors.New("invalid or oversized download request")
	}
	if len(category) > 128 {
		return Job{}, errors.New("category too long")
	}
	if snap := m.configSnapshot(); snap.TransferOnlyMode {
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
	if len(id) == 0 || len(id) > 80 ||
		strings.Contains(id, "/") || strings.Contains(id, "\\") || strings.Contains(id, "..") {
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
			if old.Phase == "failed" && !old.DeleteRequested {
				// The *arr clients re-issue a grab through this
				// endpoint when they want a retry, and a terminally
				// failed job has no other retry path: reset the row
				// to queued so the queued pass re-submits it instead
				// of returning the failed corpse as if nothing changed.
				old.Phase = "queued"
				old.Error = ""
				old.Progress = 0
				old.Downloaded = 0
				old.TotalBytes = 0
				// The new attempt also gets a fresh failure-report
				// budget: a job dead-lettered by the ack cap, or listed
				// in the persisted ack list of the previous episode,
				// would otherwise never be reported to *arr again.
				old.ReportedFailures = nil
				m.failureReportPolls[old.ID] = 0
				// And the cleanup state of the attempt it re-queues is
				// dropped: a stale transfer ID re-fails the fresh job in
				// the transfers pass, and a cleanup-pending flag would
				// hand the folder the fresh submission reuses to the
				// deletion pass.
				old.CleanupPending = false
				old.TransferUnknown = false
				old.TransferID = ""
				if err := m.saveLocked(); err != nil {
					return Job{}, err
				}
			}
			return *old, nil
		}
		return Job{}, errors.New("torrent is already assigned to a different category")
	}
	outputRoot := filepath.Join(m.outputRootLocked(), "direct")
	job := &Job{ID: id, Kind: kind, Name: name, Category: category, SourceName: filename, Phase: "queued", OutputPath: filepath.Join(outputRoot, id), Created: time.Now()}
	sourcePath, err := m.jobSourcePath(id)
	if err != nil {
		return Job{}, err
	}
	if err := os.WriteFile(sourcePath, data, 0600); err != nil {
		return Job{}, err
	}
	m.jobs[id] = job
	if category != "" && !m.categories[category] {
		m.categories[category] = true
		if err := m.saveCategoriesLocked(); err != nil {
			log.Warnf("Could not persist direct category registry: %v", err)
		}
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

// configSnapshot returns the manager's private config value under m.mu.
// The manager never reads the shared App config struct: UpdateConfig
// rewrites that struct in place from an HTTP goroutine while the poll and
// compat goroutines read these fields, and only this locked snapshot
// separates the two.
func (m *Manager) configSnapshot() config.Config {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.config
}

// ConfigUpdatedCallback installs a fresh snapshot of the updated config
// after the App's config route rewrites the shared struct.
func (m *Manager) ConfigUpdatedCallback(_ config.Config, newConfig config.Config) {
	m.mu.Lock()
	m.config = newConfig
	m.mu.Unlock()
}

// outputRootLocked resolves the validated local base for direct job
// output. Call with m.mu held. The base location goes through the same
// GetDownloadsBaseLocation validation the blackhole path uses: an invalid
// base (the filesystem root, or a directory that is not writeable) must
// not be trusted into the import path the *arrs read, so the result
// falls back to the default location instead.
func (m *Manager) outputRootLocked() string {
	fallback := filepath.Join(os.TempDir(), "premiumizearrd")
	root, err := (&m.config).GetDownloadsBaseLocation()
	if err != nil {
		log.Warnf("Direct output base location %q is unusable (%v); using %s", m.config.DownloadsDirectory, err, fallback)
		return fallback
	}
	return root
}

func (m *Manager) outputRoot() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.outputRootLocked()
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
	if category != "" && !m.categories[category] {
		m.categories[category] = true
		if err := m.saveCategoriesLocked(); err != nil {
			log.Warnf("Could not persist direct category registry: %v", err)
		}
	}
	return m.saveLocked()
}

func (m *Manager) CreateCategory(category string) error {
	if category == "" || len(category) > 128 {
		return errors.New("invalid category")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.categories[category] {
		m.categories[category] = true
		if err := m.saveCategoriesLocked(); err != nil {
			log.Warnf("Could not persist direct category registry: %v", err)
		}
	}
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
	if m.removing[id] {
		// A concurrent caller is already working this row: fold this
		// caller's request into the row as well and report success,
		// instead of dropping a different deleteFiles value silently.
		j.DeleteRequested = true
		j.DeleteFiles = deleteFiles || j.DeleteFiles
		if err := m.saveLocked(); err != nil {
			m.mu.Unlock()
			return err
		}
		m.mu.Unlock()
		return nil
	}
	previousRequested, previousFiles := j.DeleteRequested, j.DeleteFiles
	j.DeleteRequested = true
	j.DeleteFiles = deleteFiles || j.DeleteFiles
	if err := m.saveLocked(); err != nil {
		j.DeleteRequested, j.DeleteFiles = previousRequested, previousFiles
		m.mu.Unlock()
		return err
	}
	if cancel := m.active[id]; cancel != nil {
		cancel()
		m.mu.Unlock()
		return nil // Cleanup runs when the downloader releases this job.
	}
	m.removing[id] = true
	job := *j
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.removing, id)
		m.mu.Unlock()
	}()

	// The remote transfer and folder are deleted BEFORE the durable row:
	// a failure in between must not leave a Premiumize object that the
	// registry no longer knows about and nothing will ever retry. While the
	// cleanup is pending the row stays, marked failed with a retry flag the
	// next poll's deletion pass (or a repeated *arr removal) re-attempts.
	if job.TransferID != "" {
		if err := m.pm.DeleteTransfer(job.TransferID); err != nil && !transferDeleteGone(err) {
			log.Warnf("Could not delete direct transfer %s: %v", job.TransferID, err)
			m.markCleanupPending(id, deleteFiles, "Remote cleanup pending: the Premiumize transfer was not deleted; the removal will be retried")
			return err
		}
		job.TransferID = ""
		if err := m.saveRemovalProgress(id, true); err != nil {
			return err
		}
	}
	if job.CloudFolder != "" {
		if err := m.pm.DeleteFolder(job.CloudFolder); err != nil {
			if !folderDeleteGone(err) {
				log.Warnf("Could not delete direct cloud folder for %s: %v", id, err)
				m.markCleanupPending(id, deleteFiles, "Remote cleanup pending: the Premiumize folder was not deleted; the removal will be retried")
				return err
			}
			// The folder is already gone (the completion path cleaned it
			// but its clear-save failed, or it was deleted out-of-band):
			// treat the delete as done and continue to the row deletion,
			// instead of retrying against an absent folder forever.
			log.Warnf("Cloud folder of direct job %s is already deleted; clearing the reference", id)
		}
		// A DeleteRequested unknown-outcome job may own a server-side
		// transfer no row references: the just-cleared folder is the last
		// handle on it. A failed reconcile keeps the row pending so the
		// next poll's deletion pass retries the whole remote cleanup.
		if job.Phase == "failed" && job.TransferID == "" && job.TransferUnknown {
			deleted, err := m.reconcileOrphanTransfer(job.CloudFolder)
			if err != nil {
				log.Warnf("Orphan transfer reconcile for direct job %s failed: %v", id, err)
				m.markCleanupPending(id, deleteFiles, "Remote cleanup pending: the orphan transfer was not deleted; the removal will be retried")
				return err
			}
			if deleted == 0 {
				// An empty listing is not proof the submission never
				// committed: the commit may land after this scan, and a
				// transfer no remaining row names would stay on the account
				// permanently. Keep the row pending, still holding its folder
				// reference and unknown flag, so the next poll's deletion
				// pass retries the reconcile; deleting the row now would
				// orphan the very transfer this removal exists to clean up.
				// A positive deletion (or a later poll's) resolves it.
				m.markCleanupPending(id, deleteFiles, "Remote cleanup pending: no orphan transfer found yet; the removal will be retried")
				return nil
			}
			// The orphan was found and deleted: the outcome is resolved, so
			// the unknown-outcome handle can clear and removal proceeds to
			// the local cleanup and row deletion.
			job.TransferUnknown = false
		}
		job.CloudFolder = ""
		if err := m.saveRemovalProgress(id, false); err != nil {
			return err
		}
	}
	// The snapshot above was captured before a concurrent caller's flag
	// could be folded into the row through the removing-guard merge;
	// re-read the row so the local cleanup decision sees the merged value.
	m.mu.RLock()
	if cur := m.jobs[id]; cur != nil {
		deleteFiles = cur.DeleteFiles
	}
	m.mu.RUnlock()
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
	m.mu.Lock()
	defer m.mu.Unlock()
	cur := m.jobs[id]
	polls := m.failureReportPolls[id]
	_, hadPolls := m.failureReportPolls[id]
	delete(m.jobs, id)
	// A re-added job with the same derived ID would otherwise inherit the
	// removed job's report counter and start dead-lettered.
	delete(m.failureReportPolls, id)
	if err := m.saveLocked(); err != nil {
		m.jobs[id] = cur
		if hadPolls {
			m.failureReportPolls[id] = polls
		}
		return err
	}
	return nil
}

// reconcileOrphanTransfer deletes the server-side transfer(s) that an
// unknown-outcome submission left behind: the just-cleared cloud folder is
// the only remaining reference, so the account's transfer list is scanned
// for the one committed into that folder. It reports how many matching
// transfers it deleted so the caller can distinguish "an orphan was found
// and resolved" from "the listing was empty": an empty list cannot prove
// the submission never committed (the commit may still be in flight), so
// only a positive deletion may clear the row's unknown-outcome handle.
// Call without m.mu held.
func (m *Manager) reconcileOrphanTransfer(folderID string) (int, error) {
	if strings.TrimSpace(folderID) == "" {
		return 0, nil
	}
	transfers, err := m.pm.GetTransfers()
	if err != nil {
		return 0, err
	}
	deleted := 0
	for _, t := range transfers {
		if t.FolderID != folderID {
			continue
		}
		if err := m.pm.DeleteTransfer(t.ID); err != nil && !transferDeleteGone(err) {
			return deleted, err
		}
		deleted++
	}
	return deleted, nil
}

// Persist each remote deletion separately so an outage or restart resumes
// from the remaining work. The row stays pending until local cleanup finishes.
func (m *Manager) saveRemovalProgress(id string, transfer bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if j := m.jobs[id]; j != nil {
		if transfer {
			j.TransferID = ""
		} else {
			j.CloudFolder = ""
		}
	}
	return m.saveLocked()
}

// markCleanupPending keeps the durable row (with a retry flag) after a
// failed remote cleanup. Call with m.mu NOT held.
func (m *Manager) markCleanupPending(id string, deleteFiles bool, message string) {
	m.mu.Lock()
	if cur := m.jobs[id]; cur != nil {
		cur.Phase, cur.Error = "failed", message
		cur.DeleteRequested = true
		// OR with the row's current value: a concurrent caller may have
		// merged its own deleteFiles=true into the row through the
		// removing-guard; assigning the caller's local would write it back
		// to false and orphan the other caller's output files.
		cur.DeleteFiles = deleteFiles || cur.DeleteFiles
		if err := m.saveLocked(); err != nil {
			log.Errorf("Could not persist direct job %s cleanup: %v", id, err)
		}
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

// The callback returns the *arr instances that acknowledged this failure.
// Acknowledgements survive restarts, while unavailable instances are retried.
func (m *Manager) SetTorrentFailureReporter(reporter func(Job) ([]string, error)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failureReporter = reporter
}

// failedReportPollCap bounds the failure-report passes per job. Each
// pass forces a full-history refetch against every configured *arr;
// unbounded, a job no *arr's history contains (a purged grab record, a
// deleted entry, a Lidarr history without downloadIds, or a multi-*arr
// setup where only one instance made the grab) pins the *arr stack with
// that refetch every 15 s forever. After the cap the job dead-letters:
// it stays failed and *arr-visible until it is removed.
const failedReportPollCap = 8

func (m *Manager) reportFailedTorrents() {
	m.mu.RLock()
	reporter := m.failureReporter
	var failed []Job
	if reporter != nil {
		for _, j := range m.jobs {
			if j.Kind != "nzb" && j.Phase == "failed" && !j.DeleteRequested {
				failed = append(failed, *j)
			}
		}
	}
	polls := make(map[string]int, len(m.failureReportPolls))
	for id, n := range m.failureReportPolls {
		polls[id] = n
	}
	m.mu.RUnlock()
	for _, job := range failed {
		if polls[job.ID] >= failedReportPollCap {
			continue
		}
		acknowledged, err := reporter(job)
		if err != nil {
			log.Warnf("Could not report direct torrent %s failure: %v", job.ID, err)
		}
		if len(acknowledged) == 0 && err == nil {
			// Every reachable *arr answered but none tracked this grab.
			// Name the silent path once instead of looping quietly.
			if polls[job.ID] == 0 {
				log.Warnf("No configured *arr acknowledged the failure of direct torrent %s; retrying for up to %d polls", job.ID, failedReportPollCap)
			}
		}
		m.mu.Lock()
		// The write-back is episode-gated: the pass started against ONE
		// failure episode, but add()'s re-queue resets the row mid-flight
		// (phase back to queued, ReportedFailures nil, the report counter
		// zeroed) — landing this pass's stale increment and the old
		// episode's acks on the fresh episode would dead-letter it on its
		// very first pass. Only a row still in the same failure episode
		// receives the write-back.
		if j := m.jobs[job.ID]; j != nil && j.Phase == "failed" {
			// Only no-error passes count against the cap: a transient *arr
			// outage must not permanently dead-letter the report — the
			// cap exists to bound the no-ack refetch storm, and error
			// passes keep retrying until the stack is reachable again.
			// Re-read the CURRENT counter rather than the pass-start
			// snapshot: a mid-pass re-queue reset it, and the stale
			// snapshot's increment must not land on the zeroed counter.
			if err == nil {
				m.failureReportPolls[job.ID] = m.failureReportPolls[job.ID] + 1
			}
			for _, target := range acknowledged {
				if !containsReport(j.ReportedFailures, target) {
					j.ReportedFailures = append(j.ReportedFailures, target)
				}
			}
			if m.failureReportPolls[job.ID] >= failedReportPollCap {
				// Terminal notice for this job: the passes above are all it
				// gets, so the *arr stack is no longer pinned by this
				// row refetching its full history every poll.
				log.Warnf("Giving up reporting direct torrent %s failure after %d polls; the job stays failed until it is removed through the *arr client", job.ID, failedReportPollCap)
			}
			if len(acknowledged) > 0 {
				if err := m.saveLocked(); err != nil {
					log.Errorf("Could not persist direct torrent %s failure report: %v", job.ID, err)
				}
			}
		}
		m.mu.Unlock()
	}
}

func containsReport(reported []string, target string) bool {
	for _, previous := range reported {
		if previous == target {
			return true
		}
	}
	return false
}

// PollOnce advances queued and cloud jobs. The method is public to allow
// deterministic tests without waiting for the service ticker.
func (m *Manager) PollOnce(ctx context.Context) {
	m.pollMu.Lock()
	// Defer order is load-bearing: defers run LIFO, so the report runs
	// AFTER the unlock. The report issues *arr network calls (a forced
	// full-history fetch per target, up to the client timeout each) and
	// must not hold the poll critical section: an unreachable *arr would
	// otherwise inflate the effective poll period to the report time and
	// stall every queued submission, in-flight download, and cleanup pass
	// behind it.
	defer m.reportFailedTorrents()
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
	// A completed job whose folder deletion failed at completion keeps a
	// cleanup-pending flag: retry the folder deletion until it succeeds,
	// WITHOUT deleting the job row (the download is delivered and *arr may
	// still be tracking it).
	m.mu.RLock()
	cleanup := make([]Job, 0)
	for _, j := range m.jobs {
		if j.CleanupPending && !j.DeleteRequested && j.CloudFolder != "" {
			cleanup = append(cleanup, *j)
		}
	}
	m.mu.RUnlock()
	for _, j := range cleanup {
		cleared := false
		if err := m.pm.DeleteFolder(j.CloudFolder); err != nil {
			if !folderDeleteGone(err) {
				log.Warnf("Direct job %s cloud folder cleanup failed again: %v", j.ID, err)
				continue
			}
			// Already deleted out-of-band: clear the reference.
			cleared = true
		} else {
			cleared = true
		}
		if cleared {
			// A failed job whose submission outcome was unknown (a
			// post-commit create failure, or a restart mid-"submitting")
			// may own a server-side transfer no row references: the
			// just-cleared folder is the last handle on it. Reconcile
			// against the live row — a job re-queued in the meantime owns
			// a fresh transfer and must not be treated as an orphan —
			// and on failure keep the folder reference and the flag so
			// the next poll's cleanup pass retries.
			var needsReconcile bool
			m.mu.RLock()
			if cur := m.jobs[j.ID]; cur != nil {
				needsReconcile = cur.Phase == "failed" && cur.TransferID == "" && cur.TransferUnknown
				// The guard must not be one-sided: a re-queue across the
				// DeleteFolder round-trip above resets the row to a fresh
				// episode (phase queued, a fresh unknown flag) while the
				// pass's snapshot still carries the PREVIOUS episode's
				// unknown flag. The failed-phase test alone then reads the
				// reset row and skips the reconcile, so the previous
				// episode's orphan transfer is named by no remaining code
				// path and stays on the account permanently.
				needsReconcile = needsReconcile ||
					(cur.Phase == "queued" && cur.TransferID == "" && j.TransferUnknown)
			}
			m.mu.RUnlock()
			if needsReconcile {
				deleted, err := m.reconcileOrphanTransfer(j.CloudFolder)
				if err != nil {
					log.Warnf("Direct job %s orphan transfer reconcile failed: %v", j.ID, err)
					continue
				}
				if deleted == 0 {
					// An empty listing is not proof the submission never
					// committed: the commit may still be in flight. Keep the
					// folder reference and the unknown-outcome flag so the
					// next poll's cleanup pass retries the reconcile; clearing
					// them now would leave a later-committed transfer named by
					// no row.
					continue
				}
			}
		}
		m.mu.Lock()
		if cur := m.jobs[j.ID]; cur != nil {
			if cleared {
				cur.CloudFolder = ""
			}
			cur.CleanupPending = false
			cur.TransferUnknown = false
			_ = m.saveLocked()
		}
		m.mu.Unlock()
	}
	m.mu.RLock()
	queued := make([]Job, 0)
	for _, j := range m.jobs {
		if j.Phase == "queued" && !j.DeleteRequested {
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
				if j.DeleteRequested || j.Phase == "failed" || j.Phase == "completed" {
					break
				}
				id = j.ID
				if transfer.Status == "error" {
					m.markFailedLocked(j, transfer.Message)
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
	m.mu.RLock()
	dir := m.config.TransferDirectory
	m.mu.RUnlock()
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
	if j == nil || j.Phase != "queued" || j.DeleteRequested {
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
		// A failed registration save must not leak the active slot: the
		// downloader never starts for this job, so nothing would ever
		// release the entry and the quota gate would pin every other
		// download until restart. Release the slot and roll the phase
		// back to "queued" (the on-disk row is still "queued", so no
		// re-save is needed and the queued pass may retry); no
		// TransferID exists yet, so nothing remote is orphaned.
		delete(m.active, id)
		j.Phase = "queued"
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
		// A post-commit failure (a cancelled or timed-out response the
		// server may have finished anyway) leaves a transfer the row can
		// never reference: mark the uncertainty so the cleanup pass can
		// reconcile the orphan once the folder deletion converges.
		m.mu.Lock()
		if j := m.jobs[id]; j != nil {
			j.TransferUnknown = true
		}
		m.mu.Unlock()
		m.fail(id, "Premiumize submission failed; check whether a transfer was created before retrying: "+err.Error())
		m.mu.Lock()
		var deleteRequested, deleteFiles bool
		if j := m.jobs[id]; j != nil {
			deleteRequested, deleteFiles = j.DeleteRequested, j.DeleteFiles
		}
		m.mu.Unlock()
		if deleteRequested {
			// A concurrent removal outlived the failed submission; run its
			// cleanup (including the orphan reconcile) now instead of
			// leaving the row for the poll deletion pass. If the orphan
			// reconcile finds no transfer yet — the commit may still be in
			// flight — remove() parks the row pending instead of deleting
			// it, and a later pass retries.
			m.remove(id, deleteFiles)
		}
		return err
	}
	m.mu.Lock()
	deleteRequested := false
	deleteFiles := false
	orphaned := false
	if j := m.jobs[id]; j != nil {
		j.TransferID = res.ID
		j.Phase = "cloud"
		deleteRequested = j.DeleteRequested
		deleteFiles = j.DeleteFiles
		if err := m.saveLocked(); err != nil {
			delete(m.active, id)
			m.mu.Unlock()
			return err
		}
	} else {
		// The durable row was removed by a concurrent remove() AFTER the
		// transfer was committed: no row will ever reference the transfer
		// (or its reserved folder), and the poll transfer loop only names
		// row-referenced IDs, so nothing else can delete them. Delete both
		// here — the account's transfer limit would otherwise leak one
		// object per such race and eventually bounce every new direct job.
		orphaned = true
	}
	delete(m.active, id)
	m.mu.Unlock()
	if orphaned {
		if err := m.pm.DeleteTransfer(res.ID); err != nil && !transferDeleteGone(err) {
			log.Warnf("Could not delete orphan transfer of removed direct job %s: %v", id, err)
		}
		if job.CloudFolder != "" {
			if err := m.pm.DeleteFolder(job.CloudFolder); err != nil && !folderDeleteGone(err) {
				log.Warnf("Could not delete cloud folder of removed direct job %s: %v", id, err)
			}
		}
		return nil
	}
	if deleteRequested {
		return m.remove(id, deleteFiles)
	}
	return nil
}

// markFailedLocked transitions a job to the terminal failed state and
// hands any reserved cloud folder to the poll deletion pass: without the
// retry flag no path ever deletes that folder (a lifetime orphan on the
// account). Call with m.mu held.
func (m *Manager) markFailedLocked(j *Job, message string) {
	j.Phase, j.Error = "failed", message
	if j.CloudFolder != "" && !j.CleanupPending {
		j.CleanupPending = true
	}
}

func (m *Manager) fail(id, message string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if j := m.jobs[id]; j != nil {
		m.markFailedLocked(j, message)
		if err := m.saveLocked(); err != nil {
			log.Errorf("Could not persist direct job %s failure: %v", id, err)
		}
	}
}

func (m *Manager) startDownload(id string) {
	m.mu.Lock()
	j := m.jobs[id]
	if j == nil || j.DeleteRequested || j.Phase == "completed" || j.Phase == "failed" || m.active[id] != nil || j.CloudFolder == "" {
		m.mu.Unlock()
		return
	}
	// A non-positive limit means unlimited here: a config that carries
	// 0 through this door (a value the web route permits and the load
	// path normalizes only at file-parse time) must not silently stall
	// every direct download, so it cannot bind the slot gate.
	max := m.config.SimultaneousDownloads
	if max > 0 && len(m.active) >= max {
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
	snap := m.config
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
		err := DownloadCloudFolder(ctx, m.pm, job.CloudFolder, job.OutputPath, job.ID, snap.EnableTlsCheck, snap.DownloadSpeedLimit, func(done, total int64) {
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
			// The cleanup flag is persisted in the SAME save as the
			// completion: a death between the two saves would otherwise
			// orphan the cloud folder, because no recovery path re-enters
			// a completed row that lacks the flag the cleanup pass needs.
			j.Phase, j.Progress, j.Error, j.CleanupPending = "completed", 1, "", true
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
			// A failure here must not orphan the folder forever: mark the
			// row cleanup-pending and let the poll deletion pass retry the
			// folder deletion for this completed job until it succeeds.
			log.Warnf("Could not clean cloud folder of direct job %s: %v", id, err)
			m.mu.Lock()
			if j := m.jobs[id]; j != nil {
				j.CleanupPending = true
				_ = m.saveLocked()
			}
			m.mu.Unlock()
		} else {
			// The folder is gone: clear the reference and the write-ahead
			// flag in the same save.
			m.mu.Lock()
			if j := m.jobs[id]; j != nil {
				j.CloudFolder = ""
				j.CleanupPending = false
				_ = m.saveLocked()
			}
			m.mu.Unlock()
		}
	}()
}
