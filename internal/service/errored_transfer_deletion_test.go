package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/ensingerphilipp/premiumizearr-nova/internal/arr"
	"github.com/ensingerphilipp/premiumizearr-nova/internal/config"
	"github.com/ensingerphilipp/premiumizearr-nova/pkg/premiumizeme"
)

const testErroredTransferName = "Show.S01E01.720p.WEB.x264-GRP.mkv.nzb"

// eventLog is a mutex-guarded ordered event log shared by the test fakes,
// used to assert the order of cross-fake interactions (e.g. the *arr being
// told about the failure before the premiumize.me delete happens).
type eventLog struct {
	mu     sync.Mutex
	events []string
}

func (l *eventLog) record(e string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, e)
}

func (l *eventLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.events...)
}

// countEvent returns how many times e occurs in the snapshot of the log.
func (l *eventLog) countEvent(e string) int {
	count := 0
	for _, got := range l.snapshot() {
		if got == e {
			count++
		}
	}
	return count
}

// fakeClock is the injectable test clock: tests advance time explicitly
// instead of sleeping.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// fakePremiumize is an httptest fake for the premiumize.me endpoints the
// transfer manager uses (transfer list + transfer delete). A successful
// delete removes the transfer from the list, as on premiumize.me. Delete
// calls can be made to fail a bounded number of times and to block until
// released, so a test can pin a delete in flight.
type fakePremiumize struct {
	*httptest.Server
	*premiumizeme.Premiumizeme
	events *eventLog

	mu          sync.Mutex
	transfers   []premiumizeme.Transfer
	deleted     []string
	failDeletes int                // remaining delete calls to answer with an error
	blockDelete chan chan struct{} // when non-nil, each delete handler blocks until its release channel is closed
}

func newFakePremiumize(t *testing.T, transfers []premiumizeme.Transfer) *fakePremiumize {
	t.Helper()

	fake := &fakePremiumize{transfers: transfers, events: &eventLog{}}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/transfer/list", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("unexpected %s on /api/transfer/list", r.Method)
		}
		fake.events.record("list")
		fake.mu.Lock()
		defer fake.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"status":    "success",
			"transfers": fake.transfers,
		})
	})
	mux.HandleFunc("/api/transfer/delete", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("unexpected %s on /api/transfer/delete", r.Method)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read delete request body: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		values, err := url.ParseQuery(string(body))
		if err != nil {
			t.Errorf("parse delete request body %q: %v", body, err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		id := values.Get("id")
		fake.events.record("delete:" + id)

		fake.mu.Lock()
		failed := false
		if fake.failDeletes > 0 {
			fake.failDeletes--
			failed = true
		}
		var release chan struct{}
		if fake.blockDelete != nil {
			release = make(chan struct{})
		}
		fake.mu.Unlock()

		if release != nil {
			fake.blockDelete <- release
			<-release
		}

		w.Header().Set("Content-Type", "application/json")
		if failed {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"status":  "error",
				"message": "fake premiumize delete failure",
			})
			return
		}

		fake.mu.Lock()
		fake.deleted = append(fake.deleted, id)
		remaining := make([]premiumizeme.Transfer, 0, len(fake.transfers))
		for _, transfer := range fake.transfers {
			if transfer.ID != id {
				remaining = append(remaining, transfer)
			}
		}
		fake.transfers = remaining
		fake.mu.Unlock()

		// Recorded after the list update so tests can use it as a
		// synchronization point: once "deleted:<id>" is present, the
		// delete fully completed and the transfer is gone from the list.
		fake.events.record("deleted:" + id)

		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"status":  "success",
			"message": "",
		})
	})

	fake.Server = httptest.NewServer(mux)
	t.Cleanup(fake.Close)

	pm := premiumizeme.NewPremiumizemeClient("test-api-key")
	// The client joins APIBaseURL with the endpoint path, so the fake must
	// serve under /api/ like https://www.premiumize.me/api/.
	pm.APIBaseURL = fake.Server.URL + "/api/"
	fake.Premiumizeme = &pm

	return fake
}

// setTransfers replaces the transfer list the fake serves.
func (fake *fakePremiumize) setTransfers(t *testing.T, transfers []premiumizeme.Transfer) {
	t.Helper()
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.transfers = transfers
}

// deletedIDs returns the ids successfully deleted so far.
func (fake *fakePremiumize) deletedIDs() []string {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return append([]string(nil), fake.deleted...)
}

// waitForEvent blocks until at least count occurrences of event are present
// in the fake's event log, bounded by a deadline. It is a synchronization
// barrier for the goroutines a poll cycle spawns; it does not simulate
// time (time is advanced explicitly via fakeClock).
func waitForEvent(t *testing.T, fake *fakePremiumize, event string, count int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if fake.events.countEvent(event) >= count {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d x %q; events so far: %v", count, event, fake.events.snapshot())
		}
		time.Sleep(time.Millisecond)
	}
}

// fakeArr is an in-memory arr.IArr for service tests. When lookupErr is set
// the history lookup fails (an arr outage). When hasMatch is set the
// transfer name is in the history with grabbed record ID matchedID.
// HandleErrorTransfer mirrors the real wrapper contract: the history item
// is marked failed first (recorded in the shared event log), then the
// premiumize.me transfer is deleted via the given client.
type fakeArr struct {
	name      string
	hasMatch  bool
	matchedID int64
	lookupErr error
	failErr   error
	events    *eventLog
}

func (f *fakeArr) HistoryContains(_ string) (int64, bool, error) {
	if f.lookupErr != nil {
		return -1, false, f.lookupErr
	}
	if f.hasMatch {
		return f.matchedID, true, nil
	}
	return -1, false, nil
}

func (f *fakeArr) MarkHistoryItemAsFailed(id int64) error {
	if f.failErr != nil {
		return f.failErr
	}
	f.events.record("fail:" + strconv.FormatInt(id, 10))
	return nil
}

func (f *fakeArr) HandleErrorTransfer(transfer *premiumizeme.Transfer, arrID int64, pm *premiumizeme.Premiumizeme) error {
	if err := f.MarkHistoryItemAsFailed(arrID); err != nil {
		return fmt.Errorf("failed to mark history item as failed: %w", err)
	}
	if err := pm.DeleteTransfer(transfer.ID); err != nil {
		return fmt.Errorf("failed to delete transfer from premiumize.me: %+v", err)
	}
	return nil
}

func (f *fakeArr) GetArrName() string {
	return f.name
}

// newErroredTransferTestService wires a TransferManagerService against the
// fakes with the default 5 minute grace period and the injectable clock.
func newErroredTransferTestService(t *testing.T, fake *fakePremiumize, clock *fakeClock, arrs ...arr.IArr) *TransferManagerService {
	t.Helper()

	m := TransferManagerService{}.New()
	cfg := &config.Config{
		DownloadsDirectory:                      t.TempDir(),
		ErroredTransferDeleteGracePeriodSeconds: 300,
	}
	am := ArrsManagerService{}.New()
	am.Init(cfg)
	am.arrs = arrs
	m.Init(fake.Premiumizeme, &am, cfg)
	m.nowFunc = clock.Now
	return &m
}

// trackingCount returns how many errored transfers the service currently
// tracks (test helper, same package).
func trackingCount(m *TransferManagerService) int {
	m.erroredTransfersMutex.Lock()
	defer m.erroredTransfersMutex.Unlock()
	return len(m.erroredTransfers)
}

func erroredTransfer(id string) premiumizeme.Transfer {
	return premiumizeme.Transfer{
		ID:      id,
		Name:    testErroredTransferName,
		Message: "Repair failed, not enough repair blocks (28 short)",
		Status:  "error",
	}
}

// TestErroredTransferUnmatchedRemainsDuringGracePeriod verifies that an
// errored transfer with no *arr history match is NOT deleted while the
// grace period has not elapsed: it is kept, re-checked on every poll, and
// still tracked just before expiry.
func TestErroredTransferUnmatchedRemainsDuringGracePeriod(t *testing.T) {
	fake := newFakePremiumize(t, []premiumizeme.Transfer{erroredTransfer("t1")})
	clock := newFakeClock()
	m := newErroredTransferTestService(t, fake, clock, &fakeArr{name: "Sonarr"})

	m.TaskUpdateTransfersList() // first seen at T0
	if got := fake.events.countEvent("delete:t1"); got != 0 {
		t.Fatalf("delete attempts after first poll = %d, want 0 (transfer is within the grace period)", got)
	}
	if got := trackingCount(m); got != 1 {
		t.Fatalf("tracking count after first poll = %d, want 1", got)
	}

	// Just before the 5 minute grace period expires.
	clock.Advance(4*time.Minute + 59*time.Second)
	m.TaskUpdateTransfersList()
	if got := fake.events.countEvent("delete:t1"); got != 0 {
		t.Fatalf("delete attempts just before grace expiry = %d, want 0", got)
	}
	if got := trackingCount(m); got != 1 {
		t.Fatalf("tracking count just before grace expiry = %d, want 1", got)
	}
}

// TestErroredTransferUnmatchedDeletedAfterGraceExpiry verifies that an
// errored transfer that stayed unmatched is deleted from premiumize.me once
// the grace period has elapsed, and that its tracking state is cleared
// afterwards.
func TestErroredTransferUnmatchedDeletedAfterGraceExpiry(t *testing.T) {
	fake := newFakePremiumize(t, []premiumizeme.Transfer{erroredTransfer("t1")})
	clock := newFakeClock()
	m := newErroredTransferTestService(t, fake, clock, &fakeArr{name: "Sonarr"})

	m.TaskUpdateTransfersList() // first seen at T0
	clock.Advance(5 * time.Minute)
	m.TaskUpdateTransfersList() // grace elapsed: delete is spawned
	waitForEvent(t, fake, "deleted:t1", 1)

	if got := fake.events.countEvent("delete:t1"); got != 1 {
		t.Fatalf("delete attempts after grace expiry = %d, want exactly 1", got)
	}
	if ids := fake.deletedIDs(); len(ids) != 1 || ids[0] != "t1" {
		t.Fatalf("deleted ids = %v, want [t1]", ids)
	}

	// The successful delete removed the transfer from the list; the next
	// poll runs synchronously and must clear the tracking state without
	// another delete attempt.
	m.TaskUpdateTransfersList()
	if got := trackingCount(m); got != 0 {
		t.Fatalf("tracking count after deletion = %d, want 0", got)
	}
	if got := fake.events.countEvent("delete:t1"); got != 1 {
		t.Fatalf("delete attempts after tracking cleanup = %d, want still 1", got)
	}
}

// TestErroredTransferLookupFailureNeverDeletes verifies that *arr history
// lookup failures (an arr outage) are never treated as an authoritative
// no-match: no matter how long the outage lasts, the transfer is kept and
// never deleted.
func TestErroredTransferLookupFailureNeverDeletes(t *testing.T) {
	fake := newFakePremiumize(t, []premiumizeme.Transfer{erroredTransfer("t1")})
	clock := newFakeClock()
	m := newErroredTransferTestService(t, fake, clock,
		&fakeArr{name: "Sonarr", lookupErr: errors.New("sonarr unreachable")},
		&fakeArr{name: "Radarr", lookupErr: errors.New("radarr unreachable")},
	)

	m.TaskUpdateTransfersList() // first seen at T0
	for i := 0; i < 3; i++ {
		clock.Advance(24 * time.Hour) // far beyond the grace period
		m.TaskUpdateTransfersList()
	}

	if got := fake.events.countEvent("delete:t1"); got != 0 {
		t.Fatalf("delete attempts after lookup failures = %d, want 0: an arr outage must never cause automatic deletion", got)
	}
	if ids := fake.deletedIDs(); len(ids) != 0 {
		t.Fatalf("deleted ids = %v, want none", ids)
	}
	if got := trackingCount(m); got != 1 {
		t.Fatalf("tracking count after lookup failures = %d, want 1 (still waiting for a reachable *arr)", got)
	}
}

// TestErroredTransferMatchedReportedFailedBeforeDelete verifies the
// reporting order for a matched errored transfer: the *arr history item is
// marked failed FIRST, and only then is the premiumize.me transfer
// deleted, exactly once.
func TestErroredTransferMatchedReportedFailedBeforeDelete(t *testing.T) {
	fake := newFakePremiumize(t, []premiumizeme.Transfer{erroredTransfer("t1")})
	clock := newFakeClock()
	m := newErroredTransferTestService(t, fake, clock,
		&fakeArr{name: "Sonarr", hasMatch: true, matchedID: 101, events: fake.events},
	)

	m.TaskUpdateTransfersList() // match: report + delete are spawned
	waitForEvent(t, fake, "deleted:t1", 1)

	events := fake.events.snapshot()
	failIndex, deleteIndex := -1, -1
	for i, e := range events {
		if e == "fail:101" {
			failIndex = i
		}
		if e == "delete:t1" {
			deleteIndex = i
		}
	}
	if failIndex == -1 || deleteIndex == -1 {
		t.Fatalf("events = %v, want both a fail:101 (the *arr was told) and a delete:t1", events)
	}
	if failIndex > deleteIndex {
		t.Fatalf("events = %v, want the *arr history item failed BEFORE the transfer was deleted", events)
	}
	if got := fake.events.countEvent("fail:101"); got != 1 {
		t.Fatalf("failure reports = %d, want exactly 1", got)
	}
	if got := fake.events.countEvent("delete:t1"); got != 1 {
		t.Fatalf("delete attempts = %d, want exactly 1", got)
	}

	m.TaskUpdateTransfersList() // the deleted transfer is gone from the list
	if got := trackingCount(m); got != 0 {
		t.Fatalf("tracking count after successful match handling = %d, want 0", got)
	}
}

// TestErroredTransferDeleteFailureRetriedNextPoll verifies that a failed
// deletion of an unmatched errored transfer keeps its tracking state and is
// retried on the next poll after expiry, until the delete succeeds.
func TestErroredTransferDeleteFailureRetriedNextPoll(t *testing.T) {
	fake := newFakePremiumize(t, []premiumizeme.Transfer{erroredTransfer("t1")})
	clock := newFakeClock()
	m := newErroredTransferTestService(t, fake, clock, &fakeArr{name: "Sonarr"})

	fake.mu.Lock()
	fake.failDeletes = 2 // the first two delete attempts fail
	fake.mu.Unlock()

	m.TaskUpdateTransfersList() // first seen at T0
	clock.Advance(5 * time.Minute)

	m.TaskUpdateTransfersList() // attempt 1: fails
	waitForEvent(t, fake, "delete:t1", 1)
	if got := trackingCount(m); got != 1 {
		t.Fatalf("tracking count after failed delete = %d, want 1 (must be retried)", got)
	}

	m.TaskUpdateTransfersList() // attempt 2: fails
	waitForEvent(t, fake, "delete:t1", 2)
	if got := trackingCount(m); got != 1 {
		t.Fatalf("tracking count after second failed delete = %d, want 1 (must be retried)", got)
	}

	fake.mu.Lock()
	fake.failDeletes = 0 // deletes succeed from now on
	fake.mu.Unlock()

	m.TaskUpdateTransfersList() // attempt 3: succeeds
	waitForEvent(t, fake, "deleted:t1", 1)
	if got := fake.events.countEvent("delete:t1"); got != 3 {
		t.Fatalf("total delete attempts = %d, want 3", got)
	}
	if ids := fake.deletedIDs(); len(ids) != 1 || ids[0] != "t1" {
		t.Fatalf("deleted ids = %v, want [t1]", ids)
	}

	m.TaskUpdateTransfersList() // deleted: gone from the list, tracking cleared
	if got := trackingCount(m); got != 0 {
		t.Fatalf("tracking count after successful delete = %d, want 0", got)
	}
	if got := fake.events.countEvent("delete:t1"); got != 3 {
		t.Fatalf("delete attempts after tracking cleanup = %d, want still 3", got)
	}
}

// TestErroredTransferRepeatedPollingNoDuplicates verifies the per-transfer
// processing guard: while one report/delete is in flight, repeated polls
// must not spawn a second one, so the *arr failure report and the
// premiumize.me delete each happen exactly once.
func TestErroredTransferRepeatedPollingNoDuplicates(t *testing.T) {
	fake := newFakePremiumize(t, []premiumizeme.Transfer{erroredTransfer("t1")})
	clock := newFakeClock()
	m := newErroredTransferTestService(t, fake, clock,
		&fakeArr{name: "Sonarr", hasMatch: true, matchedID: 101, events: fake.events},
	)

	// Pin the in-flight delete: the fake blocks until the release channel
	// is closed, so the report goroutine holds the processing slot.
	fake.mu.Lock()
	fake.blockDelete = make(chan chan struct{})
	fake.mu.Unlock()

	m.TaskUpdateTransfersList() // poll 1: spawns the report, delete blocks in flight
	release := <-fake.blockDelete
	m.TaskUpdateTransfersList() // poll 2 while in flight: must be skipped
	m.TaskUpdateTransfersList() // poll 3 while in flight: must be skipped
	close(release)

	waitForEvent(t, fake, "deleted:t1", 1)
	if got := fake.events.countEvent("fail:101"); got != 1 {
		t.Fatalf("failure reports = %d, want exactly 1 despite repeated polling", got)
	}
	if got := fake.events.countEvent("delete:t1"); got != 1 {
		t.Fatalf("delete attempts = %d, want exactly 1 despite repeated polling", got)
	}
	if ids := fake.deletedIDs(); len(ids) != 1 || ids[0] != "t1" {
		t.Fatalf("deleted ids = %v, want [t1]", ids)
	}

	m.TaskUpdateTransfersList() // deleted: gone from the list, tracking cleared
	if got := trackingCount(m); got != 0 {
		t.Fatalf("tracking count after completion = %d, want 0", got)
	}
}

// TestErroredTransferSearchesAllArrs verifies that ALL configured *arr
// instances are searched while the grace period runs: a transfer that only
// matches in the second arr is still handled immediately on the first poll.
func TestErroredTransferSearchesAllArrs(t *testing.T) {
	fake := newFakePremiumize(t, []premiumizeme.Transfer{erroredTransfer("t1")})
	clock := newFakeClock()
	m := newErroredTransferTestService(t, fake, clock,
		&fakeArr{name: "Sonarr"}, // no match
		&fakeArr{name: "Radarr", hasMatch: true, matchedID: 201, events: fake.events},
	)

	m.TaskUpdateTransfersList()
	waitForEvent(t, fake, "deleted:t1", 1)

	if got := fake.events.countEvent("fail:201"); got != 1 {
		t.Fatalf("failure reports to the matching (second) arr = %d, want 1", got)
	}
	if got := fake.events.countEvent("delete:t1"); got != 1 {
		t.Fatalf("delete attempts = %d, want 1", got)
	}

	m.TaskUpdateTransfersList()
	if got := trackingCount(m); got != 0 {
		t.Fatalf("tracking count after successful match handling = %d, want 0", got)
	}
}

// TestErroredTrackingClearedWhenNotErroredOrGone verifies that tracking
// state is cleared when a transfer stops being errored (a fresh grace
// period applies if it errors again) and when it disappears from the
// premiumize.me transfer list.
func TestErroredTrackingClearedWhenNotErroredOrGone(t *testing.T) {
	fake := newFakePremiumize(t, []premiumizeme.Transfer{erroredTransfer("t1")})
	clock := newFakeClock()
	m := newErroredTransferTestService(t, fake, clock, &fakeArr{name: "Sonarr"})

	m.TaskUpdateTransfersList()
	if got := trackingCount(m); got != 1 {
		t.Fatalf("tracking count after first poll = %d, want 1", got)
	}

	// The transfer stops being errored: tracking must be cleared
	// immediately (synchronous), so a later re-error starts a fresh grace
	// period instead of deleting right away.
	done := erroredTransfer("t1")
	done.Status = "completed"
	fake.setTransfers(t, []premiumizeme.Transfer{done})
	m.TaskUpdateTransfersList()
	if got := trackingCount(m); got != 0 {
		t.Fatalf("tracking count after the transfer stopped being errored = %d, want 0", got)
	}

	errored := erroredTransfer("t1")
	fake.setTransfers(t, []premiumizeme.Transfer{errored})
	m.TaskUpdateTransfersList()
	if got := trackingCount(m); got != 1 {
		t.Fatalf("tracking count after the transfer errored again = %d, want 1", got)
	}
	// Fresh first-seen time: the full grace period has elapsed since T0,
	// yet no delete may happen yet.
	clock.Advance(10 * time.Minute)
	m.TaskUpdateTransfersList()
	if got := fake.events.countEvent("delete:t1"); got != 0 {
		t.Fatalf("delete attempts after re-error = %d, want 0 (fresh grace period after clearing)", got)
	}

	// The transfer disappears from the list: tracking must be pruned.
	fake.setTransfers(t, nil)
	m.TaskUpdateTransfersList()
	if got := trackingCount(m); got != 0 {
		t.Fatalf("tracking count after the transfer disappeared = %d, want 0", got)
	}
}

// TestErroredTransferGracePeriodFromConfig verifies that the grace period
// is read from the config: a 60 second grace period deletes the transfer
// after 60 seconds, and an unset (zero) value falls back to the 5 minute
// default instead of deleting immediately.
func TestErroredTransferGracePeriodFromConfig(t *testing.T) {
	// Custom 60 second grace period.
	fake := newFakePremiumize(t, []premiumizeme.Transfer{erroredTransfer("t1")})
	clock := newFakeClock()
	m := TransferManagerService{}.New()
	cfg := &config.Config{
		DownloadsDirectory:                      t.TempDir(),
		ErroredTransferDeleteGracePeriodSeconds: 60,
	}
	am := ArrsManagerService{}.New()
	am.Init(cfg)
	am.arrs = []arr.IArr{&fakeArr{name: "Sonarr"}}
	m.Init(fake.Premiumizeme, &am, cfg)
	m.nowFunc = clock.Now

	m.TaskUpdateTransfersList() // first seen at T0
	clock.Advance(59 * time.Second)
	m.TaskUpdateTransfersList()
	if got := fake.events.countEvent("delete:t1"); got != 0 {
		t.Fatalf("delete attempts at 59s with a 60s grace period = %d, want 0", got)
	}
	clock.Advance(2 * time.Second) // 61s > 60s
	m.TaskUpdateTransfersList()
	waitForEvent(t, fake, "deleted:t1", 1)
	if ids := fake.deletedIDs(); len(ids) != 1 || ids[0] != "t1" {
		t.Fatalf("deleted ids = %v, want [t1]", ids)
	}

	// Unset (zero) grace period: the 5 minute default applies, so no
	// immediate deletion.
	fake2 := newFakePremiumize(t, []premiumizeme.Transfer{erroredTransfer("t2")})
	clock2 := newFakeClock()
	m2 := TransferManagerService{}.New()
	cfg2 := &config.Config{DownloadsDirectory: t.TempDir()}
	am2 := ArrsManagerService{}.New()
	am2.Init(cfg2)
	am2.arrs = []arr.IArr{&fakeArr{name: "Sonarr"}}
	m2.Init(fake2.Premiumizeme, &am2, cfg2)
	m2.nowFunc = clock2.Now

	m2.TaskUpdateTransfersList()
	if got := fake2.events.countEvent("delete:t2"); got != 0 {
		t.Fatalf("delete attempts with an unset grace period = %d, want 0 (default 5 minutes must apply, not immediate deletion)", got)
	}
	clock2.Advance(4*time.Minute + 59*time.Second)
	m2.TaskUpdateTransfersList()
	if got := fake2.events.countEvent("delete:t2"); got != 0 {
		t.Fatalf("delete attempts just before the 5 minute default expired = %d, want 0", got)
	}
	clock2.Advance(2 * time.Second)
	m2.TaskUpdateTransfersList()
	waitForEvent(t, fake2, "deleted:t2", 1)
	if ids := fake2.deletedIDs(); len(ids) != 1 || ids[0] != "t2" {
		t.Fatalf("deleted ids = %v, want [t2]", ids)
	}
}
