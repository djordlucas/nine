package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"nine/internal/config"
	"nine/internal/memory"
	"nine/internal/memory/memtest"
	"nine/internal/toolvm"
)

// With no operator grant, nine.toml still lands in the store: the workspace
// fallback is written as `default` rows, so one code path reads the ceiling
// whether it came from the file, the workspace, or an approval.
func TestDerivedGrantsFallBackToTheWorkspace(t *testing.T) {
	cfg := &config.Config{}
	cfg.Workspace.Root = "/srv/work"

	derived := DerivedCapabilityGrants(cfg)
	byCap := map[string]memory.CapabilityGrant{}
	for _, g := range derived {
		byCap[g.Capability] = g
	}

	for _, capability := range []string{toolvm.CapFSRead, toolvm.CapFSWrite} {
		g, ok := byCap[capability]
		if !ok {
			t.Fatalf("no %s grant was derived", capability)
		}
		if g.Source != memory.GrantSourceDefault {
			t.Errorf("%s source = %q, want default", capability, g.Source)
		}
		if !strings.Contains(g.Params, "/srv/work") || !strings.Contains(g.Params, toolvm.ShippedWorkspaceGuest) {
			t.Errorf("%s params = %s, want the workspace at the shipped guest path", capability, g.Params)
		}
	}
	if len(derived) != 2 {
		t.Errorf("derived %d grants, want only the two fs defaults: %+v", len(derived), derived)
	}
}

// The file's grants are written as `config` rows, and an explicit fs grant
// replaces the workspace default rather than adding to it.
func TestDerivedGrantsFromConfig(t *testing.T) {
	cfg := &config.Config{}
	cfg.Workspace.Root = "/srv/work"
	cfg.Tools.Agent.Capabilities.FS.Read = []config.ToolMount{{Host: "/srv/data", Guest: "/data"}}
	cfg.Tools.Agent.Capabilities.Env = []string{"TZ"}
	cfg.Tools.Agent.Capabilities.Net.HTTP = &config.ToolHTTPGrant{
		AllowHosts: []string{"api.example"}, Methods: []string{"GET"},
	}

	var sources, caps []string
	for _, g := range DerivedCapabilityGrants(cfg) {
		sources = append(sources, g.Source)
		caps = append(caps, g.Capability)
		if g.Capability == toolvm.CapFSRead && strings.Contains(g.Params, "/srv/work") {
			t.Error("the workspace default was added alongside an explicit fs grant; it must be replaced")
		}
	}
	for _, s := range sources {
		if s != memory.GrantSourceConfig {
			t.Errorf("source = %q, want every grant from the file to be config", s)
		}
	}
	for _, want := range []string{toolvm.CapFSRead, toolvm.CapEnvRead, toolvm.CapNetHTTP} {
		if !contains(caps, want) {
			t.Errorf("no %s grant derived from the file; got %v", want, caps)
		}
	}
	if contains(caps, toolvm.CapFSWrite) {
		t.Errorf("an fs.write grant appeared from a read-only config: %v", caps)
	}
}

// Rows round-trip into the ceiling the host enforces, and two grants of one
// capability union rather than one winning.
func TestCeilingFromGrantsUnions(t *testing.T) {
	mk := func(source, capability string, p grantParams) memory.CapabilityGrant {
		b, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		return memory.CapabilityGrant{ID: memory.NewID(), Source: source, Capability: capability, Params: string(b)}
	}

	ceiling := CeilingFromGrants([]memory.CapabilityGrant{
		mk(memory.GrantSourceDefault, toolvm.CapFSRead, grantParams{
			Mounts: []toolvm.Mount{{Host: "/srv/work", Guest: "/work"}}}),
		mk(memory.GrantSourceApproved, toolvm.CapFSRead, grantParams{
			Mounts: []toolvm.Mount{{Host: "/srv/extra", Guest: "/extra"}}}),
		mk(memory.GrantSourceConfig, toolvm.CapEnvRead, grantParams{Env: []string{"TZ"}}),
		mk(memory.GrantSourceApproved, toolvm.CapEnvRead, grantParams{Env: []string{"LANG"}}),
		mk(memory.GrantSourceApproved, toolvm.CapNetHTTP, grantParams{
			AllowHosts: []string{"api.example"}, Methods: []string{"GET"}, MaxBytes: 2048}),
	})

	if len(ceiling.FSRead) != 2 {
		t.Errorf("FSRead = %+v, want both mounts", ceiling.FSRead)
	}
	if len(ceiling.Env) != 2 || !contains(ceiling.Env, "TZ") || !contains(ceiling.Env, "LANG") {
		t.Errorf("Env = %v, want the file's key and the approved one", ceiling.Env)
	}
	if ceiling.HTTP == nil {
		t.Fatal("no http grant in the ceiling")
	}
	if !contains(ceiling.HTTP.AllowHosts, "api.example") || ceiling.HTTP.MaxBytes != 2048 {
		t.Errorf("HTTP = %+v", ceiling.HTTP)
	}
	if len(ceiling.FSWrite) != 0 {
		t.Errorf("FSWrite = %+v, want none: nothing granted it", ceiling.FSWrite)
	}
}

// A row nobody can parse grants nothing, rather than granting everything or
// aborting the boot. Failing closed is the only safe direction for a ceiling.
func TestCeilingSkipsUnreadableGrant(t *testing.T) {
	ceiling := CeilingFromGrants([]memory.CapabilityGrant{
		{ID: "bad", Source: memory.GrantSourceConfig, Capability: toolvm.CapFSRead, Params: "{not json"},
		{ID: "good", Source: memory.GrantSourceConfig, Capability: toolvm.CapEnvRead, Params: `{"env":["TZ"]}`},
	})
	if len(ceiling.FSRead) != 0 {
		t.Errorf("an unreadable grant conferred %+v", ceiling.FSRead)
	}
	if len(ceiling.Env) != 1 {
		t.Errorf("a readable grant beside a broken one was dropped: %v", ceiling.Env)
	}
}

// Reconciliation writes the file's ceiling into the store and reports what is in
// force — the property that keeps the two consistent without a second source of
// truth.
func TestReconcileWritesConfigIntoTheStore(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	cfg := &config.Config{}
	cfg.Workspace.Root = t.TempDir()
	cfg.Tools.Agent.Capabilities.Env = []string{"TZ"}

	grants, err := ReconcileCapabilityGrants(store, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(grants) == 0 {
		t.Fatal("reconcile reported no grants in force")
	}

	// Readable back through the store, not only through the return value.
	stored, err := store.CapabilityGrantList()
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != len(grants) {
		t.Errorf("store holds %d grants, reconcile reported %d", len(stored), len(grants))
	}
	var sawEnv bool
	for _, g := range stored {
		if g.Capability == toolvm.CapEnvRead && g.Source == memory.GrantSourceConfig {
			sawEnv = true
		}
	}
	if !sawEnv {
		t.Errorf("the file's env grant is not in the store: %+v", stored)
	}
}

// A request is validated against the file's own rules at request time, so the
// refusal reaches the model — which can act on it — rather than an operator, who
// cannot.
func TestCapabilityRequestValidatesScope(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	request := NewCapabilityRequester(store, nil)("conv1")

	for _, tc := range []struct {
		name       string
		capability string
		params     string
		wantErr    string
	}{
		{"unknown capability", "shell", "", "not one Nine can grant"},
		{"http with no methods", toolvm.CapNetHTTP, `{"allow_hosts":["api.example"]}`, "methods"},
		{"http with no hosts", toolvm.CapNetHTTP, `{"methods":["GET"]}`, "allow_hosts"},
		{"relative mount", toolvm.CapFSRead, `{"mounts":[{"host":"data","guest":"/data"}]}`, "absolute"},
		{"reserved env key", toolvm.CapEnvRead, `{"env":["OPENAI_API_KEY"]}`, "reserved"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := request(context.Background(), tc.capability, "because", "t", json.RawMessage(tc.params))
			if err == nil {
				t.Fatalf("%s was accepted", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want it to mention %q", err, tc.wantErr)
			}
		})
	}

	// A well-formed request lands, and lands pending.
	out, err := request(context.Background(), toolvm.CapNetHTTP, "the rates API",
		"fetch_rates", json.RawMessage(`{"allow_hosts":["api.example"],"methods":["GET"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "nothing is granted yet") {
		t.Errorf("the tool's reply does not say the request confers nothing: %q", out)
	}
	pending, err := store.CapabilityRequestList(true)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending = %d, err = %v", len(pending), err)
	}
}

// A request may name a capability without a scope: the operator then decides it.
func TestCapabilityRequestAllowsNoScope(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if _, err := NewCapabilityRequester(store, nil)("conv1")(
		context.Background(), toolvm.CapNetHTTP, "needs the web", "", nil); err != nil {
		t.Fatalf("a request with no params was refused: %v", err)
	}
}

// The request reaches the operator's notification feed, which is the only way one
// made by a background session with nobody watching is ever seen.
func TestCapabilityRequestNotifies(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	var got string
	request := NewCapabilityRequester(store, func(_, text string) { got = text })("conv1")
	if _, err := request(context.Background(), toolvm.CapEnvRead, "reads the timezone", "clock_tool",
		json.RawMessage(`{"env":["TZ"]}`)); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"capability requested", "env", "clock_tool", "reads the timezone", "nine grants approve"} {
		if !strings.Contains(got, want) {
			t.Errorf("the notification omits %q: %q", want, got)
		}
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
