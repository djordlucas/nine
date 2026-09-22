package api

import (
	"testing"
)

// =============================================================================
// Page Bounds
// =============================================================================

// The generated router parses limit and offset, so a non-integer never reaches
// page(). What remains ours is the range, which the document declares and the
// server enforces.

func TestPage_Defaults(t *testing.T) {
	p, err := page(nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.Limit != DefaultPageLimit {
		t.Errorf("Expected default limit %d, got %d", DefaultPageLimit, p.Limit)
	}
	if p.Offset != 0 {
		t.Errorf("Expected default offset 0, got %d", p.Offset)
	}
}

func TestPage_Explicit(t *testing.T) {
	p, err := page(ptr(10), ptr(25))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.Limit != 10 || p.Offset != 25 {
		t.Errorf("Expected limit=10 offset=25, got limit=%d offset=%d", p.Limit, p.Offset)
	}
}

// An out-of-range value is an error rather than a silent clamp: a client asking
// for 5000 items should learn the cap, not receive 1000 and assume that was all.
func TestPage_RejectsOutOfRange(t *testing.T) {
	tests := []struct {
		name          string
		limit, offset *int
	}{
		{"limit zero", ptr(0), nil},
		{"limit negative", ptr(-1), nil},
		{"limit above maximum", ptr(MaxPageLimit + 1), nil},
		{"offset negative", nil, ptr(-1)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := page(tc.limit, tc.offset); err == nil {
				t.Error("Expected an error")
			}
		})
	}
}

func TestPage_AcceptsMaximumLimit(t *testing.T) {
	p, err := page(ptr(MaxPageLimit), nil)
	if err != nil {
		t.Fatalf("limit at the maximum must be accepted: %v", err)
	}
	if p.Limit != MaxPageLimit {
		t.Errorf("Expected limit %d, got %d", MaxPageLimit, p.Limit)
	}
}

func TestDerefBool(t *testing.T) {
	if derefBool(nil) {
		t.Error("Expected absent to be false")
	}
	if !derefBool(ptr(true)) {
		t.Error("Expected true")
	}
	if derefBool(ptr(false)) {
		t.Error("Expected false")
	}
}

// =============================================================================
// Paging
// =============================================================================

func TestPaginate(t *testing.T) {
	items := []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}

	tests := []struct {
		name        string
		params      pageParams
		wantPage    []int
		wantTotal   int
		wantHasMore bool
	}{
		{"first page", pageParams{Limit: 3, Offset: 0}, []int{0, 1, 2}, 10, true},
		{"middle page", pageParams{Limit: 3, Offset: 3}, []int{3, 4, 5}, 10, true},
		{"last partial page", pageParams{Limit: 3, Offset: 9}, []int{9}, 10, false},
		{"limit exceeds total", pageParams{Limit: 50, Offset: 0}, items, 10, false},
		{"offset at end", pageParams{Limit: 3, Offset: 10}, []int{}, 10, false},
		{"offset past end", pageParams{Limit: 3, Offset: 999}, []int{}, 10, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, pagination := paginate(items, tc.params)

			if len(got) != len(tc.wantPage) {
				t.Fatalf("Expected page %v, got %v", tc.wantPage, got)
			}
			for i := range got {
				if got[i] != tc.wantPage[i] {
					t.Fatalf("Expected page %v, got %v", tc.wantPage, got)
				}
			}
			if pagination.Total != tc.wantTotal {
				t.Errorf("Expected total %d, got %d", tc.wantTotal, pagination.Total)
			}
			if pagination.HasMore != tc.wantHasMore {
				t.Errorf("Expected has_more %v, got %v", tc.wantHasMore, pagination.HasMore)
			}
			if pagination.Limit != tc.params.Limit || pagination.Offset != tc.params.Offset {
				t.Errorf("Expected the request's limit/offset echoed back, got %d/%d",
					pagination.Limit, pagination.Offset)
			}
		})
	}
}

// An empty page must serialise as [] rather than null, so clients can iterate
// without a nil check.
func TestPaginate_EmptyPageIsNotNil(t *testing.T) {
	got, _ := paginate([]int{1, 2, 3}, pageParams{Limit: 10, Offset: 99})
	if got == nil {
		t.Error("Expected an empty slice, got nil")
	}
}

func TestPaginate_NilInput(t *testing.T) {
	got, pagination := paginate[int](nil, pageParams{Limit: 10, Offset: 0})
	if got == nil {
		t.Error("Expected an empty slice, got nil")
	}
	if pagination.Total != 0 || pagination.HasMore {
		t.Errorf("Expected total 0 and has_more false, got %d/%v", pagination.Total, pagination.HasMore)
	}
}
