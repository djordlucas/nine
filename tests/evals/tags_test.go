package evals_test

import (
	"testing"

	"nine/tests/evals/runner"
)

func TestFilterTags(t *testing.T) {
	cases := []*runner.Case{
		{ID: "a", Tags: []string{"tools", "generated"}},
		{ID: "b", Tags: []string{"memory"}},
		{ID: "c", Tags: []string{"generated"}},
		{ID: "d"},
	}
	ids := func(cs []*runner.Case) (out []string) {
		for _, c := range cs {
			out = append(out, c.ID)
		}
		return out
	}
	for _, tc := range []struct {
		tags []string
		want string
	}{
		{[]string{"generated"}, "a,c"},
		{[]string{"memory", "generated"}, "a,b,c"}, // any tag matches, each case once
		{[]string{"absent"}, ""},
	} {
		got := ""
		for i, id := range ids(filterTags(cases, tc.tags)) {
			if i > 0 {
				got += ","
			}
			got += id
		}
		if got != tc.want {
			t.Errorf("filterTags(%v) = %q, want %q", tc.tags, got, tc.want)
		}
	}
}
