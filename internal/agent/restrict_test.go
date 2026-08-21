package agent_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"nine/internal/agent"
)

// RestrictTo prunes handlers outside the allowlist so a disallowed tool
// dispatches as unknown (spec/contracts/roles.md R-ROLE.4, boundary 2).
func TestDispatcherRestrictTo(t *testing.T) {
	d := agent.New()
	ok := func(_ context.Context, _ json.RawMessage) (string, error) { return "ok", nil }
	d.InjectHandler("keep_me", ok)
	d.InjectHandler("drop_me", ok)
	d.InjectHandler("gap_report", ok)

	d.RestrictTo([]string{"gap_report", "keep_me", "not_registered"})

	if _, err := d.Dispatch(context.Background(), "keep_me", nil); err != nil {
		t.Errorf("keep_me: %v", err)
	}
	if _, err := d.Dispatch(context.Background(), "gap_report", nil); err != nil {
		t.Errorf("gap_report must survive every allowlist (R-ROLE.5): %v", err)
	}
	_, err := d.Dispatch(context.Background(), "drop_me", nil)
	if err == nil || !strings.Contains(err.Error(), "unknown tool") {
		t.Errorf("drop_me: err = %v, want unknown tool", err)
	}
	// Allowlisted-but-unregistered names are ignored, not created (R-ROLE.5).
	if _, err := d.Dispatch(context.Background(), "not_registered", nil); err == nil {
		t.Error("not_registered must stay unknown")
	}
}
