package toolvm

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// The shipped tier's whole purpose: a first-party capability runs inside the
// sandbox instead of as a subprocess holding the daemon's uid authority.
func TestLoadShippedRegistersTimeWithNoCapabilities(t *testing.T) {
	ctx := context.Background()
	h, err := Open(ctx, Config{UserDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer h.Close(ctx) //nolint:errcheck

	h.LoadShipped(ctx, nil)

	tool := h.Get("time")
	if tool == nil {
		t.Fatal("shipped tool \"time\" did not register")
	}
	if !tool.Shipped {
		t.Error("tool is not marked Shipped")
	}
	if tool.Kind != KindJS {
		t.Errorf("Kind = %q, want js", tool.Kind)
	}

	// The point of the migration. As a plugin this had the daemon's full
	// authority; as a tool it has none — no mount, no env, no network.
	g := tool.Grant
	if len(g.FSRead) != 0 || len(g.FSWrite) != 0 {
		t.Errorf("time was granted filesystem access: %+v", g)
	}
	if g.HTTP != nil {
		t.Error("time was granted network access")
	}
	if len(g.Env) != 0 {
		t.Errorf("time was granted env access: %v", g.Env)
	}
}

// It has to actually work, not merely register.
func TestShippedTimeReturnsTheClock(t *testing.T) {
	ctx := context.Background()
	h, err := Open(ctx, Config{UserDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer h.Close(ctx) //nolint:errcheck
	h.LoadShipped(ctx, nil)

	out, err := h.Call(ctx, "time", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	var got struct {
		ISO8601  string `json:"iso8601"`
		Human    string `json:"human"`
		Timezone string `json:"timezone"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("unmarshal %q: %v", out, err)
	}
	// The same three field names the plugin returned, so a prompt or eval
	// expecting them does not move — but the values are UTC now, which is the
	// documented behavior change.
	if got.ISO8601 == "" || !strings.Contains(got.ISO8601, "T") {
		t.Errorf("iso8601 = %q, want an ISO timestamp", got.ISO8601)
	}
	if got.Human == "" {
		t.Errorf("human = %q, want a readable date", got.Human)
	}
	// The guest has no timezone database, so UTC is the only honest answer. The
	// plugin reported the daemon's local zone; a sandboxed tool cannot, and
	// claiming a zone it cannot compute would be worse than saying UTC.
	if got.Timezone != "UTC" {
		t.Errorf("timezone = %q, want UTC", got.Timezone)
	}
	if !strings.HasSuffix(got.ISO8601, "Z") {
		t.Errorf("iso8601 = %q, want a UTC timestamp", got.ISO8601)
	}
	if !strings.HasSuffix(got.Human, "UTC") {
		t.Errorf("human = %q, want it to name the zone it used", got.Human)
	}
}

// A shipped tool loses a name collision like everything else, so a developer or
// generated tool cannot shadow first-party behavior — and neither can a plugin.
func TestLoadShippedYieldsToAnExistingOwner(t *testing.T) {
	ctx := context.Background()
	h, err := Open(ctx, Config{UserDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer h.Close(ctx) //nolint:errcheck

	h.LoadShipped(ctx, func(name string) (string, bool) {
		if name == "time" {
			return "some-plugin", true
		}
		return "", false
	})

	if h.Get("time") != nil {
		t.Error("shipped tool registered over an existing owner")
	}
	var found bool
	for _, s := range h.Status() {
		if s.Name == "time" {
			found = true
			if s.Loaded {
				t.Error("status says loaded despite the collision")
			}
			if !strings.Contains(s.Err, "already provided") {
				t.Errorf("status err = %q, want it to name the collision", s.Err)
			}
		}
	}
	if !found {
		t.Error("a skipped shipped tool must still be reported in Status")
	}
}

// resolveShipped refuses a declaration the tier cannot actually confer, rather
// than registering a tool whose capability silently does nothing.
func TestResolveShippedRefusesUnbackedCapabilities(t *testing.T) {
	if _, err := resolveShipped(Declaration{FS: []string{"read"}}); err == nil {
		t.Error("a shipped fs declaration was accepted with no mount to back it")
	}
	if _, err := resolveShipped(Declaration{Net: []string{"http"}}); err == nil {
		t.Error("a shipped net.http declaration was accepted with no allowlist")
	}
	if g, err := resolveShipped(Declaration{}); err != nil {
		t.Errorf("an empty declaration should resolve: %v", err)
	} else if len(g.capabilities()) != 0 {
		t.Errorf("empty declaration conferred %v", g.capabilities())
	}
}
