package toolvm

import (
	"context"
	"testing"
)

// Since adr/modular_nine.md P4 the non-live shipped tools are served as in-process
// Go handlers (internal/agent, RegisterShippedTools); the host loads only the Live
// process programs (pursue, reflect) from the shipped catalog. These pin that
// split and the namespace rule that still applies to what the host does load.

func TestLoadShippedLoadsOnlyLivePrograms(t *testing.T) {
	ctx := context.Background()
	h, err := Open(ctx, Config{UserDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer h.Close(ctx) //nolint:errcheck
	h.SetShippedWorkspace(ShippedWorkspace{Host: t.TempDir()})

	h.LoadShipped(ctx, nil)

	for _, name := range []string{"pursue", "reflect"} {
		tool := h.Get(name)
		if tool == nil {
			t.Errorf("live program %q did not register", name)
			continue
		}
		if !tool.Live || !tool.Shipped {
			t.Errorf("%s: Live=%v Shipped=%v, want both true", name, tool.Live, tool.Shipped)
		}
	}
	for _, name := range []string{"time", "read_file", "write_file", "edit_file", "http_get", "web_search"} {
		if h.Get(name) != nil {
			t.Errorf("non-live shipped tool %q is on the host; it is a Go handler now (agent.RegisterShippedTools)", name)
		}
	}
}

// A developer tool cannot take a shipped name — the namespace rule is
// first-registered-wins, and LoadShipped runs before Load.
func TestLoadShippedYieldsToAnExistingOwner(t *testing.T) {
	ctx := context.Background()
	h, err := Open(ctx, Config{UserDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer h.Close(ctx) //nolint:errcheck
	h.SetShippedWorkspace(ShippedWorkspace{Host: t.TempDir()})

	owner := "someone"
	h.LoadShipped(ctx, func(name string) (string, bool) {
		if name == "pursue" {
			return owner, true
		}
		return "", false
	})
	if tool := h.Get("pursue"); tool != nil {
		t.Error("pursue loaded despite the collision")
	}
}

// resolveShipped still refuses declarations the host cannot enforce — the
// capability model's core property survives for the live programs.
func TestResolveShippedRefusesUnbackedCapabilities(t *testing.T) {
	if _, err := resolveShipped(Declaration{FS: []string{"read"}}, ShippedWorkspace{}, nil, nil); err == nil {
		t.Error("fs declaration with no workspace was accepted")
	}
	if _, err := resolveShipped(Declaration{Net: []string{"http"}}, ShippedWorkspace{}, nil, nil); err == nil {
		t.Error("net declaration with no allow_hosts was accepted")
	}
	if _, err := resolveShipped(Declaration{FS: []string{"fly"}}, ShippedWorkspace{Host: "/tmp"}, nil, nil); err == nil {
		t.Error("unknown fs verb was accepted")
	}
}
