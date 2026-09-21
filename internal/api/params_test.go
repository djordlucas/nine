package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// =============================================================================
// Query Parameter Parsing
// =============================================================================

func newQueryRequest(query string) *http.Request {
	return httptest.NewRequest("GET", "/test?"+query, nil)
}

func TestParsePageParams_Defaults(t *testing.T) {
	p, err := parsePageParams(newQueryRequest(""))
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

func TestParsePageParams_Explicit(t *testing.T) {
	p, err := parsePageParams(newQueryRequest("limit=10&offset=25"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.Limit != 10 || p.Offset != 25 {
		t.Errorf("Expected limit=10 offset=25, got limit=%d offset=%d", p.Limit, p.Offset)
	}
}

// A malformed page parameter is rejected rather than silently replaced by the
// default: a client that mistypes `limit` should learn that, not receive a
// differently sized page than it asked for.
func TestParsePageParams_Rejects(t *testing.T) {
	for _, query := range []string{
		"limit=abc",
		"limit=0",
		"limit=-1",
		"limit=1001",
		"offset=abc",
		"offset=-1",
	} {
		if _, err := parsePageParams(newQueryRequest(query)); err == nil {
			t.Errorf("Expected %q to be rejected", query)
		}
	}
}

func TestParsePageParams_AcceptsMaxLimit(t *testing.T) {
	p, err := parsePageParams(newQueryRequest("limit=1000"))
	if err != nil {
		t.Fatalf("limit at the maximum must be accepted: %v", err)
	}
	if p.Limit != MaxPageLimit {
		t.Errorf("Expected limit %d, got %d", MaxPageLimit, p.Limit)
	}
}

func TestQueryBool(t *testing.T) {
	cases := map[string]bool{"v=true": true, "v=1": true, "v=false": false, "v=0": false, "": false}
	for query, want := range cases {
		got, err := queryBool(newQueryRequest(query), "v")
		if err != nil {
			t.Errorf("%q: unexpected error: %v", query, err)
			continue
		}
		if got != want {
			t.Errorf("%q: expected %v, got %v", query, want, got)
		}
	}

	if _, err := queryBool(newQueryRequest("v=maybe"), "v"); err == nil {
		t.Error("Expected 'maybe' to be rejected as a boolean")
	}
}

func TestQueryInt(t *testing.T) {
	got, err := queryInt(newQueryRequest("n=7"), "n", 3)
	if err != nil || got != 7 {
		t.Errorf("Expected 7, got %d (err: %v)", got, err)
	}

	got, err = queryInt(newQueryRequest(""), "n", 3)
	if err != nil || got != 3 {
		t.Errorf("Expected default 3, got %d (err: %v)", got, err)
	}

	if _, err := queryInt(newQueryRequest("n=x"), "n", 3); err == nil {
		t.Error("Expected 'x' to be rejected as an integer")
	}
}

// =============================================================================
// Paging
// =============================================================================

func TestPaginate(t *testing.T) {
	items := []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}

	tests := []struct {
		name       string
		params     pageParams
		wantPage   []int
		wantTotal  int
		wantHasMore bool
	}{
		{"first page", pageParams{Limit: 3, Offset: 0}, []int{0, 1, 2}, 10, true},
		{"middle page", pageParams{Limit: 3, Offset: 3}, []int{3, 4, 5}, 10, true},
		{"last full page", pageParams{Limit: 3, Offset: 9}, []int{9}, 10, false},
		{"limit exceeds total", pageParams{Limit: 50, Offset: 0}, items, 10, false},
		{"offset at end", pageParams{Limit: 3, Offset: 10}, []int{}, 10, false},
		{"offset past end", pageParams{Limit: 3, Offset: 999}, []int{}, 10, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			page, pagination := paginate(items, tc.params)

			if len(page) != len(tc.wantPage) {
				t.Fatalf("Expected page %v, got %v", tc.wantPage, page)
			}
			for i := range page {
				if page[i] != tc.wantPage[i] {
					t.Fatalf("Expected page %v, got %v", tc.wantPage, page)
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
	page, _ := paginate([]int{1, 2, 3}, pageParams{Limit: 10, Offset: 99})
	if page == nil {
		t.Error("Expected an empty slice, got nil")
	}
}

func TestPaginate_NilInput(t *testing.T) {
	page, pagination := paginate[int](nil, pageParams{Limit: 10, Offset: 0})
	if page == nil {
		t.Error("Expected an empty slice, got nil")
	}
	if pagination.Total != 0 || pagination.HasMore {
		t.Errorf("Expected total 0 and has_more false, got %d/%v", pagination.Total, pagination.HasMore)
	}
}
