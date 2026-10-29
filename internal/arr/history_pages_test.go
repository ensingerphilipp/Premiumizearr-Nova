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
