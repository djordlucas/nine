package toolvm

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"testing"
)

// fakeStateStore is a map with the StateStore shape. Quota arithmetic lives in
// the real store and is tested there; what this package owns is the grant check,
// the scope resolution, and the envelope, so the fake only has to be faithful
// about keys, scopes, and the one error type the guest is meant to catch.
type fakeStateStore struct {
	mu     sync.Mutex
	data   map[string]string // tool\x00scope\x00key -> value
	failOn string            // a key whose write returns a QuotaError
}

func newFakeStateStore() *fakeStateStore {
	return &fakeStateStore{data: map[string]string{}}
}

func (f *fakeStateStore) k(tool, scope, key string) string {
	return tool + "\x00" + scope + "\x00" + key
}

func (f *fakeStateStore) StateGet(tool, scope, key string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.data[f.k(tool, scope, key)]
	return v, ok, nil
}

func (f *fakeStateStore) StateSet(tool, scope, key, value string, _ StateQuota) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if key == f.failOn {
		return &QuotaError{Reason: "state quota: this tool may hold 1 keys and already holds 1"}
	}
	f.data[f.k(tool, scope, key)] = value
	return nil
}

func (f *fakeStateStore) StateDelete(tool, scope, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.data, f.k(tool, scope, key))
	return nil
}

func (f *fakeStateStore) StateList(tool, scope, prefix string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	want := f.k(tool, scope, prefix)
	var out []string
	for k := range f.data {
		if strings.HasPrefix(k, want) {
			out = append(out, k[strings.LastIndex(k, "\x00")+1:])
		}
	}
	sort.Strings(out)
	return out, nil
}

func (f *fakeStateStore) StateSwap(tool, scope, key string, expected *string, value string, _ StateQuota) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cur, exists := f.data[f.k(tool, scope, key)]
	if expected == nil {
		if exists {
			return false, nil
		}
	} else if !exists || cur != *expected {
		return false, nil
	}
	f.data[f.k(tool, scope, key)] = value
	return true, nil
}

// openStateHost opens a host whose tools are backed by store.
func openStateHost(t *testing.T, dir string, grants map[string]Grant, store StateStore) *Host {
	t.Helper()
	ctx := context.Background()
	h, err := Open(ctx, Config{UserDir: dir, Grants: grants, StateStore: store})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { h.Close(context.Background()) }) //nolint:errcheck
	h.Load(ctx, nil)
	return h
}

const counterManifest = `
name = "counter"
kind = "js"
entrypoint = "./counter.js"
description = "Count how many times it has been called."

[capabilities]
state = true
`

// counterSrc is the point of the whole feature in six lines: a tool that
// remembers, across an instance that does not.
const counterSrc = `
import { get, set } from "nine:state";
export default function () {
  const n = Number(get("calls") ?? 0) + 1;
  set("calls", String(n));
  return String(n);
}
`

func stateGrantFor(scope string) Grant {
	return Grant{State: &StateGrant{Scope: scope}}
}

// The feature's central claim: state survives the instance being destroyed
// between calls, while nothing in the interpreter does.
func TestStateSurvivesAcrossCalls(t *testing.T) {
	dir := t.TempDir()
	writeTool(t, dir, "counter", counterManifest, counterSrc)
	h := openStateHost(t, dir, map[string]Grant{"counter": stateGrantFor(StateScopeTool)}, newFakeStateStore())

	for i, want := range []string{"1", "2", "3"} {
		if got := call(t, h, "counter", `{}`); got != want {
			t.Fatalf("call %d returned %q, want %q — state did not survive the instance", i+1, got, want)
		}
	}
}

// A tool without the grant must be refused, and the refusal must say so rather
// than looking like an empty store — a cache that never hits is otherwise
// indistinguishable from a slow tool.
func TestStateRequiresAGrant(t *testing.T) {
	dir := t.TempDir()
	// Declares nothing, so it is granted nothing.
	writeTool(t, dir, "counter", `
name = "counter"
kind = "js"
entrypoint = "./counter.js"
description = "Count."
`, counterSrc)
	h := openStateHost(t, dir, nil, newFakeStateStore())

	_, err := h.Call(context.Background(), "counter", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("an ungranted tool reached the state store")
	}
	if !strings.Contains(err.Error(), "not granted the state capability") {
		t.Fatalf("error = %v, want it to name the missing capability", err)
	}
}

// Granting state but configuring no store is an operator mistake that must be
// named, not a store that silently forgets.
func TestStateWithoutAStoreIsNamed(t *testing.T) {
	dir := t.TempDir()
	writeTool(t, dir, "counter", counterManifest, counterSrc)
	h := openStateHost(t, dir, map[string]Grant{"counter": stateGrantFor(StateScopeTool)}, nil)

	_, err := h.Call(context.Background(), "counter", json.RawMessage(`{}`))
	if err == nil || !strings.Contains(err.Error(), "no state store configured") {
		t.Fatalf("error = %v, want it to name the missing store", err)
	}
}

// Conversation scope keys the namespace by the caller, so two conversations
// calling one tool must not see each other's values.
func TestStateConversationScopeIsolatesCallers(t *testing.T) {
	dir := t.TempDir()
	writeTool(t, dir, "counter", counterManifest, counterSrc)
	h := openStateHost(t, dir,
		map[string]Grant{"counter": stateGrantFor(StateScopeConversation)}, newFakeStateStore())

	callAs := func(conv string) string {
		t.Helper()
		ctx := WithStateScope(context.Background(), conv)
		out, err := h.Call(ctx, "counter", json.RawMessage(`{}`))
		if err != nil {
			t.Fatalf("call as %s: %v", conv, err)
		}
		return out
	}

	if got := callAs("conv-a"); got != "1" {
		t.Fatalf("conv-a first call = %q, want 1", got)
	}
	if got := callAs("conv-a"); got != "2" {
		t.Fatalf("conv-a second call = %q, want 2", got)
	}
	// A different conversation starts from nothing. If this reads 3, the scope
	// key is being ignored and conversation scope is a lie.
	if got := callAs("conv-b"); got != "1" {
		t.Fatalf("conv-b first call = %q, want 1 — it can see conv-a's state", got)
	}
}

// A conversation-scoped grant with no conversation on the context is refused
// rather than served out of the shared namespace. Falling back would hand the
// tool exactly the cross-conversation visibility the operator chose this scope
// to deny, in the paths that have no turn — where nobody is watching.
func TestStateConversationScopeRefusesAnUnscopedCall(t *testing.T) {
	dir := t.TempDir()
	writeTool(t, dir, "counter", counterManifest, counterSrc)
	h := openStateHost(t, dir,
		map[string]Grant{"counter": stateGrantFor(StateScopeConversation)}, newFakeStateStore())

	_, err := h.Call(context.Background(), "counter", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("a conversation-scoped tool ran with no conversation; it fell back to the shared namespace")
	}
	if !strings.Contains(err.Error(), "no conversation") {
		t.Fatalf("error = %v, want it to explain the missing conversation", err)
	}
}

// A quota refusal must reach the guest as a catchable, distinguishable error:
// hitting a key budget is a normal thing a tool recovers from, where the store
// being unreachable is not.
func TestStateQuotaRefusalIsCatchable(t *testing.T) {
	dir := t.TempDir()
	writeTool(t, dir, "quota", `
name = "quota"
kind = "js"
entrypoint = "./quota.js"
description = "Report what a quota refusal looks like."

[capabilities]
state = true
`, `
import { set } from "nine:state";
export default function () {
  try {
    set("full", "x");
    return "no refusal";
  } catch (e) {
    return e.code + "|" + (e.retryable ? "retryable" : "not-retryable");
  }
}
`)
	store := newFakeStateStore()
	store.failOn = "full"
	h := openStateHost(t, dir, map[string]Grant{"quota": stateGrantFor(StateScopeTool)}, store)

	got := call(t, h, "quota", `{}`)
	if got != "E_STATE_QUOTA|retryable" {
		t.Fatalf("guest saw %q, want E_STATE_QUOTA|retryable", got)
	}
}

// swap is what makes a read-modify-write safe. The guest-visible contract:
// create-if-absent, then a matching expectation, then a stale one.
func TestStateSwapFromGuest(t *testing.T) {
	dir := t.TempDir()
	writeTool(t, dir, "cas", `
name = "cas"
kind = "js"
entrypoint = "./cas.js"
description = "Exercise compare-and-set."

[capabilities]
state = true
`, `
import { swap, get } from "nine:state";
export default function () {
  const out = [];
  out.push(swap("k", null, "first"));   // creates
  out.push(swap("k", null, "again"));   // already exists
  out.push(swap("k", "wrong", "nope")); // stale expectation
  out.push(swap("k", "first", "second"));
  out.push(get("k"));
  return out.join(",");
}
`)
	h := openStateHost(t, dir, map[string]Grant{"cas": stateGrantFor(StateScopeTool)}, newFakeStateStore())

	if got, want := call(t, h, "cas", `{}`), "true,false,false,true,second"; got != want {
		t.Fatalf("swap sequence = %q, want %q", got, want)
	}
}

// nine.caps describes the grant so a tool can report its own limits. It must
// carry the scope and the resolved quotas, and it must never be readable as a
// grant (I-TVM.8) — which is why this only asserts what it says.
func TestCapsDescribesTheStateGrant(t *testing.T) {
	dir := t.TempDir()
	writeTool(t, dir, "introspect", `
name = "introspect"
kind = "js"
entrypoint = "./introspect.js"
description = "Report the state grant."

[capabilities]
state = true
`, `
export default function () {
  const c = globalThis[Symbol.for("nine.internal")].caps();
  return JSON.stringify(c.state);
}
`)
	h := openStateHost(t, dir,
		map[string]Grant{"introspect": {State: &StateGrant{Scope: StateScopeConversation, MaxKeys: 7}}},
		newFakeStateStore())

	var got struct {
		Scope      string `json:"scope"`
		MaxKeys    int    `json:"max_keys"`
		MaxValueKB int    `json:"max_value_kb"`
	}
	if err := json.Unmarshal([]byte(call(t, h, "introspect", `{}`)), &got); err != nil {
		t.Fatal(err)
	}
	if got.Scope != StateScopeConversation {
		t.Errorf("scope = %q, want %q", got.Scope, StateScopeConversation)
	}
	if got.MaxKeys != 7 {
		t.Errorf("max_keys = %d, want the operator's 7", got.MaxKeys)
	}
	if got.MaxValueKB != DefaultStateMaxValueKB {
		t.Errorf("max_value_kb = %d, want the default %d filled in", got.MaxValueKB, DefaultStateMaxValueKB)
	}
}

// The declaration and the grant must name the same capabilities, in both
// directions (R-TVM.6). state is not exempt: a tool that declares it and was
// granted nothing refuses to load rather than running with a store that is
// silently absent.
func TestStateDeclarationAndGrantMustAgree(t *testing.T) {
	t.Run("declared but not granted", func(t *testing.T) {
		dir := t.TempDir()
		writeTool(t, dir, "counter", counterManifest, counterSrc)
		h := openStateHost(t, dir, nil, newFakeStateStore())

		if h.Get("counter") != nil {
			t.Fatal("a tool declaring state loaded with no grant")
		}
		if !skippedFor(h, "counter", "state") {
			t.Fatalf("load status does not name the missing capability: %+v", h.Status())
		}
	})

	t.Run("granted but not declared", func(t *testing.T) {
		dir := t.TempDir()
		writeTool(t, dir, "plain", `
name = "plain"
kind = "js"
entrypoint = "./plain.js"
description = "Declares nothing."
`, `export default function () { return "ok"; }`)
		h := openStateHost(t, dir,
			map[string]Grant{"plain": stateGrantFor(StateScopeTool)}, newFakeStateStore())

		if h.Get("plain") != nil {
			t.Fatal("a tool granted state it never declared loaded anyway")
		}
		if !skippedFor(h, "plain", "state") {
			t.Fatalf("load status does not name the undeclared capability: %+v", h.Status())
		}
	})
}

// skippedFor reports whether name was skipped with a reason mentioning cap.
func skippedFor(h *Host, name, cap string) bool {
	for _, st := range h.Status() {
		if st.Name == name && !st.Loaded && strings.Contains(st.Err, cap) {
			return true
		}
	}
	return false
}

// The generated tier receives the ceiling's state grant only when the tool
// declares it, and cannot request its way past a ceiling that has none.
func TestStateCeilingIsAMaximumNotADefault(t *testing.T) {
	ceiling := Ceiling{Grant: Grant{State: &StateGrant{Scope: StateScopeConversation, MaxKeys: 9}}}

	t.Run("declared", func(t *testing.T) {
		g, err := resolveCeiling(Declaration{State: true}, ceiling)
		if err != nil {
			t.Fatal(err)
		}
		if g.State == nil || g.State.Scope != StateScopeConversation || g.State.MaxKeys != 9 {
			t.Fatalf("granted %+v, want the ceiling's parameters", g.State)
		}
	})

	t.Run("not declared", func(t *testing.T) {
		g, err := resolveCeiling(Declaration{}, ceiling)
		if err != nil {
			t.Fatal(err)
		}
		if g.State != nil {
			t.Fatal("a tool that declared nothing received state from the ceiling")
		}
	})

	t.Run("declared but the ceiling excludes it", func(t *testing.T) {
		_, err := resolveCeiling(Declaration{State: true}, Ceiling{})
		if err == nil {
			t.Fatal("a generated tool requested its way past the ceiling")
		}
		if !strings.Contains(err.Error(), "state") {
			t.Fatalf("refusal = %v, want it to name the capability", err)
		}
	})
}

// The roster is where an operator reviews what a tool may remember, so the
// summary has to carry the scope — the parameter the whole capability turns on.
func TestStateAppearsInTheGrantSummary(t *testing.T) {
	g := Grant{State: &StateGrant{Scope: StateScopeTool, MaxKeys: 5}}
	got := g.Summary()
	for _, want := range []string{"state", "scope=tool", "keys<=5"} {
		if !strings.Contains(got, want) {
			t.Errorf("Summary() = %q, want it to contain %q", got, want)
		}
	}
}

// scope() is how a tool reads back the namespace it was given. It cannot change
// it — this exists so a tool whose behaviour depends on the answer can ask
// rather than assume.
func TestStateScopeIsReadableFromTheGuest(t *testing.T) {
	dir := t.TempDir()
	writeTool(t, dir, "which", `
name = "which"
kind = "js"
entrypoint = "./which.js"
description = "Report the granted scope."

[capabilities]
state = true
`, `
import { scope } from "nine:state";
export default function () { return String(scope()); }
`)
	h := openStateHost(t, dir,
		map[string]Grant{"which": stateGrantFor(StateScopeTool)}, newFakeStateStore())

	if got := call(t, h, "which", `{}`); got != StateScopeTool {
		t.Fatalf("scope() = %q, want %q", got, StateScopeTool)
	}
}
