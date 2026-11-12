// SPDX-License-Identifier: Apache-2.0

package provider

import "slices"

// paginate splits items into pageSize-sized pages for a fake list pager, so a test can
// check a data source drains every page. An empty list still gives one empty page, as
// the real API does, and pageSize <= 0 means everything on one page.
func paginate[T any](items []T, pageSize int) [][]T {
	if len(items) == 0 || pageSize <= 0 {
		return [][]T{items}
	}
	return slices.Collect(slices.Chunk(items, pageSize))
}
