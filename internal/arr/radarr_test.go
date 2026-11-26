package arr

import "testing"

// TestRadarrHistoryContainsResolvesNewestGrabbedRecord verifies the issue #22
// fix on the Radarr implementation: when an older non-grabbed history record
// and a newer grabbed record share the transfer's release name,
// HistoryContains must resolve the grabbed record so the failure can be
// reported to Radarr.
func TestRadarrHistoryContainsResolvesNewestGrabbedRecord(t *testing.T) {
	runErrorTransferReportingTest(t, newTestRadarrArr, "v3")
}

// TestRadarrHistoryContainsResolvesNewestOfMultipleGrabbedRecords verifies
// that with several grabbed records sharing the release name, the newest
// one is resolved so the failure lands on the record Radarr actually tried.
func TestRadarrHistoryContainsResolvesNewestOfMultipleGrabbedRecords(t *testing.T) {
	runMultipleGrabbedRecordsTest(t, newTestRadarrArr, "v3")
}

// TestRadarrHistoryContainsFreshForcesRefetch verifies that
// HistoryContainsFresh forces a history refetch instead of serving the
// still-fresh cache.
func TestRadarrHistoryContainsFreshForcesRefetch(t *testing.T) {
	runHistoryContainsFreshForcesRefetchTest(t, newTestRadarrArr, "v3")
}

// TestRadarrHistoryContainsOnlyNonGrabbedRecords verifies that a release name
// present only as non-grabbed history (e.g. an old download failure) is
// reported as not in history.
func TestRadarrHistoryContainsOnlyNonGrabbedRecords(t *testing.T) {
	runOnlyNonGrabbedRecordsTest(t, newTestRadarrArr, "v3", "")
}

// TestRadarrHistoryContainsLookupFailureReturnsError verifies that a failed
// history fetch (500) surfaces as an error instead of an authoritative
// no-match, so a Radarr outage never looks like "not in history".
func TestRadarrHistoryContainsLookupFailureReturnsError(t *testing.T) {
	runHistoryLookupFailureTest(t, newTestRadarrArr, "v3")
}

// TestRadarrGetHistoryCollectsAllRecords pins the Radarr wrapper's history
// call site to the paginated loadHistoryPages fetch: with >1000 records the
// old single arr.Client.GetHistory(0, 1000) call re-reads offset-shifted
// pages and misses most of the history.
func TestRadarrGetHistoryCollectsAllRecords(t *testing.T) {
	runGetHistoryPaginatesAllRecordsTest(t, newTestRadarrArr, "v3", func(a IArr) ([]int64, error) {
		his, err := a.(*RadarrArr).GetHistory()
		if err != nil {
			return nil, err
		}
		ids := make([]int64, len(his.Records))
		for i, rec := range his.Records {
			ids[i] = rec.ID
		}
		return ids, nil
	})
}
