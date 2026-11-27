package arr

import "testing"

// TestLidarrHistoryContainsResolvesNewestGrabbedRecord verifies the issue #22
// fix on the Lidarr implementation: when an older non-grabbed history record
// and a newer grabbed record share the transfer's release name,
// HistoryContains must resolve the grabbed record so the failure can be
// reported to Lidarr.
func TestLidarrHistoryContainsResolvesNewestGrabbedRecord(t *testing.T) {
	runErrorTransferReportingTest(t, newTestLidarrArr, "v1")
}

// TestLidarrHistoryContainsResolvesNewestOfMultipleGrabbedRecords verifies
// that with several grabbed records sharing the release name, the newest
// one is resolved so the failure lands on the record Lidarr actually tried.
func TestLidarrHistoryContainsResolvesNewestOfMultipleGrabbedRecords(t *testing.T) {
	runMultipleGrabbedRecordsTest(t, newTestLidarrArr, "v1")
}

// TestLidarrHistoryContainsFreshForcesRefetch verifies that
// HistoryContainsFresh forces a history refetch instead of serving the
// still-fresh cache.
func TestLidarrHistoryContainsFreshForcesRefetch(t *testing.T) {
	runHistoryContainsFreshForcesRefetchTest(t, newTestLidarrArr, "v1")
}

// TestLidarrHistoryContainsOnlyNonGrabbedRecords verifies that a release name
// present only as non-grabbed history (e.g. an old download failure) is
// reported as not in history.
func TestLidarrHistoryContainsOnlyNonGrabbedRecords(t *testing.T) {
	runOnlyNonGrabbedRecordsTest(t, newTestLidarrArr, "v1", "")
}

// TestLidarrHistoryContainsLookupFailureReturnsError verifies that a failed
// history fetch (500) surfaces as an error instead of an authoritative
// no-match, so a Lidarr outage never looks like "not in history".
func TestLidarrHistoryContainsLookupFailureReturnsError(t *testing.T) {
	runHistoryLookupFailureTest(t, newTestLidarrArr, "v1")
}

// TestLidarrGetHistoryCollectsAllRecords pins the Lidarr wrapper's history
// call site to the paginated loadHistoryPages fetch: with >1000 records the
// old single arr.Client.GetHistory(0, 1000) call re-reads offset-shifted
// pages and misses most of the history.
func TestLidarrGetHistoryCollectsAllRecords(t *testing.T) {
	runGetHistoryPaginatesAllRecordsTest(t, newTestLidarrArr, "v1", func(a IArr) ([]int64, error) {
		his, err := a.(*LidarrArr).GetHistory()
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
