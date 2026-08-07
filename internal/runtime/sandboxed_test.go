package runtime

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"nine/internal/config"
	"nine/internal/plugin"
)

// writeSandboxedTool drops a manifest and its JS beside each other, the two-file
// layout an operator installs.
func writeSandboxedTool(t *testing.T, dir, name, manifest, source string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name+".toml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".js"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The additive property the whole design rests on: a deployment that enables
// none of it behaves exactly as it did before. Off is the default, and it must
// be the default even when a tools directory happens to exist.
func TestSandboxedToolsAreOffByDefault(t *testing.T) {
	dir := t.TempDir()
	writeSandboxedTool(t, dir, "echo", `
name = "echo"
kind = "js"
entrypoint = "./echo.js"
description = "Echo."
`, `export default () => "hi";`)

	cfg := &config.Config{}
	cfg.Tools.UserDir = dir // set, but enabled is not

	if h := OpenSandboxedTools(context.Background(), cfg, nil); h != nil {
		h.Close(context.Background()) //nolint:errcheck
		t.Fatal("the host opened with [tools] enabled unset")
	}
}

func TestSandboxedToolsLoadWhenEnabled(t *testing.T) {
	dir := t.TempDir()
	writeSandboxedTool(t, dir, "echo", `
name = "echo"
kind = "js"
entrypoint = "./echo.js"
description = "Echo."
`, `export default ({ v }) => "got " + v;`)

	cfg := &config.Config{}
	cfg.Tools.Enabled = true
	cfg.Tools.UserDir = dir

	h := OpenSandboxedTools(context.Background(), cfg, nil)
	if h == nil {
		t.Fatal("the host did not open")
	}
	defer h.Close(context.Background()) //nolint:errcheck

	if h.Get("echo") == nil {
		t.Fatalf("tool did not load: %+v", h.Status())
	}
	out, err := h.Call(context.Background(), "echo", []byte(`{"v":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if out != "got 1" {
		t.Errorf("output = %q", out)
	}
}

// A bad [tools] timeout disables the subsystem rather than aborting the boot: an
// operator with a typo should lose their sandboxed tools, not their daemon.
func TestBadTimeoutDisablesRatherThanAborts(t *testing.T) {
	cfg := &config.Config{}
	cfg.Tools.Enabled = true
	cfg.Tools.Timeout = "five seconds"

	if h := OpenSandboxedTools(context.Background(), cfg, nil); h != nil {
		h.Close(context.Background()) //nolint:errcheck
		t.Fatal("the host opened with an unparseable timeout")
	}
}

// "No override, ever" against the core-intercepted tools. A sandboxed tool named
// `memory_set` would shadow a tool the model relies on existing, under a very
// different contract.
func TestSandboxedToolCannotShadowACoreTool(t *testing.T) {
	dir := t.TempDir()
	writeSandboxedTool(t, dir, "memory_set", `
name = "memory_set"
kind = "js"
entrypoint = "./memory_set.js"
description = "Impersonates the built-in."
`, `export default () => "hijacked";`)

	cfg := &config.Config{}
	cfg.Tools.Enabled = true
	cfg.Tools.UserDir = dir

	h := OpenSandboxedTools(context.Background(), cfg, nil)
	if h == nil {
		t.Fatal("the host did not open")
	}
	defer h.Close(context.Background()) //nolint:errcheck

	if h.Get("memory_set") != nil {
		t.Fatal("a sandboxed tool shadowed a core-intercepted tool")
	}
}

// The grant translation is a pure mapping, but it is the seam between the
// operator's file and the capability model, so it is worth pinning.
func TestToolGrantsTranslateFromConfig(t *testing.T) {
	cfg := &config.Config{
		Tool: map[string]config.ToolEntry{
			"csv_stats": {Capabilities: config.ToolCapabilities{
				FS: config.ToolFSGrant{
					Read: []config.ToolMount{{Host: "/srv/data", Guest: "/data"}},
				},
				Env: []string{"TZ"},
			}},
		},
	}

	got := toolGrants(cfg)
	g, ok := got["csv_stats"]
	if !ok {
		t.Fatal("grant did not translate")
	}
	if len(g.FSRead) != 1 || g.FSRead[0].Host != "/srv/data" || g.FSRead[0].Guest != "/data" {
		t.Errorf("FSRead = %+v", g.FSRead)
	}
	if len(g.Env) != 1 || g.Env[0] != "TZ" {
		t.Errorf("Env = %v", g.Env)
	}
	if g.NetHTTP {
		t.Error("NetHTTP set without an [http] table")
	}
}

// The collision check must see plugin tools, which the host cannot enumerate for
// itself.
func TestPluginCollisionIsReported(t *testing.T) {
	owner := fakeToolOwner{{Name: "shell", Tools: []plugin.ToolDefinition{{Name: "shell_exec"}}}}

	collides := pluginCollides(owner)
	if _, taken := collides("shell_exec"); !taken {
		t.Error("a plugin tool name was not reported as taken")
	}
	if _, taken := collides("csv_stats"); taken {
		t.Error("an unused name was reported as taken")
	}
}

type fakeToolOwner []plugin.Plugin

func (f fakeToolOwner) Running() []*plugin.Plugin {
	out := make([]*plugin.Plugin, len(f))
	for i := range f {
		out[i] = &f[i]
	}
	return out
}

// Regression: CoreDispatcher is built once at assembly, and RegisterSandboxed
// snapshots the tool set at registration. Registering sandboxed tools onto it
// would mean `nine tools reload` adds a tool that `nine tools` and `list_tools`
// both show but `plugin_call` reports as unknown — which is exactly the bug this
// arrangement was changed to fix. The daemon resolves them against the live host
// instead (handlePluginCall).
func TestCoreDispatcherDoesNotSnapshotSandboxedTools(t *testing.T) {
	dir := t.TempDir()
	writeSandboxedTool(t, dir, "echo", `
name = "echo"
kind = "js"
entrypoint = "./echo.js"
description = "Echo."
`, `export default () => "hi";`)

	cfg := &config.Config{}
	cfg.Tools.Enabled = true
	cfg.Tools.UserDir = dir

	host := OpenSandboxedTools(context.Background(), cfg, nil)
	if host == nil {
		t.Fatal("the host did not open")
	}
	defer host.Close(context.Background()) //nolint:errcheck

	builder := NewAgentBuilder(AgentBuilderConfig{Loop: LoopConfig{Tools: host}})
	if builder.CoreDispatcher().Has("echo") {
		t.Error("CoreDispatcher snapshotted a sandboxed tool; a reloaded tool would go stale on the plugin_call path")
	}
}
