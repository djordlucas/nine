package runtime

import (
	"context"
	"encoding/json"
	"testing"

	"nine/internal/agent"
	"nine/internal/config"
	"nine/internal/memory/memtest"
)

// enabledAgentCfg is the minimum config that turns the generated tier on: the
// host itself enabled, plus [tools.agent].
func enabledAgentCfg() *config.Config {
	cfg := &config.Config{}
	cfg.Tools.Enabled = true
	cfg.Tools.Agent.Enabled = true
	cfg.Tools.Agent.Eval = true
	return cfg
}

// The whole tier end to end: an agent writes a tool, and it is callable on the
// host the next loop would build from — through the store, the ceiling check, and
// the projection back into the host, with no operator file anywhere.
func TestGeneratedToolWriteMakesToolCallable(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	host := OpenSandboxedTools(context.Background(), enabledAgentCfg(), store, nil)
	if host == nil {
		t.Fatal("host did not open")
	}
	t.Cleanup(func() { _ = host.Close(context.Background()) })

	gt := NewGeneratedToolStore(store, host, nil)
	if gt == nil {
		t.Fatal("generated tier is off despite [tools.agent] enabled")
	}

	// A pure transform: no capabilities declared, so it runs under the empty grant.
	evicted, err := gt.Write(context.Background(), genSpec("double", `export default ({n}) => ({ out: n*2 });`, nil))
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if len(evicted) != 0 {
		t.Fatalf("nothing should evict from an empty catalog: %v", evicted)
	}

	tool := host.Get("double")
	if tool == nil {
		t.Fatalf("written tool is not on the host: %+v", host.Status())
	}
	if !tool.Generated {
		t.Error("tool is not marked generated")
	}

	out, err := host.Call(context.Background(), "double", []byte(`{"n":21}`))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if out != `{"out":42}` {
		t.Errorf("output = %q", out)
	}

	// The call is recorded for LRU eviction.
	got, ok, err := store.GeneratedToolGet("double")
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if got.CallCount != 1 {
		t.Errorf("call_count = %d, want 1", got.CallCount)
	}
}

// Delete removes the row and unloads the tool from the host, so the next loop
// cannot call it.
func TestGeneratedToolDeleteUnloads(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	host := OpenSandboxedTools(context.Background(), enabledAgentCfg(), store, nil)
	t.Cleanup(func() { _ = host.Close(context.Background()) })
	gt := NewGeneratedToolStore(store, host, nil)

	if _, err := gt.Write(context.Background(), genSpec("noop", `export default () => "ok";`, nil)); err != nil {
		t.Fatal(err)
	}
	if host.Get("noop") == nil {
		t.Fatal("tool did not load")
	}
	if err := gt.Delete(context.Background(), "noop"); err != nil {
		t.Fatal(err)
	}
	if host.Get("noop") != nil {
		t.Error("deleted tool is still on the host")
	}
	if _, ok, _ := store.GeneratedToolGet("noop"); ok {
		t.Error("deleted tool is still in the store")
	}
}

// A capability the ceiling excludes is refused at write time — before anything is
// persisted, so the refusal cannot leave a dead row behind (§7).
func TestGeneratedToolWriteRefusedAboveCeiling(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Empty ceiling: net.http is not grantable to generated tools here.
	host := OpenSandboxedTools(context.Background(), enabledAgentCfg(), store, nil)
	t.Cleanup(func() { _ = host.Close(context.Background()) })
	gt := NewGeneratedToolStore(store, host, nil)

	caps := json.RawMessage(`{"net":["http"]}`)
	if _, err := gt.Write(context.Background(), genSpec("fetcher", `export default () => "x";`, caps)); err == nil {
		t.Fatal("write above the ceiling was accepted")
	}
	if _, ok, _ := store.GeneratedToolGet("fetcher"); ok {
		t.Error("a refused write left a row behind")
	}
}

// A generated tool cannot take a core meta-tool name: "one name, one
// implementation" (I-TVM.4) must hold against tool_write/tool_delete/js_eval too,
// or a write could shadow the very handler that created it.
func TestGeneratedToolCannotShadowMetaTool(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	host := OpenSandboxedTools(context.Background(), enabledAgentCfg(), store, nil)
	t.Cleanup(func() { _ = host.Close(context.Background()) })
	gt := NewGeneratedToolStore(store, host, nil)

	for _, name := range []string{"tool_write", "tool_delete", "js_eval"} {
		if _, err := gt.Write(context.Background(), genSpec(name, `export default () => "x";`, nil)); err == nil {
			t.Errorf("write named %q was accepted; it must be refused", name)
		}
		if _, ok, _ := store.GeneratedToolGet(name); ok {
			t.Errorf("a refused write named %q left a row behind", name)
		}
	}
}

// js_eval runs under the generated-tool rules and persists nothing.
func TestGeneratedEvalPersistsNothing(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	host := OpenSandboxedTools(context.Background(), enabledAgentCfg(), store, nil)
	t.Cleanup(func() { _ = host.Close(context.Background()) })
	gt := NewGeneratedToolStore(store, host, nil)

	out, err := gt.Eval(context.Background(), `export default ({a,b}) => ({ sum: a+b });`, nil, []byte(`{"a":2,"b":3}`))
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	if out != `{"sum":5}` {
		t.Errorf("output = %q", out)
	}
	if n, _ := store.GeneratedToolCount(); n != 0 {
		t.Errorf("eval persisted %d tools, want 0", n)
	}
}

// The tier stays off unless [tools.agent] opts in, even with the host enabled.
func TestGeneratedTierOffWithoutAgentConfig(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	cfg := &config.Config{}
	cfg.Tools.Enabled = true // host on, but [tools.agent] left off

	host := OpenSandboxedTools(context.Background(), cfg, store, nil)
	t.Cleanup(func() { _ = host.Close(context.Background()) })

	if host.AgentEnabled() {
		t.Fatal("generated tier is on without [tools.agent] enabled")
	}
	if gt := NewGeneratedToolStore(store, host, nil); gt != nil {
		t.Fatal("NewGeneratedToolStore returned a backend for a disabled tier")
	}
}

// The on_capability gate decision (§9.4): a write that declares nothing is inert
// and passes without a prompt; anything with reach — or unparseable args — gates.
func TestDeclaresCapability(t *testing.T) {
	cases := []struct {
		name string
		args string
		want bool
	}{
		{"no capabilities field", `{"name":"x","source":"..."}`, false},
		{"empty object", `{"capabilities":{}}`, false},
		{"empty arrays", `{"capabilities":{"fs":[],"net":[],"env":[]}}`, false},
		{"declares fs", `{"capabilities":{"fs":["read"]}}`, true},
		{"declares net", `{"capabilities":{"net":["http"]}}`, true},
		{"declares env", `{"capabilities":{"env":["TZ"]}}`, true},
		{"unparseable args gate to be safe", `not json`, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := declaresCapability(json.RawMessage(c.args)); got != c.want {
				t.Errorf("declaresCapability(%s) = %v, want %v", c.args, got, c.want)
			}
		})
	}
}

func TestCapsSummary(t *testing.T) {
	got := capsSummary(json.RawMessage(`{"fs":["read"],"net":["http"],"env":["TZ"]}`))
	if got != "fs=read net=http env=TZ" {
		t.Errorf("capsSummary = %q", got)
	}
	if s := capsSummary(json.RawMessage(`{}`)); s != "" {
		t.Errorf("empty declaration rendered %q, want empty", s)
	}
}

func genSpec(name, source string, caps json.RawMessage) agent.GeneratedToolSpec {
	return agent.GeneratedToolSpec{
		Name:         name,
		Description:  "generated: " + name,
		InputSchema:  json.RawMessage(`{"type":"object"}`),
		Source:       source,
		Capabilities: caps,
	}
}
