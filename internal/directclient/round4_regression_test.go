package directclient

// Round-4 regression tests for the high-confidence criticals of the
// semantic rereview of c1624868 (PR #104): N7, N6, R3-20, N11, R3-21.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ensingerphilipp/premiumizearr-nova/pkg/premiumizeme"
)

const round4JobID = "0123456789abcdef0123456789abcdef01234567"

// TestRound4SidecarCollisionRejectedBeforeDownload is the regression test
// for N7: the fail-closed pre-check matched only a sibling's in-progress
// name, not its completion sidecar, so an accepted "<sibling>.complete"
// listing entry let the per-file marker write overwrite the sibling's
// sidecar with this entry's file ID — mixing two entries' bytes in the
// published tree. The class is refused before any download starts.
func TestRound4SidecarCollisionRejectedBeforeDownload(t *testing.T) {
	var detailsRequests atomic.Int32
	pm := managerTestPremiumize(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/folder/list":
			fmt.Fprint(w, `{"status":"success","content":[{"id":"fa","name":"a","type":"file"},{"id":"fb","name":"a.complete","type":"file"}]}`)
		case "/api/item/details":
			detailsRequests.Add(1)
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	})

	output := filepath.Join(t.TempDir(), "download")
	err := DownloadCloudFolder(context.Background(), &pm, "root", output, "job-1", true, 0, nil)
	if err == nil {
		t.Fatal("DownloadCloudFolder() error = nil, want the completion-sidecar collision refusal")
	}
	if !strings.Contains(err.Error(), "completion sidecar") {
		t.Fatalf("error = %q, want the completion-sidecar collision refusal", err)
	}
	if got := detailsRequests.Load(); got != 0 {
		t.Fatalf("item details requests = %d, want 0 (a collision refuses before any download starts)", got)
	}
	if _, statErr := os.Stat(output); !os.IsNotExist(statErr) {
		t.Fatalf("output path exists after a refused listing: %v", statErr)
	}
}

// TestRound4PublishDoesNotShipCompletionSidecars is the regression test
// for N6: the pre-publish prune claimed every per-file completion sidecar,
// but the publish renames the whole staging tree into the *arr output
// path and nothing removes the claimed sidecars — one marker file per
// downloaded file shipped into the import tree. The sidecars are
// downloader-internal bookkeeping (the retry skip-path reads them before
// the prune runs), so they are pruned like every other unclaimed entry.
func TestRound4PublishDoesNotShipCompletionSidecars(t *testing.T) {
	// Hard requirements of the production downloader, not optional: a
	// silent skip would report unverified download behavior as passing.
	if _, err := exec.LookPath("wget"); err != nil {
		t.Fatal("wget is required by the production downloader; install it (e.g. 'apt-get install wget') to run this test")
	}
	if _, err := exec.LookPath("stdbuf"); err != nil {
		t.Fatal("stdbuf is required by the production downloader; install it (e.g. 'apt-get install coreutils') to run this test")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/folder/list":
			fmt.Fprint(w, `{"status":"success","content":[{"id":"fa","name":"a","type":"file"},{"id":"fb","name":"b","type":"file"}]}`)
		case "/api/item/details":
			id := r.URL.Query().Get("id")
			fmt.Fprintf(w, `{"status":"success","id":%q,"name":%q,"type":"file","link":%q}`, id, id, serverURL(r)+"/blob/"+id)
		case "/blob/fa":
			_, _ = w.Write([]byte("payload-a"))
		case "/blob/fb":
			_, _ = w.Write([]byte("payload-b"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	pm := premiumizeme.NewPremiumizemeClient("test-secret")
	pm.APIBaseURL = server.URL + "/api/"
	pm.HTTPClient = server.Client()

	output := filepath.Join(t.TempDir(), "download")
	if err := DownloadCloudFolder(context.Background(), &pm, "root", output, "job-1", true, 0, nil); err != nil {
		t.Fatalf("DownloadCloudFolder() error = %v", err)
	}
	var sidecars []string
	err := filepath.WalkDir(output, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(d.Name(), ".complete") {
			sidecars = append(sidecars, p)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk published tree: %v", err)
	}
	if len(sidecars) != 0 {
		t.Fatalf("published tree ships completion sidecars: %v", sidecars)
	}
	// The claim set still covers the real content and the manifest.
	for name, want := range map[string]string{"a": "payload-a", "b": "payload-b"} {
		got, rerr := os.ReadFile(filepath.Join(output, name))
		if rerr != nil {
			t.Fatalf("read %s: %v", name, rerr)
		}
		if string(got) != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
	}
	manifest, rerr := os.ReadFile(filepath.Join(output, publishedManifestName))
	if rerr != nil {
		t.Fatalf("published manifest missing: %v", rerr)
	}
	if string(manifest) != "job-1" {
		t.Fatalf("published manifest = %q, want the publishing job key", manifest)
	}
}

// TestRound4OrphanTransferDeletedWhenRowRemovedMidSubmit is the regression
// test for R3-20: a concurrent remove() that snapshots the row before the
// submission (TransferID empty) deletes the durable row while
// CreateTransferFromBytes is in flight; the post-HTTP block then sees no
// row for a committed transfer and used to return nil, leaving the
// transfer (and its reserved folder) on the account until the transfer
// limit bounces new direct jobs. The else branch deletes both.
//
// The public remove() entry routes a submitting job to its cancel branch,
// so the test drops the row the way the removal's cleanup tail does to
// land the exact post-HTTP state the branch exists for.
func TestRound4OrphanTransferDeletedWhenRowRemovedMidSubmit(t *testing.T) {
	releaseTransfer := make(chan struct{}, 1)
	parked := make(chan struct{})
	var transferDeletes, folderDeletes atomic.Int32
	pm := managerTestPremiumize(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/account/info":
			fmt.Fprint(w, `{"status":"success","limit_used":0,"booster_points":0}`)
		case "/api/folder/list":
			fmt.Fprint(w, `{"status":"success","content":[]}`)
		case "/api/folder/create":
			fmt.Fprint(w, `{"status":"success","id":"job-folder"}`)
		case "/api/transfer/create":
			close(parked)
			<-releaseTransfer
			fmt.Fprint(w, `{"status":"success","id":"transfer-1"}`)
		case "/api/transfer/list":
			fmt.Fprint(w, `{"status":"success","transfers":[]}`)
		case "/api/transfer/delete":
			transferDeletes.Add(1)
			fmt.Fprint(w, `{"status":"success"}`)
		case "/api/folder/delete":
			folderDeletes.Add(1)
			fmt.Fprint(w, `{"status":"success"}`)
		default:
			http.NotFound(w, r)
		}
	})
	manager := newTestManager(t, &pm, t.TempDir())
	if err := manager.AddMagnet(context.Background(), testMagnet, "tv"); err != nil {
		t.Fatal(err)
	}
	const jobID = "0123456789abcdef0123456789abcdef01234567"
	polled := make(chan struct{})
	go func() {
		manager.PollOnce(context.Background())
		close(polled)
	}()
	<-parked
	// The concurrent removal dropped the durable row while the create was
	// in flight; no row will ever reference the transfer (or its reserved
	// folder), and the poll loop only names row-referenced IDs.
	manager.mu.Lock()
	delete(manager.jobs, jobID)
	delete(manager.failureReportPolls, jobID)
	manager.mu.Unlock()
	releaseTransfer <- struct{}{}
	<-polled
	manager.PollOnce(context.Background())
	if got := manager.ListTorrents("tv"); len(got) != 0 {
		t.Fatalf("job row remains: %#v", got)
	}
	if got := transferDeletes.Load(); got != 1 {
		t.Fatalf("orphan transfer delete calls = %d, want 1 (a committed transfer of a removed job must not leak to the account's transfer limit)", got)
	}
	if got := folderDeletes.Load(); got != 1 {
		t.Fatalf("orphan folder delete calls = %d, want 1 (the reserved folder must not outlive the removed job)", got)
	}
	waitQuiescent(t, manager)
}

// TestRound4ReportWriteBackGatedOnFailureEpisode is the regression test for
// N11: the failure-report write-back derived from the pass-start counter
// snapshot while add()'s re-queue resets the counter mid-flight — the
// stale increment landed on the zeroed counter (a fresh episode
// dead-lettered on its very first pass) and the old episode's acks grafted
// onto it. The write-back is now episode-gated and increments from the
// current counter value.
func TestRound4ReportWriteBackGatedOnFailureEpisode(t *testing.T) {
	pm := managerTestPremiumize(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/account/info":
			fmt.Fprint(w, `{"status":"success","limit_used":0,"booster_points":100}`)
		case "/api/transfer/list":
			fmt.Fprint(w, `{"status":"success","transfers":[]}`)
		default:
			http.NotFound(w, r)
		}
	})
	manager := newTestManager(t, &pm, t.TempDir())
	const requeuedMagnet = "magnet:?xt=urn:btih:fedcba9876543210fedcba9876543210fedcba98&dn=Requeued.Release"
	if err := manager.AddMagnet(context.Background(), testMagnet, "tv"); err != nil {
		t.Fatal(err)
	}
	if err := manager.AddMagnet(context.Background(), requeuedMagnet, "tv"); err != nil {
		t.Fatal(err)
	}
	const requeuedID = "fedcba9876543210fedcba9876543210fedcba98"
	manager.mu.Lock()
	if j := manager.jobs[round4JobID]; j != nil {
		j.Phase = "failed"
	}
	if j := manager.jobs[requeuedID]; j != nil {
		j.Phase = "failed"
	}
	manager.mu.Unlock()

	manager.SetTorrentFailureReporter(func(j Job) ([]string, error) {
		switch j.ID {
		case round4JobID:
			// The *arr client re-grabs mid-pass: add() resets the row
			// to a fresh episode (phase queued, the report counter
			// zeroed, the ack list cleared) while this pass is still
			// running against the previous episode's snapshot.
			if err := manager.AddMagnet(context.Background(), testMagnet, "tv"); err != nil {
				t.Errorf("re-queue the failed job: %v", err)
			}
			return []string{"sonarr"}, nil
		case requeuedID:
			return []string{"radarr"}, nil
		}
		return nil, nil
	})

	manager.PollOnce(context.Background())

	manager.mu.RLock()
	a := manager.jobs[round4JobID]
	b := manager.jobs[requeuedID]
	pollsA := manager.failureReportPolls[round4JobID]
	pollsB := manager.failureReportPolls[requeuedID]
	manager.mu.RUnlock()
	if a == nil || b == nil {
		t.Fatalf("rows missing after the pass: a=%v b=%v", a, b)
	}
	// The pass's stale increment and the old episode's acks must not land
	// on the fresh episode: the counter stays zero and no ack grafts, or
	// the fresh episode starts one pass into its own dead-letter budget.
	if pollsA != 0 {
		t.Errorf("re-queued job's report counter = %d, want 0 (the pass's stale increment must not land on the zeroed counter)", pollsA)
	}
	if len(a.ReportedFailures) != 0 {
		t.Errorf("re-queued job's acknowledgements = %#v, want none (the old episode's acks must not graft onto the fresh one)", a.ReportedFailures)
	}
	if a.Phase != "queued" {
		t.Errorf("re-queued job phase = %q, want queued", a.Phase)
	}
	// A job that stayed failed receives the write-back as before.
	if pollsB != 1 {
		t.Errorf("failed job's report counter = %d, want 1 (the no-error pass counts against the cap)", pollsB)
	}
	if !containsReport(b.ReportedFailures, "radarr") {
		t.Errorf("failed job's acknowledgements = %#v, want radarr", b.ReportedFailures)
	}
	waitQuiescent(t, manager)
}

// TestRound4CleanupPassReconcilesAcrossRequeue is the regression test for
// R3-21: the cleanup pass's live-row guard was one-sided — a re-queue
// across the DeleteFolder round-trip resets the row to a fresh episode
// (phase queued, a fresh unknown flag) while the pass's snapshot still
// carries the PREVIOUS episode's unknown flag, so the failed-phase test
// alone read the reset row and skipped the reconcile, leaving the
// previous episode's orphan transfer on the account permanently. The
// guard also reconciles a fresh row that inherits the snapshot's flag.
func TestRound4CleanupPassReconcilesAcrossRequeue(t *testing.T) {
	releaseFolder := make(chan struct{})
	folderParked := make(chan struct{})
	var orphanDeletes atomic.Int32
	var orphanGone atomic.Bool
	pm := managerTestPremiumize(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/account/info":
			fmt.Fprint(w, `{"status":"success","limit_used":0,"booster_points":100}`)
		case "/api/folder/list":
			fmt.Fprint(w, `{"status":"success","content":[]}`)
		case "/api/folder/create":
			fmt.Fprint(w, `{"status":"success","id":"new-folder"}`)
		case "/api/transfer/create":
			fmt.Fprint(w, `{"status":"success","id":"transfer-new","name":"Example.Release","type":"magnet"}`)
		case "/api/transfer/list":
			if orphanGone.Load() {
				fmt.Fprint(w, `{"status":"success","transfers":[]}`)
			} else {
				fmt.Fprint(w, `{"status":"success","transfers":[{"id":"T-orph-1","name":"Example.Release","status":"error","progress":1,"folder_id":"folder-1"}]}`)
			}
		case "/api/transfer/delete":
			_ = r.ParseForm()
			if r.FormValue("id") == "T-orph-1" {
				orphanDeletes.Add(1)
				orphanGone.Store(true)
			}
			fmt.Fprint(w, `{"status":"success"}`)
		case "/api/folder/delete":
			close(folderParked)
			<-releaseFolder
			fmt.Fprint(w, `{"status":"success"}`)
		default:
			http.NotFound(w, r)
		}
	})
	manager := newTestManager(t, &pm, t.TempDir())
	if err := manager.AddMagnet(context.Background(), testMagnet, "tv"); err != nil {
		t.Fatal(err)
	}
	const jobID = "0123456789abcdef0123456789abcdef01234567"
	// The failed unknown-outcome shape the cleanup pass picks up: the
	// previous episode committed a transfer the row no longer references.
	manager.mu.Lock()
	if j := manager.jobs[jobID]; j != nil {
		j.Phase = "failed"
		j.CleanupPending = true
		j.TransferUnknown = true
		j.CloudFolder = "folder-1"
	}
	manager.mu.Unlock()

	polled := make(chan struct{})
	go func() {
		manager.PollOnce(context.Background())
		close(polled)
	}()
	<-folderParked
	// The *arr client re-grabs the release during the folder-deletion
	// round-trip: the row resets to a fresh episode before the pass's
	// live-row guard reads it.
	if err := manager.AddMagnet(context.Background(), testMagnet, "tv"); err != nil {
		t.Fatalf("re-queue the failed job: %v", err)
	}
	releaseFolder <- struct{}{}
	<-polled
	manager.PollOnce(context.Background())

	if got := orphanDeletes.Load(); got != 1 {
		t.Fatalf("orphan transfer deletes = %d, want 1 (a re-queue across the round-trip must not drop the previous episode's orphan reconcile)", got)
	}
	manager.mu.RLock()
	j := manager.jobs[jobID]
	manager.mu.RUnlock()
	if j == nil {
		t.Fatal("re-queued row missing after the pass")
	}
	if j.TransferUnknown {
		t.Errorf("fresh episode still flagged unknown after the previous episode's reconcile succeeded")
	}
	if j.CleanupPending {
		t.Errorf("fresh episode still cleanup-pending after the folder deletion succeeded")
	}
	// The previous episode's folder reference must be gone from the row;
	// the fresh episode's own submission reserves its own folder.
	if j.CloudFolder != "new-folder" {
		t.Errorf("fresh episode's cloud folder reference = %q, want its own new reserved folder (the previous episode's reference must be cleared)", j.CloudFolder)
	}
	if j.TransferID != "transfer-new" {
		t.Errorf("fresh episode's transfer ID = %q, want the new submission's transfer, not the orphaned one", j.TransferID)
	}
	waitQuiescent(t, manager)
}
