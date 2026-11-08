package plugin_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"nine/internal/plugin"
)

// The manager handshakes MCP first and falls back to the legacy envelope
// (R-PLUG.16, adr/modular_nine.md P1). These tests pin both directions of that
// dual-stack against real fixture binaries.

// A plugin that answers initialize on its socket loads as an MCP plugin: bare
// tool names (first-party tools are never prefixed, §9.9), unbounded
// concurrency (unix-socket HTTP is concurrent), no async jobs (MCP's core has
// none; they ride a _meta extension the manager does not consume yet).
func TestStartMCPSpeakingPlugin(t *testing.T) {
	bin := buildBinary(t, "./internal/plugin/testmcpserve")
	m := plugin.NewManager("")

	p, err := m.Start(bin)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer m.Stop(p) //nolint:errcheck

	if p.Name != "testmcpserve" {
		t.Errorf("plugin name = %q, want %q", p.Name, "testmcpserve")
	}
	if len(p.Tools) != 1 || p.Tools[0].Name != "mcp_echo" {
		t.Fatalf("tools = %+v, want bare [mcp_echo]", p.Tools)
	}
	if p.Tools[0].Description == "" || len(p.Tools[0].InputSchema) == 0 {
		t.Errorf("tool description/schema dropped: %+v", p.Tools[0])
	}
	if p.MaxConcurrent != 0 {
		t.Errorf("max_concurrent = %d, want 0 (unix-socket HTTP is concurrent)", p.MaxConcurrent)
	}
	if p.AsyncJobs {
		t.Error("async_jobs = true, want false: MCP plugins do not advertise jobs")
	}

	args := json.RawMessage(`{"message":"hello mcp"}`)
	res, err := m.Call(context.Background(), p, "mcp_echo", args)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if res.Output != string(args) {
		t.Errorf("output = %q, want %q", res.Output, args)
	}
}

// A job method against an MCP plugin fails closed, exactly as a v1 legacy plugin
// without async_jobs does: the daemon refuses rather than guessing.
func TestMCPPluginRejectsJobs(t *testing.T) {
	bin := buildBinary(t, "./internal/plugin/testmcpserve")
	m := plugin.NewManager("")
	p, err := m.Start(bin)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer m.Stop(p) //nolint:errcheck

	if _, err := m.JobStatus(context.Background(), p, "job-1"); err == nil {
		t.Error("JobStatus on an MCP plugin = nil, want an error (async_jobs is false)")
	}
	if err := m.JobCancel(context.Background(), p, "job-1"); err == nil {
		t.Error("JobCancel on an MCP plugin = nil, want an error (async_jobs is false)")
	}
}

// The legacy envelope still loads through the same spawn path — the fallback is
// what keeps every existing user plugin working during the dual-stack.
func TestLegacyEnvelopeStillLoads(t *testing.T) {
	bin := buildBinary(t, "./internal/plugin/testplugin")
	m := plugin.NewManager("")

	p, err := m.Start(bin)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer m.Stop(p) //nolint:errcheck

	if len(p.Tools) != 1 || p.Tools[0].Name != "echo" {
		t.Fatalf("tools = %+v, want [echo]", p.Tools)
	}
	res, err := m.Call(context.Background(), p, "echo", json.RawMessage(`{"x":1}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if res.Output == "" {
		t.Error("empty echo output")
	}
}

// [plugins].disabled blocks both start paths: an MCP-speaking plugin and a
// legacy one are refused by wire name before any process is spawned (R-PLUG.14).
// (The Try* entry points additionally record the refusal so UnmatchedDisabled
// does not report the name as a typo; covered by the built-in tests.)
func TestDisabledBlocksBothWires(t *testing.T) {
	mcpBin := buildBinary(t, "./internal/plugin/testmcpserve")
	legacyBin := buildBinary(t, "./internal/plugin/testplugin")

	m := plugin.NewManager("")
	m.SetDisabled([]string{"testmcpserve", "testplugin"})

	for _, bin := range []string{mcpBin, legacyBin} {
		p, err := m.Start(bin)
		if err == nil {
			m.Stop(p) //nolint:errcheck
			t.Errorf("Start(%s) on a disabled plugin = nil error, want %v", bin, plugin.ErrPluginDisabled)
			continue
		}
		if !errors.Is(err, plugin.ErrPluginDisabled) {
			t.Errorf("Start(%s) err = %v, want ErrPluginDisabled", bin, err)
		}
	}
}

// Probe (the `nine plugin validate` handshake) accepts both wires: an
// MCP-speaking binary and a legacy one both validate, with their tools listed.
func TestProbeAcceptsBothWires(t *testing.T) {
	mcpBin := buildBinary(t, "./internal/plugin/testmcpserve")
	legacyBin := buildBinary(t, "./internal/plugin/testplugin")

	for _, tc := range []struct {
		name string
		bin  string
		want string
	}{
		{"mcp", mcpBin, "mcp_echo"},
		{"legacy", legacyBin, "echo"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			desc, err := plugin.Probe(tc.bin)
			if err != nil {
				t.Fatalf("Probe: %v", err)
			}
			if len(desc.Tools) != 1 || desc.Tools[0].Name != tc.want {
				t.Errorf("tools = %+v, want [%s]", desc.Tools, tc.want)
			}
		})
	}
}
