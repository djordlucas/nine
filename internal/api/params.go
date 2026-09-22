package api

import (
	"nine/internal/api/apigen"
)

// Pagination bounds (spec/contracts/api.md API-HTTP-6).
const (
	// DefaultPageLimit is the page size applied when a request omits `limit`.
	DefaultPageLimit = 50

	// MaxPageLimit caps `limit` so one request cannot ask the daemon to
	// materialise an unbounded result set.
	MaxPageLimit = 1000
)

// pageParams is the validated limit/offset pair for a list request. The
// generated router parses the values; page() in strict.go bounds them. Offset paging is the whole of it: the daemon hands back a fully
// materialised slice on every call, so there is no server-side stream for an
// opaque cursor to point into, and one would only encode the offset by another
// name while implying a stability the underlying slice does not have.
type pageParams struct {
	Limit  int
	Offset int
}

// paginate slices items to the requested page and describes it. An offset past
// the end yields an empty page rather than an error: a client walking offsets
// to exhaustion should see the list run out, not a failure.
func paginate[T any](items []T, p pageParams) ([]T, apigen.Pagination) {
	total := len(items)

	start := min(p.Offset, total)
	end := min(start+p.Limit, total)
	page := items[start:end]

	// Guarantee `[]` rather than `null` in JSON for an empty page, so clients
	// can iterate the field without a nil check.
	if page == nil {
		page = []T{}
	}

	return page, apigen.Pagination{
		Limit:   p.Limit,
		Offset:  p.Offset,
		Total:   total,
		HasMore: end < total,
	}
}

