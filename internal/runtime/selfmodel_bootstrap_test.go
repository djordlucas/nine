package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nine/internal/memory"
	"nine/internal/memory/memtest"
)

func bootstrapStore(t *testing.T) *memory.Store {
	t.Helper()
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// writeBootstrap drops a self-model file in a temp dir and returns its path.
func writeBootstrap(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "self-model.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write bootstrap: %v", err)
	}
	return path
}

// A section becomes one key holding its fields, because that is the shape the
// self-model assembler reads. Per-field keys would store an identity no turn
// ever sees.
func TestBootstrapSelfModelWritesSectionKeys(t *testing.T) {
	store := bootstrapStore(t)
	path := writeBootstrap(t, `
[identity]
name = "Alice"
purpose = "Code review"

[persona]
description = "A senior Go engineer."
`)

	wrote, err := BootstrapSelfModel(store, path)
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if !wrote {
		t.Fatal("bootstrap reported no write")
	}

	got := store.KVGetString("self/identity")
	if !strings.Contains(got, "name: Alice") || !strings.Contains(got, "purpose: Code review") {
		t.Errorf("self/identity = %q, want both fields", got)
	}
	if got := store.KVGetString("self/persona"); !strings.Contains(got, "senior Go engineer") {
		t.Errorf("self/persona = %q", got)
	}
	if store.KVGetString(bootstrapSentinel) == "" {
		t.Error("sentinel not set")
	}
}

// Fields render in a fixed order, so the same file always produces the same
// text and a diff of the store is not noise.
func TestBootstrapSelfModelIsDeterministic(t *testing.T) {
	body := `
[identity]
zulu = "z"
alpha = "a"
mike = "m"
`
	first := bootstrapStore(t)
	if _, err := BootstrapSelfModel(first, writeBootstrap(t, body)); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	second := bootstrapStore(t)
	if _, err := BootstrapSelfModel(second, writeBootstrap(t, body)); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	got := first.KVGetString("self/identity")
	if got != second.KVGetString("self/identity") {
		t.Fatal("two runs of one file produced different text")
	}
	if want := "alpha: a\nmike: m\nzulu: z"; got != want {
		t.Errorf("self/identity = %q, want %q", got, want)
	}
}

// The file runs once per database. An instance that has revised its own
// self-model is not reset to the packaged text on the next restart.
func TestBootstrapSelfModelRunsOnce(t *testing.T) {
	store := bootstrapStore(t)
	path := writeBootstrap(t, "[identity]\nname = \"Alice\"\n")

	if _, err := BootstrapSelfModel(store, path); err != nil {
		t.Fatalf("first bootstrap: %v", err)
	}
	if err := store.Set("self/identity", "name: Alice, revised by reflection"); err != nil {
		t.Fatalf("set: %v", err)
	}

	wrote, err := BootstrapSelfModel(store, path)
	if err != nil {
		t.Fatalf("second bootstrap: %v", err)
	}
	if wrote {
		t.Error("bootstrap ran a second time")
	}
	if got := store.KVGetString("self/identity"); !strings.Contains(got, "revised by reflection") {
		t.Errorf("self/identity was overwritten: %q", got)
	}
}

// The packaged identity has to win over the generic default, which is the whole
// point of running first. BootstrapSelfKV skips a database that already has
// self/identity.
func TestBootstrapSelfModelBeatsTheGenericDefault(t *testing.T) {
	store := bootstrapStore(t)
	path := writeBootstrap(t, "[identity]\nname = \"Alice\"\n")

	if _, err := BootstrapSelfModel(store, path); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if err := BootstrapSelfKV(store, []string{"shell"}); err != nil {
		t.Fatalf("BootstrapSelfKV: %v", err)
	}

	if got := store.KVGetString("self/identity"); !strings.Contains(got, "Alice") {
		t.Errorf("generic default overwrote the packaged identity: %q", got)
	}
}

// No configured path is the shipped posture and must not be an error.
func TestBootstrapSelfModelUnconfigured(t *testing.T) {
	store := bootstrapStore(t)
	for _, path := range []string{"", "   "} {
		wrote, err := BootstrapSelfModel(store, path)
		if err != nil || wrote {
			t.Errorf("path %q: wrote=%v err=%v, want false/nil", path, wrote, err)
		}
	}
	if store.KVGetString(bootstrapSentinel) != "" {
		t.Error("sentinel set without a file")
	}
}

// A configured path that is absent is a deployment mistake, not a reason to
// refuse to boot: the instance runs with defaults and the log says why.
func TestBootstrapSelfModelMissingFile(t *testing.T) {
	store := bootstrapStore(t)
	wrote, err := BootstrapSelfModel(store, filepath.Join(t.TempDir(), "absent.toml"))
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if wrote {
		t.Error("reported a write for a missing file")
	}
}

// A malformed file stops the boot. Booting generic Nine under a personality's
// name is the failure this prevents.
func TestBootstrapSelfModelMalformed(t *testing.T) {
	store := bootstrapStore(t)
	if _, err := BootstrapSelfModel(store, writeBootstrap(t, "[identity\nname = ")); err == nil {
		t.Fatal("malformed TOML did not error")
	}
	if store.KVGetString(bootstrapSentinel) != "" {
		t.Error("sentinel set despite a parse failure")
	}
}

// A nested table has no sensible rendering into prose, so it is refused by name
// rather than flattened into something nobody wrote.
func TestBootstrapSelfModelNestedTableRefused(t *testing.T) {
	store := bootstrapStore(t)
	_, err := BootstrapSelfModel(store, writeBootstrap(t, "[identity.nested]\nname = \"x\"\n"))
	if err == nil {
		t.Fatal("nested table did not error")
	}
	if !strings.Contains(err.Error(), "nested") {
		t.Errorf("error should name the problem: %v", err)
	}
}

// Scalars other than strings reach the model as text, since that is all a
// self-model is.
func TestBootstrapSelfModelRendersScalars(t *testing.T) {
	store := bootstrapStore(t)
	path := writeBootstrap(t, `
[state]
prs_analyzed = 12
ratio = 0.5
active = true
focus = ["go", "review"]
`)
	if _, err := BootstrapSelfModel(store, path); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	got := store.KVGetString("self/state")
	for _, want := range []string{"prs_analyzed: 12", "ratio: 0.5", "active: true", "focus: go, review"} {
		if !strings.Contains(got, want) {
			t.Errorf("self/state = %q, missing %q", got, want)
		}
	}
}

// A top-level scalar sets its key directly, so a file can write self/identity
// without inventing a section for it.
func TestBootstrapSelfModelTopLevelScalar(t *testing.T) {
	store := bootstrapStore(t)
	path := writeBootstrap(t, "identity = \"Alice, a code review assistant.\"\n")
	if _, err := BootstrapSelfModel(store, path); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if got := store.KVGetString("self/identity"); got != "Alice, a code review assistant." {
		t.Errorf("self/identity = %q", got)
	}
}

// An empty file leaves the defaults in place rather than marking the database
// bootstrapped with nothing in it.
func TestBootstrapSelfModelEmptyFile(t *testing.T) {
	store := bootstrapStore(t)
	wrote, err := BootstrapSelfModel(store, writeBootstrap(t, "\n# nothing here\n"))
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if wrote {
		t.Error("reported a write for an empty file")
	}
	if store.KVGetString(bootstrapSentinel) != "" {
		t.Error("sentinel set for an empty file")
	}
}

// A nil store is the no-store deployment, not a crash.
func TestBootstrapSelfModelNilStore(t *testing.T) {
	if _, err := BootstrapSelfModel(nil, "/some/path.toml"); err != nil {
		t.Errorf("nil store should be a no-op: %v", err)
	}
}
