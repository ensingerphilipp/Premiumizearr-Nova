package arr

import "golift.io/starr"

// loadHistoryPages is a workaround for golift.io/starr's own all-records
// loop: GetHistory/GetHistoryContext fetch in pages but shrink perPage via
// starr.AdjustPerPage as they approach the reported total, and *arr
// computes the history offset as (page-1)*pageSize - so the shrunken final
// page re-reads records already collected and skips the tail (verified
// still present in starr v1.4.1, the newest release when this was written).
//
// This loop keeps the page size fixed instead, and stops on the first empty
// page or a page fetch failure (see its tests). When upstream fixes its
// pagination, call arr.Client.GetHistory(0, 1000) at the wrapper call sites
// again and delete this file. Filing an upstream issue would let the
// workaround be tracked for retirement.
func loadHistoryPages[T any](fetch func(*starr.PageReq) (*T, error), counts func(*T) (int, int), appendPage func(*T, *T)) (*T, error) {
	const pageSize = 1000
	var result *T
	collected := 0
	for page := 1; ; page++ {
		current, err := fetch(&starr.PageReq{Page: page, PageSize: pageSize})
		if err != nil {
			return nil, err
		}
		count, total := counts(current)
		if result == nil {
			result = current
		} else {
			appendPage(result, current)
		}
		collected += count
		if count == 0 || collected >= total {
			return result, nil
		}
	}
}
