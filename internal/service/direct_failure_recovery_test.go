package service

import (
	"errors"
	"testing"
	"time"

	"github.com/ensingerphilipp/premiumizearr-nova/internal/directclient"
	"github.com/ensingerphilipp/premiumizearr-nova/pkg/premiumizeme"
)

type recoveringFailureArr struct {
	unavailable    bool
	queries, marks int
}

func (f *recoveringFailureArr) HistoryContains(string) (int64, bool, error) {
	return 0, false, nil
}
func (f *recoveringFailureArr) HistoryContainsFresh(string) (int64, bool, error) {
	return 0, false, nil
}
func (f *recoveringFailureArr) HistoryContainsDownloadIDFresh(string) (int64, bool, error) {
	f.queries++
	if f.unavailable {
		return 0, false, errors.New("temporary connection failure")
	}
	return 7, true, nil
}
func (f *recoveringFailureArr) MarkHistoryItemAsFailed(int64) error { f.marks++; return nil }
func (f *recoveringFailureArr) HandleErrorTransfer(*premiumizeme.Transfer, int64, *premiumizeme.Premiumizeme) error {
	return nil
}
func (f *recoveringFailureArr) GetArrName() string { return "sonarr" }

func TestFailureReporterRecoversAfterOutage(t *testing.T) {
	f := &recoveringFailureArr{unavailable: true}
	a := ArrsManagerService{}.New()
	now := time.Unix(1000, 0)
	a.failureReportNow = func() time.Time { return now }
	a.failureTargets = []directFailureTarget{{key: "sonarr", client: f}}
	job := directclient.Job{ID: "same-download-id"}
	for i := 0; i < 2; i++ {
		if _, err := a.ReportDirectTorrentFailure(job); err == nil {
			t.Fatal("outage was not observed")
		}
	}
	f.unavailable = false
	var acks []string
	for i := 0; i < 8; i++ {
		now = now.Add(15 * time.Second)
		got, err := a.ReportDirectTorrentFailure(job)
		if err != nil {
			t.Fatal(err)
		}
		acks = append(acks, got...)
		job.ReportedFailures = append(job.ReportedFailures, got...)
	}
	t.Logf("after recovery: queries=%d marks=%d acknowledgements=%v", f.queries, f.marks, acks)
	if len(acks) != 1 || f.marks != 1 {
		t.Fatal("a temporary two-pass outage permanently disabled reporting to the recovered target")
	}
}

func TestFailureReporterBoundsHealthyMissesDuringLongOutage(t *testing.T) {
	untracked := &round4FakeArr{name: "untracked", noHistory: true}
	broken := &round4FakeArr{name: "offline", permanentError: true}
	a := ArrsManagerService{}.New()
	a.failureTargets = []directFailureTarget{{key: "untracked", client: untracked}, {key: "offline", client: broken}}
	now := time.Unix(1000, 0)
	a.failureReportNow = func() time.Time { return now }
	job := directclient.Job{ID: "download", Created: now}
	for pass := 0; pass < 100; pass++ {
		if _, err := a.ReportDirectTorrentFailure(job); err == nil {
			t.Fatal("an offline target consumed the no-error report budget")
		}
		now = now.Add(15 * time.Second)
	}
	if calls := untracked.queryCalls.Load(); calls != directFailureHistoryPollCap {
		t.Fatalf("healthy unmatched target fetched history %d times, want %d", calls, directFailureHistoryPollCap)
	}
	if calls := broken.queryCalls.Load(); calls >= 15 {
		t.Fatalf("offline target did not back off: %d queries", calls)
	}
	broken.permanentError = false
	now = now.Add(5 * time.Minute)
	acks, err := a.ReportDirectTorrentFailure(job)
	if err != nil || len(acks) != 1 || acks[0] != "offline" {
		t.Fatalf("long outage did not recover: acknowledgements=%v err=%v", acks, err)
	}
	// Re-grabbing the same torrent must reset the healthy target's budget.
	job.Attempt++
	untracked.noHistory = false
	acks, err = a.ReportDirectTorrentFailure(job)
	if err != nil || !round4ContainsReport(acks, "untracked") {
		t.Fatalf("new attempt inherited the previous lookup budget: %v, %v", acks, err)
	}
	// Removing and re-adding also starts a new job with attempt zero.
	job.Attempt = 0
	job.Created = now
	acks, err = a.ReportDirectTorrentFailure(job)
	if err != nil || !round4ContainsReport(acks, "untracked") {
		t.Fatalf("fresh job inherited the removed job's retry state: %v, %v", acks, err)
	}
}
