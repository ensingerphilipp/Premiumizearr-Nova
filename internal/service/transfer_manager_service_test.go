package service

import (
	"fmt"
	"sync"
	"testing"

	"github.com/ensingerphilipp/premiumizearr-nova/pkg/premiumizeme"
)

// TestCountDownloadsActiveTopLevelJobs verifies the SimultaneousDownloads
// counting invariant: an active download is a top-level folder job from
// synchronous admission in HandleFinishedItem until the job's deferred
// removal. Every such job counts as exactly one regardless of whether it is
// listing folders, generating a link, downloading a child file, or between
// child files; transient child-file tracking must not affect the count.
func TestCountDownloadsActiveTopLevelJobs(t *testing.T) {
	m := TransferManagerService{}.New()

	// Two concurrently active top-level folder jobs: the state
	// HandleFinishedItem leaves in downloadList right after admitting both,
	// before any child-file entry exists (e.g. both jobs still listing).
	m.addDownload(&premiumizeme.Item{Name: "Show.S01"}, true)
	m.addDownload(&premiumizeme.Item{Name: "Show.S02"}, true)
	if got := m.countDownloads(); got != 2 {
		t.Fatalf("two active top-level jobs: countDownloads() = %d, want 2", got)
	}

	// A transient child-file entry (one job mid-wget) must not change the
	// count; child entries are keyed by bare file name, as in production.
	m.addDownload(&premiumizeme.Item{Name: "01.mkv"}, false)
	if got := m.countDownloads(); got != 2 {
		t.Fatalf("with transient child-file entry: countDownloads() = %d, want 2", got)
	}

	// Job completion removes exactly its own top-level entry.
	m.removeDownload("Show.S01")
	if got := m.countDownloads(); got != 1 {
		t.Fatalf("after one job completed: countDownloads() = %d, want 1", got)
	}

	// Zero-value guard: entries added without the top-level marker (all
	// transient child-file tracking) count as no active jobs.
	n := TransferManagerService{}.New()
	n.addDownload(&premiumizeme.Item{Name: "02.mkv"}, false)
	if got := n.countDownloads(); got != 0 {
		t.Fatalf("child-file entries only: countDownloads() = %d, want 0", got)
	}
}

// TestTransferManagerServiceDownloadListConcurrency exercises
// addDownload/removeDownload and countDownloads concurrently: all workers
// are released by a single barrier (no sleeps), so `go test -race` covers
// the downloadListMutex synchronization of the counting path.
func TestTransferManagerServiceDownloadListConcurrency(t *testing.T) {
	const workers = 8
	const iterations = 50

	m := TransferManagerService{}.New()

	var wg sync.WaitGroup
	release := make(chan struct{})
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			<-release
			for i := 0; i < iterations; i++ {
				name := fmt.Sprintf("worker%d-%d", w, i)
				m.addDownload(&premiumizeme.Item{Name: name}, true)
				m.countDownloads()
				m.removeDownload(name)
			}
		}(w)
	}
	close(release)
	wg.Wait()

	if got := m.countDownloads(); got != 0 {
		t.Fatalf("countDownloads() after all workers drained = %d, want 0", got)
	}
}
