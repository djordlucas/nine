package runner

import (
	"strings"
	"testing"

	"nine/internal/llm"
)

// storeBig is a scripted trajectory that writes a large file into the store and
// then answers. The file is written through file_store (not spilled) so the
// stored_files assertion can be exercised without a live over-cap tool.
func storeBig(path, content string) []llm.Response {
	return []llm.Response{
		{ToolCalls: []llm.ToolCall{toolCall("1", "file_store", map[string]any{
			"path": path, "content": content,
		})}, StopReason: "tool_use"},
		{Text: "stored", StopReason: "end_turn"},
	}
}

func TestGrade_StoredFiles_ExactPath(t *testing.T) {
	c := &Case{
		ID:      "grade-stored-files-exact",
		Prompts: []string{"store it"},
		Expect: Expect{SideEffects: SideEffects{
			StoredFiles: map[string]StringMatch{"archive/seq.txt": {Contains: "9999"}},
		}},
	}
	g := runCase(t, c, storeBig("archive/seq.txt", "1 2 3 9999 10000"))
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
		Expect: Expect{SideEffects: SideEffects{
			StoredFiles: map[string]StringMatch{"spill/": {Contains: "needle"}},
		}},
	}
	g := runCase(t, c, storeBig("spill-like/agent-1/shell-abc123.txt", "hay needle hay"))
	if g.Pass {
		t.Fatal("a file outside the prefix must not satisfy the assertion")
	}

	c2 := &Case{
		ID:      "grade-stored-files-prefix-hit",
		Prompts: []string{"store it"},
		Expect: Expect{SideEffects: SideEffects{
			StoredFiles: map[string]StringMatch{"reports/": {Contains: "needle"}},
		}},
	}
	if g := runCase(t, c2, storeBig("reports/agent-1/shell-abc123.txt", "hay needle hay")); !g.Pass {
		t.Errorf("expected the prefix to match an unpredictable path, got: %v", g.Failures)
	}
}

func TestGrade_StoredFiles_AbsentAndUnmatched(t *testing.T) {
	absent := &Case{
		ID:      "grade-stored-files-absent",
		Prompts: []string{"store it"},
		Expect: Expect{SideEffects: SideEffects{
			StoredFiles: map[string]StringMatch{"nowhere/": {Contains: "x"}},
		}},
	}
	g := runCase(t, absent, storeBig("somewhere/a.txt", "x"))
	if g.Pass {
		t.Fatal("expected a failure when no file exists under the prefix")
	}
	if !strings.Contains(strings.Join(g.Failures, " "), "no stored file") {
		t.Errorf("failure %v should say no file was found under the prefix", g.Failures)
	}

	unmatched := &Case{
		ID:      "grade-stored-files-unmatched",
		Prompts: []string{"store it"},
		Expect: Expect{SideEffects: SideEffects{
			StoredFiles: map[string]StringMatch{"reports/": {Contains: "missing-token"}},
		}},
	}
	if g := runCase(t, unmatched, storeBig("reports/a.txt", "some other content")); g.Pass {
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
	g := runCase(t, c, storeBig("small.txt", "tiny"))
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
	if g := runCase(t, c, storeBig("small.txt", "tiny")); !g.Pass {
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
