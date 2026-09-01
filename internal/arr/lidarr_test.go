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

// TestLidarrHistoryContainsOnlyNonGrabbedRecords verifies that a release name
// present only as non-grabbed history (e.g. an old download failure) is
// reported as not in history.
func TestLidarrHistoryContainsOnlyNonGrabbedRecords(t *testing.T) {
	runOnlyNonGrabbedRecordsTest(t, newTestLidarrArr, "v1", "")
}
