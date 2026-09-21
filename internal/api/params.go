package api

import (
	"fmt"
	"net/http"
	"strconv"
)

// Pagination bounds (spec/contracts/api.md API-HTTP-5).
const (
	// DefaultPageLimit is the page size applied when a request omits `limit`.
	DefaultPageLimit = 50

	// MaxPageLimit caps `limit` so one request cannot ask the daemon to
	// materialise an unbounded result set.
	MaxPageLimit = 1000
)

// pageParams is the limit/offset pair parsed from a list request's query
// string. Offset paging is the whole of it: the daemon hands back a fully
// materialised slice on every call, so there is no server-side stream for an
// opaque cursor to point into, and one would only encode the offset by another
// name while implying a stability the underlying slice does not have.
type pageParams struct {
	Limit  int
	Offset int
}

// parsePageParams reads `limit` and `offset`. Absent values take the defaults;
// present-but-invalid values are an error rather than a silent fallback, so a
// typo surfaces as a 400 instead of quietly returning the wrong page.
func parsePageParams(r *http.Request) (pageParams, error) {
	p := pageParams{Limit: DefaultPageLimit}

	if raw := r.URL.Query().Get("limit"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil {
			return p, fmt.Errorf("limit must be an integer, got %q", raw)
		}
		if limit < 1 {
			return p, fmt.Errorf("limit must be at least 1, got %d", limit)
		}
		if limit > MaxPageLimit {
			return p, fmt.Errorf("limit must be at most %d, got %d", MaxPageLimit, limit)
		}
		p.Limit = limit
	}

	if raw := r.URL.Query().Get("offset"); raw != "" {
		offset, err := strconv.Atoi(raw)
		if err != nil {
			return p, fmt.Errorf("offset must be an integer, got %q", raw)
		}
		if offset < 0 {
			return p, fmt.Errorf("offset must not be negative, got %d", offset)
		}
		p.Offset = offset
	}

	return p, nil
}

// paginate slices items to the requested page and describes it. An offset past
// the end yields an empty page rather than an error: a client walking offsets
// to exhaustion should see the list run out, not a failure.
func paginate[T any](items []T, p pageParams) ([]T, Pagination) {
	total := len(items)

	start := min(p.Offset, total)
	end := min(start+p.Limit, total)
	page := items[start:end]

	// Guarantee `[]` rather than `null` in JSON for an empty page, so clients
	// can iterate the field without a nil check.
	if page == nil {
		page = []T{}
	}

	return page, Pagination{
		Limit:   p.Limit,
		Offset:  p.Offset,
		Total:   total,
		HasMore: end < total,
	}
}

// queryBool reads a boolean query parameter. An absent parameter is false; an
// unparseable one is an error, for the same reason as parsePageParams.
func queryBool(r *http.Request, name string) (bool, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return false, nil
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean, got %q", name, raw)
	}
	return v, nil
}

// queryInt reads an integer query parameter, returning def when absent.
func queryInt(r *http.Request, name string, def int) (int, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return def, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return def, fmt.Errorf("%s must be an integer, got %q", name, raw)
	}
	return v, nil
}
