package plugin_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"nine/internal/plugin"
)

// Plugins are MCP servers, full stop (adr/modular_nine.md P4): the legacy
// envelope is gone, and the manager speaks MCP on whichever transport the
// plugin picks — streamable HTTP on its Unix socket (the default) or stdio
// (the manifest's `transport = "stdio"` opt-in, §9.6). These pin both
// transports and the rejection of anything that is not MCP.

// A plugin answering initialize on its socket loads with bare tool names
// (first-party tools are never prefixed, §9.9), unbounded concurrency
// (unix-socket HTTP is concurrent), and no async jobs.
func TestStartMCPPluginSocketHTTP(t *testing.T) {
	bin := buildBinary(t, "./internal/plugin/testplugin")
	m := plugin.NewManager("")

	p, err := m.Start(bin)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer m.Stop(p) //nolint:errcheck

	if p.Name != "testplugin" {
		t.Errorf("plugin name = %q, want %q", p.Name, "testplugin")
	}
	if p.MaxConcurrent != 0 {
		t.Errorf("max_concurrent = %d, want 0 (unix-socket HTTP is concurrent)", p.MaxConcurrent)
	}
	if p.AsyncJobs {
		t.Error("async_jobs = true, want false: MCP plugins do not advertise jobs")
	}

	args := json.RawMessage(`{"message":"hello mcp"}`)
	res, err := m.Call(context.Background(), p, "echo", args)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if res.Output != string(args) {
		t.Errorf("output = %q, want %q", res.Output, args)
	}
}

// The stdio transport loads the same plugin over pipes, serial.
func TestStartMCPPluginStdio(t *testing.T) {
	bin := buildBinary(t, "./internal/plugin/testplugin")
	m := plugin.NewManager("")

	p, err := m.StartStdio(bin)
	if err != nil {
		t.Fatalf("StartStdio: %v", err)
	}
	defer m.Stop(p) //nolint:errcheck

	if p.MaxConcurrent != 1 {
		t.Errorf("max_concurrent = %d, want 1 (stdio is serial)", p.MaxConcurrent)
	}
	res, err := m.Call(context.Background(), p, "echo", json.RawMessage(`{"via":"stdio"}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if res.Output != `{"via":"stdio"}` {
		t.Errorf("output = %q, want the echoed args", res.Output)
	}
}

// A binary that never answers initialize is not a plugin: the start fails and
// nothing is tracked. (The old envelope's protocol-version rejection is gone
// with it — initialize is the negotiation.)
func TestStartRejectsNonMCPBinary(t *testing.T) {
	m := plugin.NewManager("")

	if p, err := m.Start("/usr/bin/true"); err == nil {
		m.Stop(p) //nolint:errcheck
		t.Fatal("Start(/usr/bin/true) = nil error, want failure")
	}
	if len(m.Running()) != 0 {
		t.Errorf("rejected binary was tracked; Running() = %d", len(m.Running()))
	}
	if _, err := m.StartStdio("/usr/bin/true"); err == nil {
		t.Fatal("StartStdio(/usr/bin/true) = nil error, want failure")
	}
}

// A job method against an MCP plugin fails closed: the envelope's job methods
// are gone with it, and no plugin advertises async_jobs.
func TestMCPPluginRejectsJobs(t *testing.T) {
	bin := buildBinary(t, "./internal/plugin/testplugin")
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

// [plugins].disabled blocks both transports by wire name before any process is
// spawned (R-PLUG.14).
func TestDisabledBlocksBothTransports(t *testing.T) {
	bin := buildBinary(t, "./internal/plugin/testplugin")

	m := plugin.NewManager("")
	m.SetDisabled([]string{"testplugin"})

	for _, start := range []func() (*plugin.Plugin, error){
		func() (*plugin.Plugin, error) { return m.Start(bin) },
		func() (*plugin.Plugin, error) { return m.StartStdio(bin) },
	} {
		p, err := start()
		if err == nil {
			m.Stop(p) //nolint:errcheck
			t.Fatal("start on a disabled plugin = nil error, want ErrPluginDisabled")
		}
		if !errors.Is(err, plugin.ErrPluginDisabled) {
			t.Errorf("err = %v, want ErrPluginDisabled", err)
		}
	}
}

// Probe (the `nine plugin validate` handshake) accepts a plugin on either
// transport: socket HTTP, or stdio when asked.
func TestProbeBothTransports(t *testing.T) {
	bin := buildBinary(t, "./internal/plugin/testplugin")

	for _, transport := range []string{plugin.TransportHTTP, plugin.TransportStdio} {
		t.Run(transport, func(t *testing.T) {
			desc, err := plugin.ProbeTransport(bin, transport)
			if err != nil {
				t.Fatalf("ProbeTransport(%s): %v", transport, err)
			}
			found := false
			for _, tool := range desc.Tools {
				if tool.Name == "echo" {
					found = true
				}
			}
			if !found {
				t.Errorf("tools = %+v, want echo among them", desc.Tools)
			}
		})
	}

	// ProbeAny finds the transport by itself.
	if _, err := plugin.ProbeAny(bin); err != nil {
		t.Errorf("ProbeAny: %v", err)
	}

	// A non-MCP binary fails both probes.
	if _, err := plugin.ProbeAny("/usr/bin/true"); err == nil {
		t.Error("ProbeAny(/usr/bin/true) = nil, want failure")
	}
}

// A stdio user plugin is probed and started over pipes when its manifest says
// so; the socket default is unchanged.
func TestUserPluginStdioManifest(t *testing.T) {
	bin := buildBinary(t, "./internal/plugin/testplugin")
	dir := t.TempDir()
	entrypoint, _ := json.Marshal(bin)
	manifest := "name = \"weather\"\nentrypoint = " + string(entrypoint) + "\ntransport = \"stdio\"\n"
	if err := os.WriteFile(filepath.Join(dir, "weather.toml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	m := plugin.NewManager("")
	m.LoadUserPlugins(dir)
	defer m.StopAll() //nolint:errcheck

	st, ok := statusByName(m.UserStatus(), "weather")
	if !ok {
		t.Fatal("weather not in UserStatus")
	}
	if !st.Loaded {
		t.Fatalf("weather not loaded: %s", st.Err)
	}
	p, ok := m.PluginByName(filepath.Base(bin))
	if !ok {
		t.Fatal("weather plugin not running")
	}
	if p.MaxConcurrent != 1 {
		t.Errorf("max_concurrent = %d, want 1 for a stdio user plugin", p.MaxConcurrent)
	}
	res, err := m.Call(context.Background(), p, "echo", json.RawMessage(`{"ok":true}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if res.Output != `{"ok":true}` {
		t.Errorf("output = %q, want the echoed args", res.Output)
	}
}
