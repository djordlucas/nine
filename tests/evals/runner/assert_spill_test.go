package runner

import (
	"strings"
	"testing"

	"nine/internal/llm"
)

// answered is a scripted run that calls no tool: these cases assert over files
// seeded into the store by setup, not over anything the model does. Seeding
// used to go through file_store, which is retired — the daemon is the only
// writer of that table now (adr/file-namespaces.md).
func answered() []llm.Response {
	return []llm.Response{{Text: "done", StopReason: "end_turn"}}
}

func TestGrade_StoredFiles_ExactPath(t *testing.T) {
	c := &Case{
		ID:      "grade-stored-files-exact",
		Prompts: []string{"store it"},
		Setup:  Setup{StoredFiles: map[string]string{"archive/seq.txt": "1 2 3 9999 10000"}},
		Expect: Expect{SideEffects: SideEffects{
			StoredFiles: map[string]StringMatch{"archive/seq.txt": {Contains: "9999"}},
		}},
	}
	g := runCase(t, c, answered())
	if !g.Pass {
		t.Errorf("expected pass, got failures: %v", g.Failures)
	}
}

// The prefix form is what a spill assertion needs: the random suffix in
// spill/<agent>/<tool>-<rand>.txt cannot be named by a case.
func TestGrade_StoredFiles_PrefixMatchesUnpredictablePath(t *testing.T) {
	c := &Case{
		ID:      "grade-stored-files-prefix",
		Prompts: []string{"store it"},
		Setup:   Setup{StoredFiles: map[string]string{"spill-like/agent-1/shell-abc123.txt": "hay needle hay"}},
		Expect: Expect{SideEffects: SideEffects{
			StoredFiles: map[string]StringMatch{"spill/": {Contains: "needle"}},
		}},
	}
	g := runCase(t, c, answered())
	if g.Pass {
		t.Fatal("a file outside the prefix must not satisfy the assertion")
	}

	c2 := &Case{
		ID:      "grade-stored-files-prefix-hit",
		Prompts: []string{"store it"},
		Setup:   Setup{StoredFiles: map[string]string{"reports/agent-1/shell-abc123.txt": "hay needle hay"}},
		Expect: Expect{SideEffects: SideEffects{
			StoredFiles: map[string]StringMatch{"reports/": {Contains: "needle"}},
		}},
	}
	if g := runCase(t, c2, answered()); !g.Pass {
		t.Errorf("expected the prefix to match an unpredictable path, got: %v", g.Failures)
	}
}

func TestGrade_StoredFiles_AbsentAndUnmatched(t *testing.T) {
	absent := &Case{
		ID:      "grade-stored-files-absent",
		Prompts: []string{"store it"},
		Setup:   Setup{StoredFiles: map[string]string{"somewhere/a.txt": "x"}},
		Expect: Expect{SideEffects: SideEffects{
			StoredFiles: map[string]StringMatch{"nowhere/": {Contains: "x"}},
		}},
	}
	g := runCase(t, absent, answered())
	if g.Pass {
		t.Fatal("expected a failure when no file exists under the prefix")
	}
	if !strings.Contains(strings.Join(g.Failures, " "), "no stored file") {
		t.Errorf("failure %v should say no file was found under the prefix", g.Failures)
	}

	unmatched := &Case{
		ID:      "grade-stored-files-unmatched",
		Prompts: []string{"store it"},
		Setup:   Setup{StoredFiles: map[string]string{"reports/a.txt": "some other content"}},
		Expect: Expect{SideEffects: SideEffects{
			StoredFiles: map[string]StringMatch{"reports/": {Contains: "missing-token"}},
		}},
	}
	if g := runCase(t, unmatched, answered()); g.Pass {
		t.Error("expected a failure when the file exists but does not match")
	}
}

// The spills assertion reads tool_end.spill_path out of the journal. A
// scripted run produces small outputs, so nothing spills and a min of 1 must
// fail — which is exactly what protects a case from silently passing when the
// tool result happened to fit under the cap.
func TestGrade_Spills_NoneWhenOutputsAreSmall(t *testing.T) {
	c := &Case{
		ID:      "grade-spills-none",
		Prompts: []string{"do it"},
		Expect: Expect{Trajectory: Trajectory{
			Spills: &CountExpect{Min: 1},
		}},
	}
	g := runCase(t, c, answered())
	if g.Pass {
		t.Fatal("expected a failure: no output was large enough to spill")
	}
	if !strings.Contains(strings.Join(g.Failures, " "), "spilled") {
		t.Errorf("failure %v should mention spilling", g.Failures)
	}
}

func TestGrade_Spills_ZeroMinAlwaysHolds(t *testing.T) {
	c := &Case{
		ID:      "grade-spills-zero",
		Prompts: []string{"do it"},
		Expect: Expect{Trajectory: Trajectory{
			Spills:  &CountExpect{Min: 0},
			NoStall: true,
		}},
	}
	if g := runCase(t, c, answered()); !g.Pass {
		t.Errorf("a min of 0 should always hold, got: %v", g.Failures)
	}
}

// A case asserting only stored_files or only spills is still a valid case.
func TestExpectHasAny_CoversNewAssertions(t *testing.T) {
	stored := Expect{SideEffects: SideEffects{
		StoredFiles: map[string]StringMatch{"spill/": {Contains: "x"}},
	}}
	if !stored.hasAny() {
		t.Error("stored_files alone should count as an assertion")
	}
	spills := Expect{Trajectory: Trajectory{Spills: &CountExpect{Min: 1}}}
	if !spills.hasAny() {
		t.Error("spills alone should count as an assertion")
	}
}

func TestValidate_StoredFilesMatcher(t *testing.T) {
	c := &Case{
		ID:      "bad-stored-files",
		Prompts: []string{"x"},
		Expect: Expect{SideEffects: SideEffects{
			StoredFiles: map[string]StringMatch{"spill/": {}}, // no predicate
		}},
	}
	c.defaults()
	if err := c.validate(); err == nil {
		t.Error("expected an empty stored_files matcher to be rejected at load")
	}
}
