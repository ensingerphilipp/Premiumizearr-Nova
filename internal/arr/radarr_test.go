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
