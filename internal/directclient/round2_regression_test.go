package directclient

// Round-2 regression tests: one per high-confidence critical of the second
// AO semantic review of PR #104, written to fail against the pre-fix code.
// Each test names the review finding it locks down in its doc comment.

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ensingerphilipp/premiumizearr-nova/internal/config"
	"github.com/ensingerphilipp/premiumizearr-nova/pkg/premiumizeme"
)

// TestDownloadCloudFolderRejectsIDSuffixCollision is the round-2 regression
// test for the finding that a cloud entry whose name is another entry's
// in-progress staging name ("<name>.<id>.partial") makes one file's wget
// resume against the other file's completed bytes. The downloader now fails
// closed on the class before any download starts.
func TestDownloadCloudFolderRejectsIDSuffixCollision(t *testing.T) {
	linkRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/folder/list":
			// Listing order matters: the colliding entry is published
			// BEFORE the entry whose in-progress name it is named after.
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "content": []map[string]string{
				{"id": "fb", "name": "x.fa.partial", "type": "file"},
				{"id": "fa", "name": "x", "type": "file"},
			}})
		case "/api/item/details":
			linkRequests++
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "success", "type": "file", "id": r.URL.Query().Get("id"), "name": "x", "link": serverURL(r) + "/blob"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	pm := premiumizeme.NewPremiumizemeClient("test-secret")
	pm.APIBaseURL = server.URL + "/api/"
	pm.HTTPClient = server.Client()

	output := filepath.Join(t.TempDir(), "download")
	err := DownloadCloudFolder(context.Background(), &pm, "root", output, "job-1", true, 0, nil)
	if err == nil {
		t.Fatal("a listing whose entry name is a sibling's in-progress name was accepted; the call must refuse before mixing content")
	}
	if !strings.Contains(err.Error(), "collides with another file's in-progress name") {
		t.Fatalf("error = %q, want the ID-suffix collision diagnosis", err)
	}
	if linkRequests != 0 {
		t.Errorf("generated %d Premiumize links after the collision was detected", linkRequests)
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Errorf("a refused publish created the output directory: %v", err)
	}
}

// TestDownloadCloudFolderPrunesStaleStagingBeforePublish is the round-2
// regression test for the finding that the Lstat skip trusted ANY existing
// staged target, so a stale target left by an earlier listing was re-used
// under the replaced entry's name, and unclaimed staged entries (in-flight
// names of replaced entries) were published into the release. The skip now
// requires the completion sidecar to name the entry that produced the
// target, and unclaimed staged entries are pruned before the atomic publish.
func TestDownloadCloudFolderPrunesStaleStagingBeforePublish(t *testing.T) {
	if _, err := exec.LookPath("wget"); err != nil {
		t.Fatal("wget is required by the production downloader; install it (e.g. 'apt-get install wget') to run this test")
	}
	if _, err := exec.LookPath("stdbuf"); err != nil {
		t.Fatal("stdbuf is required by the production downloader; install it (e.g. 'apt-get install coreutils') to run this test")
	}
	// Listing generation 1 names both files with IDs a1/c1; generation 2
	// replaces both entries (a2/c2), modelling the replaced folder a
	// retry re-lists.
	gens := map[string][]map[string]string{
		"1": {{"id": "a1", "name": "a", "type": "file"}, {"id": "c1", "name": "c", "type": "file"}},
		"2": {{"id": "a2", "name": "a", "type": "file"}, {"id": "c2", "name": "c", "type": "file"}},
	}
	var gen atomic.Value
	gen.Store("1")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/folder/list":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "content": gens[gen.Load().(string)]})
		case "/api/item/details":
			id := r.URL.Query().Get("id")
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "success", "type": "file", "id": id, "name": id, "link": serverURL(r) + "/blob/" + id})
		case "/blob/a1":
			_, _ = w.Write([]byte("A-old content for file a (first listing)"))
		case "/blob/a2":
			_, _ = w.Write([]byte("A-new content for file a (second listing)"))
		case "/blob/c1":
			// A 404 is a definitive failure: wget does not retry it,
			// so the first run terminates here.
			http.NotFound(w, r)
		case "/blob/c2":
			_, _ = w.Write([]byte("C-new content for file c (second listing)"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	pm := premiumizeme.NewPremiumizemeClient("test-secret")
	pm.APIBaseURL = server.URL + "/api/"
	pm.HTTPClient = server.Client()

	output := filepath.Join(t.TempDir(), "download")
	stagePath := output + ".partial"

	// Run 1 downloads "a" (first listing) and then fails on c1, so the
	// staging tree survives the call with an interrupted generation in it.
	if err := DownloadCloudFolder(context.Background(), &pm, "root", output, "job-1", true, 0, nil); err == nil {
		t.Fatal("run 1: want an error from the failed file c1, got nil")
	}
	// Plant the interrupted-generation leftover the retry must not
	// publish: the in-progress name of the replaced entry c1.
	if err := os.WriteFile(filepath.Join(stagePath, "c.c1.partial"), []byte("stale bytes of an earlier listing"), 0600); err != nil {
		t.Fatal(err)
	}

	// Run 2 re-lists the replaced folder and must publish the new
	// entries' bytes only.
	gen.Store("2")
	if err := DownloadCloudFolder(context.Background(), &pm, "root", output, "job-1", true, 0, nil); err != nil {
		t.Fatalf("run 2: %v", err)
	}
	// "a" must hold the NEW entry's bytes: a skip that merely found the
	// staged target would keep the first listing's bytes under the
	// replaced name.
	got, err := os.ReadFile(filepath.Join(output, "a"))
	if err != nil {
		t.Fatalf("published a missing: %v", err)
	}
	if string(got) != "A-new content for file a (second listing)" {
		t.Fatalf("published a = %q, want the second listing's content", got)
	}
	got, err = os.ReadFile(filepath.Join(output, "c"))
	if err != nil {
		t.Fatalf("published c missing: %v", err)
	}
	if string(got) != "C-new content for file c (second listing)" {
		t.Fatalf("published c = %q, want the second listing's content", got)
	}
	// No in-progress entry of any generation may reach the published tree.
	var partials []string
	filepath.WalkDir(output, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".partial") {
			partials = append(partials, p)
		}
		return nil
	})
	if len(partials) != 0 {
		t.Fatalf("published tree carries in-progress entries %v; an earlier listing's staging must be pruned before publish", partials)
	}
	if _, err := os.Stat(stagePath); !os.IsNotExist(err) {
		t.Errorf("staging directory remains after successful publish: %v", err)
	}
}

// TestAddRequeueResetsFailureReportState is the round-2 regression test
// for the finding that a re-grab reset the job's phase but kept the
// previous failure episode's ack list and its report budget, so the new
// attempt was never reported to *arr again (and a counter at the cap
// dead-lettered the report permanently).
func TestAddRequeueResetsFailureReportState(t *testing.T) {
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
	if err := manager.AddMagnet(context.Background(), testMagnet, "tv"); err != nil {
		t.Fatal(err)
	}
	const jobID = "0123456789abcdef0123456789abcdef01234567"
	manager.mu.Lock()
	if j := manager.jobs[jobID]; j != nil {
		// The previous episode: its failure was reported to sonarr,
		// and the job accumulated the full report budget.
		j.Phase = "failed"
		j.ReportedFailures = []string{"sonarr"}
		j.TransferID = "transfer-1"
		j.CleanupPending = true
		if err := manager.saveLocked(); err != nil {
			manager.mu.Unlock()
			t.Fatal(err)
		}
	}
	manager.failureReportPolls[jobID] = failedReportPollCap
	manager.mu.Unlock()

	// The *arr client re-grabs the same release: the re-queue branch.
	if err := manager.AddMagnet(context.Background(), testMagnet, "tv"); err != nil {
		t.Fatalf("re-grab: %v", err)
	}
	manager.mu.RLock()
	job := *manager.jobs[jobID]
	manager.mu.RUnlock()
	if job.Phase != "queued" {
		t.Fatalf("phase after re-grab = %q, want queued", job.Phase)
	}
	if job.ReportedFailures != nil {
		t.Fatalf("ReportedFailures after re-grab = %#v, want nil (the previous episode's acks must not gate the new attempt)", job.ReportedFailures)
	}
	if n := manager.failureReportPolls[jobID]; n != 0 {
		t.Fatalf("failure-report passes after re-grab = %d, want 0 (a fresh attempt gets a fresh budget)", n)
	}
	if job.TransferID != "" {
		t.Fatalf("TransferID after re-grab = %q, want cleared (a stale transfer re-fails the fresh job in the transfers pass)", job.TransferID)
	}
	if job.CleanupPending {
		t.Fatal("stale CleanupPending survives the re-grab; the deletion pass would hand the folder the fresh submission reuses to the deletion pass")
	}

	// A NEW failure episode must be reportable: a job carrying the old
	// episode's counter (or dead-lettered at the cap) would never reach
	// *arr again.
	manager.mu.Lock()
	if j := manager.jobs[jobID]; j != nil {
		j.Phase = "failed"
	}
	manager.mu.Unlock()
	var reporterCalls atomic.Int32
	manager.SetTorrentFailureReporter(func(j Job) ([]string, error) {
		reporterCalls.Add(1)
		return []string{"sonarr"}, nil
	})
	manager.PollOnce(context.Background())
	if reporterCalls.Load() == 0 {
		t.Fatalf("a fresh failure episode was never reported: the previous episode's report budget still gates it (polls = %d)", manager.failureReportPolls[jobID])
	}
	manager.mu.RLock()
	acked := manager.jobs[jobID].ReportedFailures
	manager.mu.RUnlock()
	if !containsReport(acked, "sonarr") {
		t.Fatalf("acknowledgement not recorded after a reachable report pass: %#v", acked)
	}
	waitQuiescent(t, manager)
}

// TestAddRequeueClearsStaleTransferAndCleanupState is the round-2
// regression test for the finding that a re-queued job kept its previous
// attempt's transfer ID and cleanup-pending flag: the transfers pass
// re-failed the fresh job against the stale transfer, and the deletion
// pass deleted the cloud folder the fresh submission reuses.
func TestAddRequeueClearsStaleTransferAndCleanupState(t *testing.T) {
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
	if err := manager.AddMagnet(context.Background(), testMagnet, "tv"); err != nil {
		t.Fatal(err)
	}
	const jobID = "0123456789abcdef0123456789abcdef01234567"
	manager.mu.Lock()
	if j := manager.jobs[jobID]; j != nil {
		j.Phase = "failed"
		j.TransferID = "transfer-1"
		j.CleanupPending = true
		j.CloudFolder = "folder-1"
		if err := manager.saveLocked(); err != nil {
			manager.mu.Unlock()
			t.Fatal(err)
		}
	}
	manager.mu.Unlock()

	if err := manager.AddMagnet(context.Background(), testMagnet, "tv"); err != nil {
		t.Fatalf("re-grab: %v", err)
	}
	manager.mu.RLock()
	got := *manager.jobs[jobID]
	manager.mu.RUnlock()
	if got.Phase != "queued" {
		t.Fatalf("phase after re-grab = %q, want queued", got.Phase)
	}
	if got.TransferID != "" {
		t.Fatalf("stale transfer ID %q survives the re-grab; the transfers pass re-fails the fresh job against a transfer that belongs to the previous attempt", got.TransferID)
	}
	if got.CleanupPending {
		t.Fatal("stale CleanupPending survives the re-grab; the deletion pass would delete the folder the fresh submission reuses")
	}
	if got.CloudFolder != "folder-1" {
		t.Fatalf("cloud folder reference after re-grab = %q, want folder-1 (the fresh submission reuses the reserved folder)", got.CloudFolder)
	}
	waitQuiescent(t, manager)
}

// TestRemoveConcurrentSecondCallerFoldsDeleteFiles is the round-2
// regression test for the finding that a second RemoveTorrent caller for a
// row another caller is already removing returned nil without folding its
// deleteFiles value into the row: the request was silently dropped, and the
// running caller kept the flag it had captured before the second caller
// arrived, so deleteFiles=true never reached the local cleanup.
func TestRemoveConcurrentSecondCallerFoldsDeleteFiles(t *testing.T) {
	block := make(chan struct{})
	var transferDeletes, folderDeletes atomic.Int32
	pm := managerTestPremiumize(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/transfer/delete":
			transferDeletes.Add(1)
			<-block
			fmt.Fprint(w, `{"status":"success"}`)
		case "/api/folder/delete":
			folderDeletes.Add(1)
			fmt.Fprint(w, `{"status":"success"}`)
		case "/api/transfer/list":
			fmt.Fprint(w, `{"status":"success","transfers":[]}`)
		default:
			http.NotFound(w, r)
		}
	})
	manager := newTestManager(t, &pm, t.TempDir())
	if err := manager.AddMagnet(context.Background(), testMagnet, "tv"); err != nil {
		t.Fatal(err)
	}
	const jobID = "0123456789abcdef0123456789abcdef01234567"
	manager.mu.Lock()
	job := manager.jobs[jobID]
	job.Phase, job.Progress = "completed", 1
	job.TransferID, job.CloudFolder = "transfer-1", "folder-1"
	if err := manager.saveLocked(); err != nil {
		manager.mu.Unlock()
		t.Fatal(err)
	}
	manager.mu.Unlock()
	localFile := filepath.Join(job.OutputPath, "episode.mkv")
	if err := os.MkdirAll(job.OutputPath, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(localFile, []byte("media"), 0600); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		done <- manager.RemoveTorrent(jobID, false)
	}()
	// Wait for the first caller to park inside the transfer delete,
	// i.e. past the removing-guard merge it owns.
	deadline := time.Now().Add(10 * time.Second)
	for transferDeletes.Load() == 0 {
		select {
		case <-done:
			t.Fatal("first caller finished before parking in the transfer delete")
		case <-time.After(5 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatal("first caller never reached the transfer delete")
		}
	}
	// While the first caller is parked, a second caller asks for the
	// SAME row with deleteFiles=true.
	if err := manager.RemoveTorrent(jobID, true); err != nil {
		t.Fatalf("second concurrent removal error = %v", err)
	}
	manager.mu.RLock()
	merged := manager.jobs[jobID].DeleteFiles
	manager.mu.RUnlock()
	if !merged {
		t.Fatal("the second caller's deleteFiles value was dropped by the removing guard: the row still has DeleteFiles=false, so the running caller deletes nothing")
	}
	close(block)
	if err := <-done; err != nil {
		t.Fatalf("first caller error = %v", err)
	}
	if _, err := os.Stat(localFile); err == nil {
		t.Fatal("the merged deleteFiles never reached the running caller: the job's local data is still present")
	}
	if len(manager.ListTorrents("tv")) != 0 {
		t.Fatalf("job row remains after the merged removal: %#v", manager.ListTorrents("tv"))
	}
	if transferDeletes.Load() != 1 {
		t.Fatalf("transfer delete calls = %d, want 1 (exactly one caller deletes the transfer)", transferDeletes.Load())
	}
	if folderDeletes.Load() != 1 {
		t.Fatalf("folder delete calls = %d, want 1", folderDeletes.Load())
	}
	waitQuiescent(t, manager)
}

// TestFailureReportPollsIgnoreErrorPasses is the round-2 regression test
// for the finding that a failing *arr reporter pass (a transient outage of
// the stack) counted against the report cap, so one outage of cap length
// dead-lettered the report permanently. Only error-free passes may count.
func TestFailureReportPollsIgnoreErrorPasses(t *testing.T) {
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
	if err := manager.AddMagnet(context.Background(), testMagnet, "tv"); err != nil {
		t.Fatal(err)
	}
	const jobID = "0123456789abcdef0123456789abcdef01234567"
	manager.mu.Lock()
	if j := manager.jobs[jobID]; j != nil {
		j.Phase = "failed"
	}
	manager.mu.Unlock()
	var outage atomic.Bool
	outage.Store(true)
	var reporterCalls atomic.Int32
	manager.SetTorrentFailureReporter(func(j Job) ([]string, error) {
		reporterCalls.Add(1)
		if outage.Load() {
			return nil, fmt.Errorf("arr stack unreachable")
		}
		return []string{"sonarr"}, nil
	})

	// Outage phase: every pass errors, so none of them may count
	// against the cap.
	for i := 0; i < 10; i++ {
		manager.PollOnce(context.Background())
	}
	if got := reporterCalls.Load(); got != 10 {
		t.Fatalf("reporter calls during the outage = %d, want 10 (a failing pass must not consume the report budget)", got)
	}

	// Recovery phase: the report succeeds and is capped after
	// failedReportPollCap good passes.
	outage.Store(false)
	for i := 0; i < 19; i++ {
		manager.PollOnce(context.Background())
	}
	if got := reporterCalls.Load(); got != 10+failedReportPollCap {
		t.Fatalf("total reporter calls = %d, want %d (10 outage passes + %d capped recovery passes)", got, 10+failedReportPollCap, failedReportPollCap)
	}
	manager.mu.RLock()
	acked := manager.jobs[jobID].ReportedFailures
	manager.mu.RUnlock()
	if !containsReport(acked, "sonarr") {
		t.Fatalf("acknowledgement not recorded after the recovered passes: %#v", acked)
	}
	waitQuiescent(t, manager)
}

// TestCleanupPassReconcilesOrphanTransfer is the round-2 regression test
// for the finding that the poll cleanup pass deleted the cloud folder of a
// failed unknown-outcome job without ever reaching the server-side
// transfer the unknown submission may have committed into it. The pass
// now reconciles the orphan through the just-cleared folder and keeps the
// row pending when the reconcile fails.
//
// The fixture hand-writes the persisted registry and asserts through it:
// the old-code shape of the row (the cleanup flag, the folder reference)
// is the contract both code versions persist, so the test compiles
// against the pre-fix code, where the flag is simply never written.
func TestCleanupPassReconcilesOrphanTransfer(t *testing.T) {
	var listBroken atomic.Bool
	var orphanDeletes, folderDeletes atomic.Int32
	pm := managerTestPremiumize(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/account/info":
			fmt.Fprint(w, `{"status":"success","limit_used":0,"booster_points":100}`)
		case "/api/transfer/list":
			if listBroken.Load() {
				http.Error(w, "premiumize unavailable", http.StatusServiceUnavailable)
				return
			}
			fmt.Fprint(w, `{"status":"success","transfers":[{"id":"T-orph-1","name":"Example.Release","status":"error","progress":1,"folder_id":"folder-1"}]}`)
		case "/api/transfer/delete":
			// DeleteTransfer posts the id as a form field.
			_ = r.ParseForm()
			if r.FormValue("id") == "T-orph-1" {
				orphanDeletes.Add(1)
			}
			fmt.Fprint(w, `{"status":"success"}`)
		case "/api/folder/delete":
			folderDeletes.Add(1)
			fmt.Fprint(w, `{"status":"success"}`)
		default:
			http.NotFound(w, r)
		}
	})
	configDir := t.TempDir()
	stateDir := filepath.Join(configDir, "direct-jobs")
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		t.Fatal(err)
	}
	rows := `[{"id":"0123456789abcdef0123456789abcdef01234567","kind":"magnet","name":"Example.Release","category":"tv","phase":"failed","transfer_id":"","cloud_folder":"folder-1","cleanup_pending":true,"transfer_unknown":true,"created":"2026-10-06T00:00:00Z"}]`
	if err := os.WriteFile(filepath.Join(stateDir, "jobs.json"), []byte(rows), 0600); err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(&pm, &config.Config{TransferDirectory: "arrDownloads"}, configDir)
	if err != nil {
		t.Fatalf("restart manager: %v", err)
	}
	const jobID = "0123456789abcdef0123456789abcdef01234567"

	readPersisted := func() map[string]any {
		data, err := os.ReadFile(filepath.Join(stateDir, "jobs.json"))
		if err != nil {
			t.Fatalf("read persisted registry: %v", err)
		}
		var stored []map[string]any
		if err := json.Unmarshal(data, &stored); err != nil {
			t.Fatalf("parse persisted registry: %v", err)
		}
		if len(stored) != 1 {
			t.Fatalf("persisted rows = %d, want 1", len(stored))
		}
		return stored[0]
	}

	// Arm 1: the reconcile's transfer listing is broken; the row must keep
	// the folder reference and the pending flag so the next poll retries.
	listBroken.Store(true)
	manager.PollOnce(context.Background())
	row := readPersisted()
	if row["cleanup_pending"] != true {
		t.Fatalf("after a failed reconcile the row must keep cleanup_pending: %v", row)
	}
	if row["cloud_folder"] != "folder-1" {
		t.Fatalf("after a failed reconcile the row must keep the folder reference (the last handle on the orphan): %v", row)
	}
	if row["transfer_unknown"] != true {
		t.Fatalf("after a failed reconcile the row must keep transfer_unknown: %v", row)
	}
	if orphanDeletes.Load() != 0 {
		t.Fatalf("orphan transfer deletes = %d after a failed reconcile, want 0", orphanDeletes.Load())
	}

	// Arm 2: the listing works; the orphan committed into the
	// just-cleared folder is deleted and the row converges.
	listBroken.Store(false)
	manager.PollOnce(context.Background())
	row = readPersisted()
	if v, ok := row["cleanup_pending"]; ok && v == true {
		t.Fatalf("cleanup_pending still set after the successful reconcile: %v", row)
	}
	if v, ok := row["cloud_folder"]; ok && v != "" {
		t.Fatalf("cloud_folder still set after the successful reconcile: %v", row)
	}
	if v, ok := row["transfer_unknown"]; ok && v == true {
		t.Fatalf("transfer_unknown still set after the successful reconcile: %v", row)
	}
	if orphanDeletes.Load() != 1 {
		t.Fatalf("orphan transfer deletes = %d, want 1 (the account still holds a transfer no row references)", orphanDeletes.Load())
	}
	if folderDeletes.Load() != 2 {
		t.Fatalf("folder delete calls = %d, want 2 (one per arm)", folderDeletes.Load())
	}
	waitQuiescent(t, manager)
}

// TestCompletionPersistsCleanupPendingWriteAhead is the round-2 regression
// test for the finding that the completion save and the cleanup-pending
// save were separate: a death between them left a completed row with the
// cloud folder set but no pending flag, and no recovery path re-enters a
// completed row, so the folder sat on the account forever. The flag is now
// persisted in the SAME save as the completion, before the folder deletion
// is issued.
func TestCompletionPersistsCleanupPendingWriteAhead(t *testing.T) {
	if _, err := exec.LookPath("wget"); err != nil {
		t.Fatal("wget is required by the production downloader; install it (e.g. 'apt-get install wget') to run this test")
	}
	if _, err := exec.LookPath("stdbuf"); err != nil {
		t.Fatal("stdbuf is required by the production downloader; install it (e.g. 'apt-get install coreutils') to run this test")
	}
	const (
		folder  = "job-folder-1"
		payload = "finished media payload"
	)
	var transferStatus atomic.Value
	transferStatus.Store("downloading")
	var createCount, folderCount atomic.Int32
	type completionSnapshot struct {
		phase          string
		cloudFolder    string
		cleanupPending bool
	}
	var atDelete atomic.Value

	configDir := t.TempDir()
	downloadsDir := filepath.Join(t.TempDir(), "downloads")
	if err := os.MkdirAll(downloadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		TransferDirectory:     "arrDownloads",
		DownloadsDirectory:    downloadsDir,
		SimultaneousDownloads: 1,
		EnableTlsCheck:        true,
	}
	const jobID = "0123456789abcdef0123456789abcdef01234567"
	var manager *Manager
	var api *httptest.Server
	api = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/account/info":
			fmt.Fprint(w, `{"status":"success","limit_used":0,"booster_points":0}`)
		case "/api/folder/list":
			if id := r.URL.Query().Get("id"); id == folder {
				fmt.Fprint(w, `{"status":"success","content":[{"id":"file-1","name":"episode.mkv","type":"file"}]}`)
			} else {
				fmt.Fprint(w, `{"status":"success","content":[]}`)
			}
		case "/api/folder/create":
			if folderCount.Add(1) == 1 {
				fmt.Fprint(w, `{"status":"success","id":"direct-root"}`)
			} else {
				fmt.Fprint(w, `{"status":"success","id":"job-folder-1"}`)
			}
		case "/api/transfer/create":
			createCount.Add(1)
			fmt.Fprint(w, `{"status":"success","id":"transfer-1","name":"Example.Release","type":"torrent"}`)
		case "/api/transfer/list":
			fmt.Fprintf(w, `{"status":"success","transfers":[{"id":"transfer-1","name":"Example.Release","status":%q,"progress":0.5}]}`, transferStatus.Load().(string))
		case "/api/item/details":
			fmt.Fprintf(w, `{"status":"success","id":"file-1","name":"episode.mkv","type":"file","link":%q}`, api.URL+"/media")
		case "/media":
			w.Header().Set("Content-Type", "application/octet-stream")
			fmt.Fprint(w, payload)
		case "/api/folder/delete":
			// The row as it stands at the moment the completion path
			// asks Premiumize to delete the cloud folder: the write-ahead
			// save must have landed before this request.
			manager.mu.Lock()
			if j := manager.jobs[jobID]; j != nil {
				atDelete.Store(completionSnapshot{j.Phase, j.CloudFolder, j.CleanupPending})
			}
			manager.mu.Unlock()
			fmt.Fprint(w, `{"status":"success"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()

	pm := premiumizeme.NewPremiumizemeClient("test-key")
	pm.APIBaseURL = api.URL + "/api/"
	pm.HTTPClient = api.Client()

	manager, err := NewManager(&pm, cfg, configDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.AddMagnet(context.Background(), testMagnet, "tv"); err != nil {
		t.Fatalf("queue magnet: %v", err)
	}
	manager.PollOnce(context.Background())
	transferStatus.Store("finished")
	manager.PollOnce(context.Background())

	deadline := time.Now().Add(10 * time.Second)
	var snap completionSnapshot
	for {
		jobs := manager.ListTorrents("tv")
		if len(jobs) == 1 && jobs[0].State == "completed" {
			if v := atDelete.Load(); v != nil {
				snap = v.(completionSnapshot)
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("completion did not converge: jobs = %#v", jobs)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if snap.phase != "completed" || snap.cloudFolder != folder || !snap.cleanupPending {
		t.Fatalf("row at the cloud-folder delete = {phase %q folder %q cleanupPending %v}, want {completed %s true}: the cleanup flag must be persisted in the same save as the completion, BEFORE the deletion is issued", snap.phase, snap.cloudFolder, snap.cleanupPending, folder)
	}
	// After the deletion succeeds, the reference and the flag clear in
	// the same save as well.
	manager.mu.RLock()
	j := manager.jobs[jobID]
	final := completionSnapshot{}
	if j != nil {
		final = completionSnapshot{j.Phase, j.CloudFolder, j.CleanupPending}
	}
	manager.mu.RUnlock()
	if final.phase != "completed" || final.cloudFolder != "" || final.cleanupPending {
		t.Fatalf("row after the successful folder deletion = {phase %q folder %q cleanupPending %v}, want {completed \"\" false}", final.phase, final.cloudFolder, final.cleanupPending)
	}
	got, err := os.ReadFile(filepath.Join(j.OutputPath, "episode.mkv"))
	if err != nil {
		t.Fatalf("read imported output: %v", err)
	}
	if string(got) != payload {
		t.Fatalf("downloaded content = %q, want %q", got, payload)
	}
	if createCount.Load() != 1 {
		t.Fatalf("transfer/create calls = %d, want exactly 1", createCount.Load())
	}
	waitQuiescent(t, manager)
}
