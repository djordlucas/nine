package tui

import (
	"strings"
	"testing"
)

func TestContextHint(t *testing.T) {
	tests := []struct {
		name        string
		used, budg  int
		showContext bool
		want        string
	}{
		{"unknown budget is silent", 5000, 0, true, ""},
		{"below threshold, show on", 4000, 8000, true, "  ·  ctx: 4000/8000"},
		{"below threshold, show off", 4000, 8000, false, ""},
		{"at threshold warns even when show off", 7200, 8000, false, "  ·  ⚠ ctx: 7200/8000 (90%)"},
		{"over threshold warns when show on", 7600, 8000, true, "  ·  ⚠ ctx: 7600/8000 (95%)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := contextHint(tc.used, tc.budg, tc.showContext)
			if got != tc.want {
				t.Errorf("contextHint(%d, %d, %v) = %q, want %q", tc.used, tc.budg, tc.showContext, got, tc.want)
			}
		})
	}
}

// The warning marker must survive whatever showContext preference the user set,
// so a session under pressure is never silently at the ceiling.
func TestContextHintWarnsRegardlessOfShowContext(t *testing.T) {
	for _, show := range []bool{true, false} {
		if !strings.Contains(contextHint(7999, 8000, show), "⚠") {
			t.Errorf("showContext=%v: expected ⚠ warning near budget ceiling", show)
		}
	}
}
