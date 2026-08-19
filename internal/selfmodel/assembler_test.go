package selfmodel_test

import (
	"context"
	"strings"
	"testing"

	"nine/internal/memory/memtest"
	"nine/internal/selfmodel"
)

func newAssembler(t *testing.T, runtime string, tools []string) *selfmodel.Assembler {
	t.Helper()
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	return selfmodel.New(store, nil, func() []string { return tools }, runtime)
}

// The Environment block reports the runtime it was given, verbatim. It is passed
// in rather than probed so that the answer is decided once, at boot, by config —
// which is also the only place an operator can correct it.
func TestBuildReportsInjectedRuntime(t *testing.T) {
	for _, want := range []string{"host", "Docker container", "Podman container", "Kubernetes pod"} {
		t.Run(want, func(t *testing.T) {
			got := newAssembler(t, want, nil).Build(context.Background(), nil)
			if !strings.Contains(got, "Runtime: "+want) {
				t.Errorf("block = %q, want it to report %q", got, want)
			}
		})
	}
}

// An empty runtime omits the line rather than guessing. Claiming "host" when the
// daemon is actually confined is the specific error worth avoiding: it invites
// the model to reason about the operator's machine.
func TestBuildOmitsRuntimeWhenUnknown(t *testing.T) {
	got := newAssembler(t, "", nil).Build(context.Background(), nil)
	if strings.Contains(got, "Runtime:") {
		t.Errorf("block = %q, want no Runtime line when the runtime is unknown", got)
	}
	if strings.Contains(got, "host") {
		t.Errorf("block = %q, want no claim of running on the host", got)
	}
}

func TestBuildListsTools(t *testing.T) {
	got := newAssembler(t, "host", []string{"echo", "read_file"}).Build(context.Background(), nil)
	if !strings.Contains(got, "Tools (2):") {
		t.Errorf("block = %q, want a tool count", got)
	}
	for _, tool := range []string{"echo", "read_file"} {
		if !strings.Contains(got, tool) {
			t.Errorf("block = %q, want it to name %q", got, tool)
		}
	}
}

// No tools means no Tools line — an empty list would read as a capability claim.
func TestBuildOmitsEmptyToolList(t *testing.T) {
	got := newAssembler(t, "host", nil).Build(context.Background(), nil)
	if strings.Contains(got, "Tools") {
		t.Errorf("block = %q, want no Tools line when none are loaded", got)
	}
}

// The self/* KV keys are the durable product of a reflection turn; the assembler
// reads them back every turn, which is what makes reflection persist.
func TestBuildIncludesSelfKnowledgeFromKV(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set("self/identity", "I am nine."); err != nil {
		t.Fatal(err)
	}
	if err := store.Set("self/learned", "Prod DB is db.prod.example.com."); err != nil {
		t.Fatal(err)
	}
	// Not a self/* key: must not leak into the self-model block.
	if err := store.Set("notes/secret", "should not appear"); err != nil {
		t.Fatal(err)
	}

	a := selfmodel.New(store, nil, func() []string { return nil }, "host")
	got := a.Build(context.Background(), nil)

	for _, want := range []string{"self/identity", "I am nine.", "self/learned", "db.prod.example.com"} {
		if !strings.Contains(got, want) {
			t.Errorf("block = %q, want it to contain %q", got, want)
		}
	}
	if strings.Contains(got, "should not appear") {
		t.Error("a non-self/* key leaked into the self-model block")
	}
	// An unset self/* key contributes no empty heading.
	if strings.Contains(got, "self/capabilities") {
		t.Errorf("block = %q, want no heading for an unset key", got)
	}
}

// Without a query vector there is nothing to rank against, so the skills section
// is skipped entirely.
func TestBuildSkipsSkillsWithoutQueryVector(t *testing.T) {
	got := newAssembler(t, "host", nil).Build(context.Background(), nil)
	if strings.Contains(got, "Relevant skills") {
		t.Errorf("block = %q, want no skills section without a query vector", got)
	}
}

// A query vector with no matching skills still must not emit an empty heading.
func TestBuildSkipsSkillsSectionWhenNoneMatch(t *testing.T) {
	got := newAssembler(t, "host", nil).Build(context.Background(), []float32{0.1, 0.2, 0.3})
	if strings.Contains(got, "Relevant skills") {
		t.Errorf("block = %q, want no skills heading when nothing matched", got)
	}
}

// Build is called on every turn, so it must not accumulate or mutate state:
// two calls with the same inputs produce the same block.
func TestBuildIsRepeatable(t *testing.T) {
	a := newAssembler(t, "host", []string{"echo"})
	first := a.Build(context.Background(), nil)
	second := a.Build(context.Background(), nil)
	if first != second {
		t.Errorf("Build is not repeatable:\nfirst  = %q\nsecond = %q", first, second)
	}
}
