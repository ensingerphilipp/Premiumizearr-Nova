package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
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
// released, so a test can pin a delete in flight (per transfer ID, so one
// pinned delete does not hold up the others); the list call can be made
// to fail a bounded number of times as well.
type fakePremiumize struct {
	*httptest.Server
	*premiumizeme.Premiumizeme
	events *eventLog

	mu             sync.Mutex
	transfers      []premiumizeme.Transfer
	deleted        []string
	failDeletes    int                      // remaining delete calls to answer with an error
	failList       int                      // remaining list calls to answer with an error
	blockedDeletes map[string]chan struct{} // when non-nil, each delete handler for a listed id blocks until its release channel is closed
}

// blockDeletes enables per-ID delete pinning: every subsequent delete
// handler registers its release channel in blockedDeletes and waits on it
// before applying the delete.
func (fake *fakePremiumize) blockDeletes() {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.blockedDeletes == nil {
		fake.blockedDeletes = make(map[string]chan struct{})
	}
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
		if fake.failList > 0 {
			fake.failList--
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"status":  "error",
				"message": "fake premiumize list failure",
			})
			return
		}
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
		if fake.blockedDeletes != nil {
			release = make(chan struct{})
			fake.blockedDeletes[id] = release
		}
		fake.mu.Unlock()

		if release != nil {
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

// awaitBlockedDelete blocks until the fake's delete handler for id has
// started and is pinned by its release channel, then returns that channel;
// closing it releases the handler.
func (fake *fakePremiumize) awaitBlockedDelete(t *testing.T, id string) chan struct{} {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		fake.mu.Lock()
		release := fake.blockedDeletes[id]
		fake.mu.Unlock()
		if release != nil {
			return release
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for the delete of %s to start", id)
		}
		time.Sleep(time.Millisecond)
	}
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

// waitForProcessing blocks until every in-flight report/delete goroutine
// spawned by the polls so far has fully finished (state settled), so a
// test asserts on the settled tracking state instead of racing the
// goroutines. Bounded by a deadline so a stuck goroutine fails the test
// instead of hanging it.
func waitForProcessing(t *testing.T, m *TransferManagerService) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		m.processingWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for in-flight errored transfer processing to finish")
	}
}

// fakeArr is an in-memory arr.IArr for service tests. When lookupErr is set
// the history lookup fails (an arr outage). When hasMatch is set, every
// transfer name is in the history with grabbed record ID matchedID;
// matchByName scopes the match to specific names (checked first). When
// freshMatch is set, only the fresh lookup (a forced history refetch)
// reports the match: the cached lookup is older than a recent grab. When
// freshErr is set, only the fresh lookup fails (a forced history refetch
// going down). HandleErrorTransfer mirrors the real wrapper contract: the
// history item is marked failed first (recorded in the shared event log),
// then the premiumize.me transfer is deleted via the given client.
type fakeArr struct {
	name        string
	hasMatch    bool
	matchedID   int64
	matchByName map[string]int64
	lookupErr   error
	freshErr    error
	failErr     error
	freshMatch  bool
	events      *eventLog
}

func (f *fakeArr) HistoryContains(name string) (int64, bool, error) {
	if f.lookupErr != nil {
		return -1, false, f.lookupErr
	}
	if id, ok := f.matchByName[name]; ok {
		return id, true, nil
	}
	if f.hasMatch {
		return f.matchedID, true, nil
	}
	return -1, false, nil
}

func (f *fakeArr) HistoryContainsFresh(name string) (int64, bool, error) {
	if f.events != nil {
		f.events.record("fresh:" + f.name)
	}
	if f.freshErr != nil {
		return -1, false, f.freshErr
	}
	if f.freshMatch {
		return f.matchedID, true, nil
	}
	return f.HistoryContains(name)
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
	return newErroredTransferTestServiceWithGrace(t, fake, clock, 300, arrs...)
}

// newErroredTransferTestServiceWithGrace wires a TransferManagerService
// against the fakes with an explicit grace period (seconds) in the config
// and the injectable clock. A cleanup hook waits for in-flight
// report/delete goroutines before the fakes are torn down, so no
// goroutine can outlive the fake servers.
func newErroredTransferTestServiceWithGrace(t *testing.T, fake *fakePremiumize, clock *fakeClock, graceSeconds int, arrs ...arr.IArr) *TransferManagerService {
	t.Helper()

	m := TransferManagerService{}.New()
	cfg := &config.Config{
		DownloadsDirectory:                      t.TempDir(),
		ErroredTransferDeleteGracePeriodSeconds: graceSeconds,
	}
	am := ArrsManagerService{}.New()
	am.Init(cfg)
	am.arrs = arrs
	m.Init(fake.Premiumizeme, &am, cfg)
	m.nowFunc = clock.Now
	t.Cleanup(func() { waitForProcessing(t, &m) })
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
	waitForProcessing(t, m)

	// The tracking state must already be gone before any further poll:
	// the successful delete settled it itself, it must not rely on the
	// next poll's prune.
	if got := trackingCount(m); got != 0 {
		t.Fatalf("tracking count right after the successful delete = %d, want 0 (settled by the success path, not a later poll)", got)
	}

	if got := fake.events.countEvent("delete:t1"); got != 1 {
		t.Fatalf("delete attempts after grace expiry = %d, want exactly 1", got)
	}
	if ids := fake.deletedIDs(); len(ids) != 1 || ids[0] != "t1" {
		t.Fatalf("deleted ids = %v, want [t1]", ids)
	}

	// The successful delete removed the transfer from the list; the next
	// poll runs synchronously and must not start another delete attempt.
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
	waitForProcessing(t, m)

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
	// The tracking state must already be gone before any further poll:
	// the successful report/delete settled it itself, it must not rely on
	// the next poll's prune.
	if got := trackingCount(m); got != 0 {
		t.Fatalf("tracking count right after the successful report/delete = %d, want 0 (settled by the success path, not a later poll)", got)
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
	waitForProcessing(t, m)
	if got := trackingCount(m); got != 1 {
		t.Fatalf("tracking count after failed delete = %d, want 1 (must be retried)", got)
	}

	m.TaskUpdateTransfersList() // attempt 2: fails
	waitForEvent(t, fake, "delete:t1", 2)
	waitForProcessing(t, m)
	if got := trackingCount(m); got != 1 {
		t.Fatalf("tracking count after second failed delete = %d, want 1 (must be retried)", got)
	}

	fake.mu.Lock()
	fake.failDeletes = 0 // deletes succeed from now on
	fake.mu.Unlock()

	m.TaskUpdateTransfersList() // attempt 3: succeeds
	waitForEvent(t, fake, "deleted:t1", 1)
	waitForProcessing(t, m)
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
	fake.blockDeletes()

	m.TaskUpdateTransfersList() // poll 1: spawns the report, delete blocks in flight
	release := fake.awaitBlockedDelete(t, "t1")
	m.TaskUpdateTransfersList() // poll 2 while in flight: must be skipped
	m.TaskUpdateTransfersList() // poll 3 while in flight: must be skipped
	close(release)

	waitForEvent(t, fake, "deleted:t1", 1)
	waitForProcessing(t, m)
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
	waitForProcessing(t, m)

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
// state is cleared when a transfer stops being errored, and that a
// cleared-then-re-errored transfer starts a fresh grace period that
// expires inclusively at its own boundary (deletion IS the correct
// outcome there).
func TestErroredTrackingClearedWhenNotErroredOrGone(t *testing.T) {
	fake := newFakePremiumize(t, []premiumizeme.Transfer{erroredTransfer("t1")})
	clock := newFakeClock()
	m := newErroredTransferTestService(t, fake, clock, &fakeArr{name: "Sonarr"})

	m.TaskUpdateTransfersList() // first seen at T0
	if got := trackingCount(m); got != 1 {
		t.Fatalf("tracking count after first poll = %d, want 1", got)
	}

	// The transfer stops being errored: tracking must be cleared
	// immediately (synchronous), so a later re-error starts a fresh grace
	// period instead of inheriting T0.
	done := erroredTransfer("t1")
	done.Status = "completed"
	fake.setTransfers(t, []premiumizeme.Transfer{done})
	m.TaskUpdateTransfersList()
	if got := trackingCount(m); got != 0 {
		t.Fatalf("tracking count after the transfer stopped being errored = %d, want 0", got)
	}

	errored := erroredTransfer("t1")
	fake.setTransfers(t, []premiumizeme.Transfer{errored})
	m.TaskUpdateTransfersList() // re-errored at T0: fresh first-seen time
	if got := trackingCount(m); got != 1 {
		t.Fatalf("tracking count after the transfer errored again = %d, want 1", got)
	}

	// Fresh first-seen time: still within the fresh grace period, so no
	// delete may happen yet.
	clock.Advance(4*time.Minute + 59*time.Second)
	m.TaskUpdateTransfersList()
	if got := fake.events.countEvent("delete:t1"); got != 0 {
		t.Fatalf("delete attempts within the fresh grace period = %d, want 0", got)
	}
	if got := trackingCount(m); got != 1 {
		t.Fatalf("tracking count within the fresh grace period = %d, want 1", got)
	}

	// At exactly the fresh grace boundary the grace period has elapsed
	// (expiry is inclusive at the boundary), so the unmatched transfer is
	// deleted now.
	clock.Advance(1 * time.Second)
	m.TaskUpdateTransfersList()
	waitForEvent(t, fake, "deleted:t1", 1)
	waitForProcessing(t, m)
	if ids := fake.deletedIDs(); len(ids) != 1 || ids[0] != "t1" {
		t.Fatalf("deleted ids = %v, want [t1] at the fresh grace boundary", ids)
	}

	// The deletion removed the transfer from the list: the next poll
	// clears the tracking state.
	m.TaskUpdateTransfersList()
	if got := trackingCount(m); got != 0 {
		t.Fatalf("tracking count after deletion = %d, want 0", got)
	}
}

// TestErroredTrackingPrunedWhenGoneFromList verifies that tracking state
// is pruned when an errored transfer disappears from the premiumize.me
// transfer list while still inside its grace period.
func TestErroredTrackingPrunedWhenGoneFromList(t *testing.T) {
	fake := newFakePremiumize(t, []premiumizeme.Transfer{erroredTransfer("t1")})
	clock := newFakeClock()
	m := newErroredTransferTestService(t, fake, clock, &fakeArr{name: "Sonarr"})

	m.TaskUpdateTransfersList()
	if got := trackingCount(m); got != 1 {
		t.Fatalf("tracking count after first poll = %d, want 1", got)
	}

	// The transfer disappears from the list: tracking must be pruned and
	// nothing may be deleted.
	fake.setTransfers(t, nil)
	m.TaskUpdateTransfersList()
	if got := trackingCount(m); got != 0 {
		t.Fatalf("tracking count after the transfer disappeared = %d, want 0 (pruned)", got)
	}
	if got := fake.events.countEvent("delete:t1"); got != 0 {
		t.Fatalf("delete attempts = %d, want 0 (the transfer is gone, nothing to delete)", got)
	}
}

// TestErroredTransferWithoutAnyArrNeverDeletes verifies the regression
// guard for a configuration with no *arr clients: there is nothing to
// verify the transfer against and nothing to notify about the failed
// download, so an errored transfer is kept (and tracked) forever instead
// of being silently auto-deleted after the grace period.
func TestErroredTransferWithoutAnyArrNeverDeletes(t *testing.T) {
	fake := newFakePremiumize(t, []premiumizeme.Transfer{erroredTransfer("t1")})
	clock := newFakeClock()
	m := newErroredTransferTestService(t, fake, clock) // no arr clients

	m.TaskUpdateTransfersList()
	for i := 0; i < 3; i++ {
		clock.Advance(24 * time.Hour) // far beyond the grace period
		m.TaskUpdateTransfersList()
	}

	if got := fake.events.countEvent("delete:t1"); got != 0 {
		t.Fatalf("delete attempts without any *arr = %d, want 0 (nothing to verify or notify)", got)
	}
	if ids := fake.deletedIDs(); len(ids) != 0 {
		t.Fatalf("deleted ids = %v, want none", ids)
	}
	if got := trackingCount(m); got != 1 {
		t.Fatalf("tracking count without any *arr = %d, want 1 (kept, not deleted)", got)
	}
}

// TestErroredTransferStaleNoMatchNotDeleted verifies that the deletion
// decision does not trust the stale per-arr history cache: when the cached
// lookup says "no match" but a fresh history fetch finds the grab, the
// transfer is reported to the arr (fail first) and only then deleted -
// never silently deleted as unmatched.
func TestErroredTransferStaleNoMatchNotDeleted(t *testing.T) {
	fake := newFakePremiumize(t, []premiumizeme.Transfer{erroredTransfer("t1")})
	clock := newFakeClock()
	m := newErroredTransferTestService(t, fake, clock,
		&fakeArr{name: "Sonarr", freshMatch: true, matchedID: 101, events: fake.events},
	)

	m.TaskUpdateTransfersList() // cached lookup: no match, within grace
	if got := fake.events.countEvent("delete:t1"); got != 0 {
		t.Fatalf("delete attempts within the grace period = %d, want 0", got)
	}

	clock.Advance(5 * time.Minute)
	m.TaskUpdateTransfersList() // grace expired: the fresh lookup finds the grab
	waitForEvent(t, fake, "deleted:t1", 1)
	waitForProcessing(t, m)

	if got := fake.events.countEvent("fresh:Sonarr"); got != 1 {
		t.Fatalf("fresh lookups before the deletion decision = %d, want exactly 1", got)
	}
	if got := fake.events.countEvent("fail:101"); got != 1 {
		t.Fatalf("failure reports = %d, want 1 (the arr must be told about the failed download)", got)
	}
	events := fake.events.snapshot()
	freshIndex, failIndex, deleteIndex := -1, -1, -1
	for i, e := range events {
		switch e {
		case "fresh:Sonarr":
			freshIndex = i
		case "fail:101":
			failIndex = i
		case "delete:t1":
			deleteIndex = i
		}
	}
	if freshIndex < 0 || failIndex < 0 || deleteIndex < 0 || freshIndex > failIndex || failIndex > deleteIndex {
		t.Fatalf("events = %v, want the fresh lookup BEFORE the failure report BEFORE the delete", events)
	}
	if ids := fake.deletedIDs(); len(ids) != 1 || ids[0] != "t1" {
		t.Fatalf("deleted ids = %v, want [t1]", ids)
	}
}

// TestErroredTrackingSurvivesTransferListFailure verifies that a failed
// premiumize.me transfer-list poll does not lose grace-period tracking
// state: the poll returns early on the error, and the next successful poll
// continues from the same first-seen time.
func TestErroredTrackingSurvivesTransferListFailure(t *testing.T) {
	fake := newFakePremiumize(t, []premiumizeme.Transfer{erroredTransfer("t1")})
	clock := newFakeClock()
	m := newErroredTransferTestService(t, fake, clock, &fakeArr{name: "Sonarr"})

	m.TaskUpdateTransfersList() // first seen at T0
	if got := trackingCount(m); got != 1 {
		t.Fatalf("tracking count after first poll = %d, want 1", got)
	}

	fake.mu.Lock()
	fake.failList = 1 // the next transfer-list call answers with an error
	fake.mu.Unlock()

	m.TaskUpdateTransfersList() // list fails: the poll aborts before processing anything
	if got := fake.events.countEvent("delete:t1"); got != 0 {
		t.Fatalf("delete attempts after a failed list poll = %d, want 0", got)
	}
	if got := trackingCount(m); got != 1 {
		t.Fatalf("tracking count after a failed list poll = %d, want 1 (tracking must survive the error)", got)
	}

	// The next successful poll is still inside the grace period (same
	// first-seen time, only 2 minutes later) and must not delete.
	clock.Advance(2 * time.Minute)
	m.TaskUpdateTransfersList()
	if got := fake.events.countEvent("delete:t1"); got != 0 {
		t.Fatalf("delete attempts after the list recovered = %d, want 0 (still within the grace period)", got)
	}
	if got := trackingCount(m); got != 1 {
		t.Fatalf("tracking count after the list recovered = %d, want 1", got)
	}
}

// TestErroredTransferReportFailureRetriedNextPoll verifies that a failed
// *arr failure report (HandleErrorTransfer error) keeps the tracking state
// and is retried on the next poll, and that a successful retry reports the
// failure exactly once and deletes the transfer.
func TestErroredTransferReportFailureRetriedNextPoll(t *testing.T) {
	fake := newFakePremiumize(t, []premiumizeme.Transfer{erroredTransfer("t1")})
	clock := newFakeClock()
	sonarr := &fakeArr{name: "Sonarr", hasMatch: true, matchedID: 101, events: fake.events}
	m := newErroredTransferTestService(t, fake, clock, sonarr)

	sonarr.failErr = errors.New("sonarr unavailable")
	m.TaskUpdateTransfersList() // report spawned, fails in the *arr
	waitForProcessing(t, m)
	if got := fake.events.countEvent("fail:101"); got != 0 {
		t.Fatalf("failure reports after a failed report = %d, want 0", got)
	}
	if got := fake.events.countEvent("delete:t1"); got != 0 {
		t.Fatalf("delete attempts after a failed report = %d, want 0 (the arr was never told)", got)
	}
	if got := trackingCount(m); got != 1 {
		t.Fatalf("tracking count after a failed report = %d, want 1 (must be retried)", got)
	}

	sonarr.failErr = nil
	m.TaskUpdateTransfersList() // retry: report + delete
	waitForEvent(t, fake, "deleted:t1", 1)
	waitForProcessing(t, m)
	if got := fake.events.countEvent("fail:101"); got != 1 {
		t.Fatalf("failure reports after the retry = %d, want exactly 1", got)
	}
	if got := fake.events.countEvent("delete:t1"); got != 1 {
		t.Fatalf("delete attempts after the retry = %d, want exactly 1", got)
	}
	if ids := fake.deletedIDs(); len(ids) != 1 || ids[0] != "t1" {
		t.Fatalf("deleted ids = %v, want [t1]", ids)
	}

	m.TaskUpdateTransfersList() // deleted: gone from the list, tracking cleared
	if got := trackingCount(m); got != 0 {
		t.Fatalf("tracking count after the successful retry = %d, want 0", got)
	}
}

// TestErroredTransferMixedLookupFailureNeverDeletes verifies the mixed
// case: with one *arr answering an authoritative no-match and another
// unreachable, the no-match is not authoritative and the transfer is kept
// no matter how long the outage lasts; once the unreachable *arr recovers
// (still no match) the long-expired grace period deletes the transfer.
func TestErroredTransferMixedLookupFailureNeverDeletes(t *testing.T) {
	fake := newFakePremiumize(t, []premiumizeme.Transfer{erroredTransfer("t1")})
	clock := newFakeClock()
	radarr := &fakeArr{name: "Radarr", lookupErr: errors.New("radarr unreachable")}
	m := newErroredTransferTestService(t, fake, clock,
		&fakeArr{name: "Sonarr"}, // authoritative no-match
		radarr,                   // outage
	)

	m.TaskUpdateTransfersList() // first seen at T0
	for i := 0; i < 3; i++ {
		clock.Advance(24 * time.Hour) // far beyond the grace period
		m.TaskUpdateTransfersList()
	}

	if got := fake.events.countEvent("delete:t1"); got != 0 {
		t.Fatalf("delete attempts while one *arr is unreachable = %d, want 0 (a mixed no-match is not authoritative)", got)
	}
	if got := trackingCount(m); got != 1 {
		t.Fatalf("tracking count while one *arr is unreachable = %d, want 1", got)
	}

	// The unreachable *arr recovers and also has no match: the no-match is
	// now authoritative on every *arr, so the long-expired grace period
	// deletes the transfer.
	radarr.lookupErr = nil
	m.TaskUpdateTransfersList()
	waitForEvent(t, fake, "deleted:t1", 1)
	waitForProcessing(t, m)
	if ids := fake.deletedIDs(); len(ids) != 1 || ids[0] != "t1" {
		t.Fatalf("deleted ids = %v, want [t1] after the outage recovered", ids)
	}
}

// TestErroredTransferGracePeriodClampedForHugeValues verifies that an
// out-of-range configured grace period cannot overflow the duration
// arithmetic into a negative grace (which would delete immediately): the
// value is clamped and the transfer is kept.
func TestErroredTransferGracePeriodClampedForHugeValues(t *testing.T) {
	fake := newFakePremiumize(t, []premiumizeme.Transfer{erroredTransfer("t1")})
	clock := newFakeClock()
	m := newErroredTransferTestServiceWithGrace(t, fake, clock, math.MaxInt, &fakeArr{name: "Sonarr"})

	m.TaskUpdateTransfersList()
	clock.Advance(24 * time.Hour)
	m.TaskUpdateTransfersList()
	if got := fake.events.countEvent("delete:t1"); got != 0 {
		t.Fatalf("delete attempts with a huge configured grace period = %d, want 0 (clamped, not wrapped negative)", got)
	}
	if got := trackingCount(m); got != 1 {
		t.Fatalf("tracking count with a huge configured grace period = %d, want 1", got)
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
	m := newErroredTransferTestServiceWithGrace(t, fake, clock, 60, &fakeArr{name: "Sonarr"})

	m.TaskUpdateTransfersList() // first seen at T0
	clock.Advance(59 * time.Second)
	m.TaskUpdateTransfersList()
	if got := fake.events.countEvent("delete:t1"); got != 0 {
		t.Fatalf("delete attempts at 59s with a 60s grace period = %d, want 0", got)
	}
	clock.Advance(2 * time.Second) // 61s > 60s
	m.TaskUpdateTransfersList()
	waitForEvent(t, fake, "deleted:t1", 1)
	waitForProcessing(t, m)
	if ids := fake.deletedIDs(); len(ids) != 1 || ids[0] != "t1" {
		t.Fatalf("deleted ids = %v, want [t1]", ids)
	}

	// Unset (zero) grace period: the 5 minute default applies, so no
	// immediate deletion.
	fake2 := newFakePremiumize(t, []premiumizeme.Transfer{erroredTransfer("t2")})
	clock2 := newFakeClock()
	m2 := newErroredTransferTestServiceWithGrace(t, fake2, clock2, 0, &fakeArr{name: "Sonarr"})

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
	waitForProcessing(t, m2)
	if ids := fake2.deletedIDs(); len(ids) != 1 || ids[0] != "t2" {
		t.Fatalf("deleted ids = %v, want [t2]", ids)
	}
}

// TestErroredTransferFreshLookupFailureNeverDeletes verifies the last gate
// before an unmatched deletion: when the grace-expired FRESH history
// lookup fails on an *arr (an outage that started after the cached
// lookup), the no-match is not authoritative and the transfer is kept
// until the *arr answers again.
func TestErroredTransferFreshLookupFailureNeverDeletes(t *testing.T) {
	fake := newFakePremiumize(t, []premiumizeme.Transfer{erroredTransfer("t1")})
	clock := newFakeClock()
	sonarr := &fakeArr{name: "Sonarr", events: fake.events}
	m := newErroredTransferTestService(t, fake, clock, sonarr)

	m.TaskUpdateTransfersList() // first seen at T0, cached lookup: no match
	clock.Advance(5 * time.Minute)

	// The forced history refetch goes down: the fresh no-match is not
	// authoritative, so the transfer must be kept.
	sonarr.freshErr = errors.New("sonarr history refetch failed")
	m.TaskUpdateTransfersList()
	if got := fake.events.countEvent("fresh:Sonarr"); got != 1 {
		t.Fatalf("fresh lookups = %d, want 1 (the grace-expired path must force a fresh lookup)", got)
	}
	if got := fake.events.countEvent("delete:t1"); got != 0 {
		t.Fatalf("delete attempts after a failed fresh lookup = %d, want 0 (a lookup failure is not a no-match)", got)
	}
	if got := trackingCount(m); got != 1 {
		t.Fatalf("tracking count after a failed fresh lookup = %d, want 1 (kept for the next poll)", got)
	}

	// The *arr recovers and the fresh lookup confirms the no-match: the
	// long-expired grace period now deletes the transfer.
	sonarr.freshErr = nil
	m.TaskUpdateTransfersList()
	waitForEvent(t, fake, "deleted:t1", 1)
	waitForProcessing(t, m)
	if ids := fake.deletedIDs(); len(ids) != 1 || ids[0] != "t1" {
		t.Fatalf("deleted ids = %v, want [t1] after the fresh lookup recovered", ids)
	}
}

// TestErroredTrackingNotStoppedWhileProcessingInFlight verifies that the
// poller-side "no longer errored" stop path does not remove the tracking
// state of a transfer whose report/delete goroutine is still in flight:
// the goroutine still owns the processing slot and settles the state.
func TestErroredTrackingNotStoppedWhileProcessingInFlight(t *testing.T) {
	fake := newFakePremiumize(t, []premiumizeme.Transfer{erroredTransfer("t1")})
	clock := newFakeClock()
	m := newErroredTransferTestService(t, fake, clock,
		&fakeArr{name: "Sonarr", hasMatch: true, matchedID: 101, events: fake.events},
	)
	fake.blockDeletes()

	m.TaskUpdateTransfersList() // match: report spawned, delete pinned in flight
	release := fake.awaitBlockedDelete(t, "t1")

	// The transfer stops being errored while processing is in flight:
	// the stop path must skip the in-flight entry instead of removing it.
	done := erroredTransfer("t1")
	done.Status = "completed"
	fake.setTransfers(t, []premiumizeme.Transfer{done})
	m.TaskUpdateTransfersList()
	if got := trackingCount(m); got != 1 {
		t.Fatalf("tracking count while processing is in flight = %d, want 1 (the stop path must not remove an in-flight entry)", got)
	}

	close(release)
	waitForProcessing(t, m)
	if got := fake.events.countEvent("fail:101"); got != 1 {
		t.Fatalf("failure reports = %d, want exactly 1", got)
	}
	if got := fake.events.countEvent("delete:t1"); got != 1 {
		t.Fatalf("delete attempts = %d, want exactly 1", got)
	}
	if ids := fake.deletedIDs(); len(ids) != 1 || ids[0] != "t1" {
		t.Fatalf("deleted ids = %v, want [t1]", ids)
	}
	if got := trackingCount(m); got != 0 {
		t.Fatalf("tracking count after the in-flight processing settled = %d, want 0", got)
	}
}

// TestErroredTrackingNotPrunedWhileProcessingInFlight verifies that
// pruneErroredTransfers does not remove the tracking state of a transfer
// that disappeared from the list while its report/delete goroutine is
// still in flight: the goroutine still owns the processing slot and
// settles the state.
func TestErroredTrackingNotPrunedWhileProcessingInFlight(t *testing.T) {
	fake := newFakePremiumize(t, []premiumizeme.Transfer{erroredTransfer("t1")})
	clock := newFakeClock()
	m := newErroredTransferTestService(t, fake, clock,
		&fakeArr{name: "Sonarr", hasMatch: true, matchedID: 101, events: fake.events},
	)
	fake.blockDeletes()

	m.TaskUpdateTransfersList() // match: report spawned, delete pinned in flight
	release := fake.awaitBlockedDelete(t, "t1")

	// The transfer disappears from the list while processing is in
	// flight: the prune path must skip the in-flight entry.
	fake.setTransfers(t, nil)
	m.TaskUpdateTransfersList()
	if got := trackingCount(m); got != 1 {
		t.Fatalf("tracking count while processing is in flight = %d, want 1 (the prune path must not remove an in-flight entry)", got)
	}

	close(release)
	waitForProcessing(t, m)
	if got := fake.events.countEvent("fail:101"); got != 1 {
		t.Fatalf("failure reports = %d, want exactly 1", got)
	}
	if got := fake.events.countEvent("delete:t1"); got != 1 {
		t.Fatalf("delete attempts = %d, want exactly 1", got)
	}
	if got := trackingCount(m); got != 0 {
		t.Fatalf("tracking count after the in-flight processing settled = %d, want 0", got)
	}
}

// TestErroredTransferDeletePathInFlightSkipsRepeatedPolls verifies the
// in-flight skip on the UNMATCHED delete path: while one delete of a
// grace-expired transfer is in flight, repeated polls must not spawn a
// second one, so the transfer is deleted exactly once.
func TestErroredTransferDeletePathInFlightSkipsRepeatedPolls(t *testing.T) {
	fake := newFakePremiumize(t, []premiumizeme.Transfer{erroredTransfer("t1")})
	clock := newFakeClock()
	m := newErroredTransferTestService(t, fake, clock, &fakeArr{name: "Sonarr"})
	fake.blockDeletes()

	m.TaskUpdateTransfersList() // first seen at T0
	clock.Advance(5 * time.Minute)

	m.TaskUpdateTransfersList() // grace expired: delete spawned, pinned in flight
	release := fake.awaitBlockedDelete(t, "t1")

	// Repeated polls while the delete is in flight: all must skip.
	for i := 0; i < 2; i++ {
		clock.Advance(24 * time.Hour)
		m.TaskUpdateTransfersList()
	}
	if got := fake.events.countEvent("delete:t1"); got != 1 {
		t.Fatalf("delete attempts while one is in flight = %d, want 1 (repeated polls must skip the in-flight transfer)", got)
	}

	close(release)
	waitForEvent(t, fake, "deleted:t1", 1)
	waitForProcessing(t, m)
	if got := fake.events.countEvent("delete:t1"); got != 1 {
		t.Fatalf("delete attempts = %d, want exactly 1", got)
	}
	if ids := fake.deletedIDs(); len(ids) != 1 || ids[0] != "t1" {
		t.Fatalf("deleted ids = %v, want [t1]", ids)
	}

	m.TaskUpdateTransfersList()
	if got := trackingCount(m); got != 0 {
		t.Fatalf("tracking count after completion = %d, want 0", got)
	}
}

// TestErroredTransferFreshMatchInFlightSkipsRepeatedPolls verifies the
// in-flight skip on the FRESH-match path: while one report/delete for a
// fresh-lookup match is in flight, repeated polls (which also find the
// fresh match) must not spawn a second one.
func TestErroredTransferFreshMatchInFlightSkipsRepeatedPolls(t *testing.T) {
	fake := newFakePremiumize(t, []premiumizeme.Transfer{erroredTransfer("t1")})
	clock := newFakeClock()
	m := newErroredTransferTestService(t, fake, clock,
		&fakeArr{name: "Sonarr", freshMatch: true, matchedID: 101, events: fake.events},
	)
	fake.blockDeletes()

	m.TaskUpdateTransfersList() // cached lookup: no match, within grace
	clock.Advance(5 * time.Minute)

	m.TaskUpdateTransfersList() // grace expired: fresh match, report spawned, delete pinned
	release := fake.awaitBlockedDelete(t, "t1")

	// Repeated polls while in flight: each re-runs the fresh lookup and
	// finds the same match, but must skip (the slot is held).
	for i := 0; i < 2; i++ {
		clock.Advance(24 * time.Hour)
		m.TaskUpdateTransfersList()
	}
	if got := fake.events.countEvent("delete:t1"); got != 1 {
		t.Fatalf("delete attempts while one is in flight = %d, want 1 (repeated fresh matches must skip the in-flight transfer)", got)
	}
	if got := fake.events.countEvent("fresh:Sonarr"); got != 3 {
		t.Fatalf("fresh lookups = %d, want 3 (one per grace-expired poll)", got)
	}

	close(release)
	waitForEvent(t, fake, "deleted:t1", 1)
	waitForProcessing(t, m)
	if got := fake.events.countEvent("fail:101"); got != 1 {
		t.Fatalf("failure reports = %d, want exactly 1", got)
	}
	if got := fake.events.countEvent("delete:t1"); got != 1 {
		t.Fatalf("delete attempts = %d, want exactly 1", got)
	}
	if ids := fake.deletedIDs(); len(ids) != 1 || ids[0] != "t1" {
		t.Fatalf("deleted ids = %v, want [t1]", ids)
	}

	m.TaskUpdateTransfersList()
	if got := trackingCount(m); got != 0 {
		t.Fatalf("tracking count after completion = %d, want 0", got)
	}
}

// TestErroredTransferProcessingIsolatedPerTransferID verifies that the
// processing slots are per transfer ID: while one transfer's delete is
// pinned in flight, a second errored transfer is reported and deleted
// independently, and settling the first does not duplicate or disturb the
// second.
func TestErroredTransferProcessingIsolatedPerTransferID(t *testing.T) {
	secondName := "Other.Release.720p.WEB.x264-GRP.mkv.nzb"
	second := erroredTransfer("t2")
	second.Name = secondName
	fake := newFakePremiumize(t, []premiumizeme.Transfer{erroredTransfer("t1"), second})
	clock := newFakeClock()
	m := newErroredTransferTestService(t, fake, clock,
		// Each *arr matches exactly one of the two transfers, so the two
		// report/delete goroutines run against different *arrs.
		&fakeArr{name: "Sonarr", matchByName: map[string]int64{testErroredTransferName: 101}, events: fake.events},
		&fakeArr{name: "Radarr", matchByName: map[string]int64{secondName: 201}, events: fake.events},
	)
	fake.blockDeletes()

	m.TaskUpdateTransfersList() // both transfers match (different *arrs): both reports in flight
	release1 := fake.awaitBlockedDelete(t, "t1")
	release2 := fake.awaitBlockedDelete(t, "t2")

	// Both deletes pinned: nothing may have been deleted yet.
	if got := fake.events.countEvent("delete:t1"); got != 1 {
		t.Fatalf("delete:t1 attempts before release = %d, want 1 (attempt started, delete pinned)", got)
	}
	if got := fake.events.countEvent("delete:t2"); got != 1 {
		t.Fatalf("delete:t2 attempts before release = %d, want 1 (attempt started, delete pinned)", got)
	}
	if ids := fake.deletedIDs(); len(ids) != 0 {
		t.Fatalf("deleted ids before release = %v, want none (both deletes are pinned)", ids)
	}

	// Release the first delete: it settles independently of the second.
	close(release1)
	waitForEvent(t, fake, "deleted:t1", 1)
	if got := fake.events.countEvent("delete:t2"); got != 1 {
		t.Fatalf("delete:t2 attempts after the first settled = %d, want 1 (the second must stay in flight, not be duplicated)", got)
	}
	if ids := fake.deletedIDs(); len(ids) != 1 || ids[0] != "t1" {
		t.Fatalf("deleted ids after the first settled = %v, want [t1]", ids)
	}

	// A poll while the second delete is still in flight must skip it.
	m.TaskUpdateTransfersList()
	if got := fake.events.countEvent("delete:t2"); got != 1 {
		t.Fatalf("delete:t2 attempts after a concurrent poll = %d, want 1 (the in-flight second must be skipped)", got)
	}
	if got := trackingCount(m); got != 1 {
		t.Fatalf("tracking count with the second still in flight = %d, want 1 (only the second remains)", got)
	}

	close(release2)
	waitForEvent(t, fake, "deleted:t2", 1)
	waitForProcessing(t, m)
	if got := fake.events.countEvent("fail:101"); got != 1 {
		t.Fatalf("failure reports to Sonarr = %d, want exactly 1", got)
	}
	if got := fake.events.countEvent("fail:201"); got != 1 {
		t.Fatalf("failure reports to Radarr = %d, want exactly 1", got)
	}
	if got := fake.events.countEvent("delete:t1"); got != 1 {
		t.Fatalf("delete:t1 attempts = %d, want exactly 1", got)
	}
	if got := fake.events.countEvent("delete:t2"); got != 1 {
		t.Fatalf("delete:t2 attempts = %d, want exactly 1", got)
	}
	ids := fake.deletedIDs()
	if len(ids) != 2 || ids[0] != "t1" || ids[1] != "t2" {
		t.Fatalf("deleted ids = %v, want [t1 t2]", ids)
	}

	m.TaskUpdateTransfersList()
	if got := trackingCount(m); got != 0 {
		t.Fatalf("tracking count after both settled = %d, want 0", got)
	}
}

// TestErroredTrackingListFailureKeepsOriginalFirstSeen verifies that a
// failed transfer-list poll does not reset the grace period: the first
// seen time from the successful poll stands, so the grace period that
// spans the failed poll still expires and deletes.
func TestErroredTrackingListFailureKeepsOriginalFirstSeen(t *testing.T) {
	fake := newFakePremiumize(t, []premiumizeme.Transfer{erroredTransfer("t1")})
	clock := newFakeClock()
	m := newErroredTransferTestService(t, fake, clock, &fakeArr{name: "Sonarr"})

	m.TaskUpdateTransfersList() // first seen at T0
	if got := trackingCount(m); got != 1 {
		t.Fatalf("tracking count after first poll = %d, want 1", got)
	}

	// Halfway through the 5 minute grace period the transfer list fails:
	// the poll aborts before it can touch the tracking state.
	clock.Advance(2*time.Minute + 30*time.Second)
	fake.mu.Lock()
	fake.failList = 1
	fake.mu.Unlock()
	m.TaskUpdateTransfersList()
	if got := trackingCount(m); got != 1 {
		t.Fatalf("tracking count after a failed list poll = %d, want 1", got)
	}

	// Halfway again (5 minutes after T0 in total): the grace period is
	// measured from the ORIGINAL first-seen time, so it has now expired
	// and the unmatched transfer is deleted.
	clock.Advance(2*time.Minute + 30*time.Second)
	m.TaskUpdateTransfersList()
	waitForEvent(t, fake, "deleted:t1", 1)
	waitForProcessing(t, m)
	if ids := fake.deletedIDs(); len(ids) != 1 || ids[0] != "t1" {
		t.Fatalf("deleted ids = %v, want [t1] (the failed list poll must not reset the grace period)", ids)
	}
}
