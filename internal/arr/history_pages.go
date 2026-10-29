package arr

import "golift.io/starr"

// loadHistoryPages keeps the page size fixed. Changing it between requests
// changes the server's offsets and can skip records on the last page.
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
