package memory_test

import (
	"encoding/json"
	"testing"

	"nine/internal/memory"
	"nine/internal/memory/memtest"
)

func openTools(t *testing.T) *memory.Store {
	t.Helper()
	s, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func write(t *testing.T, s *memory.Store, name, src string) {
	t.Helper()
	if err := s.GeneratedToolUpsert(memory.GeneratedTool{
		Name: name, Description: name + " does a thing.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		Source:      src,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestGeneratedToolRoundTrip(t *testing.T) {
	s := openTools(t)
	write(t, s, "iso_week_of", `export default () => 1;`)

	got, found, err := s.GeneratedToolGet("iso_week_of")
	if err != nil || !found {
		t.Fatalf("get: err=%v found=%v", err, found)
	}
	if got.Source != `export default () => 1;` {
		t.Errorf("Source = %q", got.Source)
	}
	if got.CallCount != 0 || got.LastCalledAt != "" {
		t.Errorf("a fresh tool has usage: count=%d last=%q", got.CallCount, got.LastCalledAt)
	}
}

// tool_write on an existing name replaces rather than duplicating — that is what
// stops the catalog filling with near-identical variants as the agent iterates.
func TestUpsertReplacesRatherThanDuplicating(t *testing.T) {
	s := openTools(t)
	write(t, s, "thing", `export default () => 1;`)
	write(t, s, "thing", `export default () => 2;`)

	n, err := s.GeneratedToolCount()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("count = %d, want 1", n)
	}
	got, _, _ := s.GeneratedToolGet("thing") //nolint:errcheck
	if got.Source != `export default () => 2;` {
		t.Errorf("Source = %q, want the rewrite", got.Source)
	}
}

// Usage survives a rewrite. Resetting it would make the most actively maintained
// tools the most likely to be evicted, which is exactly backwards.
func TestUsageSurvivesARewrite(t *testing.T) {
	s := openTools(t)
	write(t, s, "thing", `export default () => 1;`)
	for i := 0; i < 3; i++ {
		if err := s.GeneratedToolTouch("thing"); err != nil {
			t.Fatal(err)
		}
	}
	write(t, s, "thing", `export default () => 2;`)

	got, _, _ := s.GeneratedToolGet("thing") //nolint:errcheck
	if got.CallCount != 3 {
		t.Errorf("CallCount = %d, want the history preserved", got.CallCount)
	}
}

// Eviction is least-recently-called first, so a tool that earns its place keeps
// it (docs/sandboxed-tools.md §9.2).
func TestEvictionDropsTheLeastRecentlyCalled(t *testing.T) {
	s := openTools(t)
	for _, n := range []string{"a", "b", "c", "d"} {
		write(t, s, n, `export default () => 1;`)
	}
	// c and a are used; b and d are never called.
	if err := s.GeneratedToolTouch("c"); err != nil {
		t.Fatal(err)
	}
	if err := s.GeneratedToolTouch("a"); err != nil {
		t.Fatal(err)
	}

	evicted, err := s.GeneratedToolEvictOldest(2)
	if err != nil {
		t.Fatal(err)
	}
	if len(evicted) != 2 {
		t.Fatalf("evicted %v, want 2", evicted)
	}
	for _, n := range evicted {
		if n == "a" || n == "c" {
			t.Errorf("evicted %q, which had been called", n)
		}
	}
	if _, found, _ := s.GeneratedToolGet("a"); !found { //nolint:errcheck
		t.Error("a called tool was evicted")
	}
	if _, found, _ := s.GeneratedToolGet("c"); !found { //nolint:errcheck
		t.Error("a called tool was evicted")
	}
}

// Under the cap, eviction does nothing.
func TestEvictionIsANoOpUnderTheCap(t *testing.T) {
	s := openTools(t)
	write(t, s, "a", `export default () => 1;`)

	evicted, err := s.GeneratedToolEvictOldest(64)
	if err != nil {
		t.Fatal(err)
	}
	if len(evicted) != 0 {
		t.Errorf("evicted %v under the cap", evicted)
	}
}

func TestGeneratedToolDelete(t *testing.T) {
	s := openTools(t)
	write(t, s, "scratch", `export default () => 1;`)
	if err := s.GeneratedToolDelete("scratch"); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := s.GeneratedToolGet("scratch"); found { //nolint:errcheck
		t.Error("the tool survived deletion")
	}
}
