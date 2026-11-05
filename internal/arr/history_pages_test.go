package arr

import (
	"golift.io/starr"
	"testing"
)

type testHistory struct {
	records []int
	total   int
}

func TestLoadHistoryPagesKeepsOffsetsStable(t *testing.T) {
	const total = 1076
	calls := 0
	fetch := func(req *starr.PageReq) (*testHistory, error) {
		calls++
		if req.PageSize != 1000 {
			t.Fatalf("page %d has size %d", req.Page, req.PageSize)
		}
		h := &testHistory{total: total}
		for i := (req.Page - 1) * req.PageSize; i < req.Page*req.PageSize && i < total; i++ {
			h.records = append(h.records, i)
		}
		return h, nil
	}
	h, err := loadHistoryPages(fetch, func(h *testHistory) (int, int) { return len(h.records), h.total }, func(a, b *testHistory) { a.records = append(a.records, b.records...) })
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || len(h.records) != total {
		t.Fatalf("calls=%d, records=%d", calls, len(h.records))
	}
	for i, v := range h.records {
		if i != v {
			t.Fatalf("record %d is %d", i, v)
		}
	}
}

// TestLoadHistoryPagesStopsOnEmptyPage pins the count == 0 termination
// branch: when the server reports a total larger than the records it will
// ever serve (e.g. a stale total after history pruning), the loop must stop
// after the first empty page instead of requesting empty pages forever — in
// the wrappers that would hold all four history mutexes and stall every
// HistoryContains/HistoryContainsFresh/HandleErrorTransfer call for that
// *arr. The call bound fails a mutant that removes the guard instead of
// hanging; the exactly-satisfied total of the test above still stops on the
// last non-empty page with no extra probe request.
func TestLoadHistoryPagesStopsOnEmptyPage(t *testing.T) {
	const (
		total    = 1500
		served   = 1000
		maxCalls = 2
	)
	calls := 0
	fetch := func(req *starr.PageReq) (*testHistory, error) {
		calls++
		if calls > maxCalls {
			t.Fatalf("loadHistoryPages kept requesting pages past the last non-empty page (calls=%d): the count == 0 termination branch is missing", calls)
		}
		h := &testHistory{total: total}
		if req.Page == 1 {
			for i := 0; i < served; i++ {
				h.records = append(h.records, i)
			}
		}
		return h, nil
	}
	h, err := loadHistoryPages(fetch, func(h *testHistory) (int, int) { return len(h.records), h.total }, func(a, b *testHistory) { a.records = append(a.records, b.records...) })
	if err != nil {
		t.Fatal(err)
	}
	if calls != maxCalls {
		t.Fatalf("calls = %d, want %d (stop after the first empty page, no extra probe requests)", calls, maxCalls)
	}
	if len(h.records) != served {
		t.Fatalf("records = %d, want %d (everything the server actually served)", len(h.records), served)
	}
}
