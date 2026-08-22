package ninectx_test

import (
	"encoding/json"
	"strings"
	"testing"

	ninectx "nine/internal/context"
	"nine/internal/llm"
)

// makeVec returns a unit vector of dimension n with 1.0 at position pos.
func makeVec(n, pos int) []float32 {
	v := make([]float32, n)
	v[pos] = 1.0
	return v
}

// chars returns a string of n copies of c.
func chars(n int, c byte) string { return strings.Repeat(string(c), n) }

// tokStr returns a string that costs exactly t tokens (±1) in the 4-chars/token model.
// tokStr builds a string the builder counts as t tokens. It asks the estimator
// rather than multiplying by a hardcoded ratio, so recalibrating the
// bytes-per-token constant does not silently change what these tests assert:
// the old helper assumed 4 bytes per token, and a smaller divisor made every
// sized input overflow its budget.
func tokStr(t int) string {
	s := chars(t*4, 'x') // 4 is an upper bound on bytes per token; trim down
	for len(s) > 0 && ninectx.EstimateTokens(s) > t {
		s = s[:len(s)-1]
	}
	return s
}

func TestBuildSystemCoreAlwaysPresent(t *testing.T) {
	core := tokStr(800) // 800 tokens
	b := ninectx.New(ninectx.Config{Budget: 1000})
	req := b.Build(ninectx.BuildInput{SystemCore: core})
	if !strings.HasPrefix(req.System, core) {
		t.Error("system core was trimmed")
	}
}

func TestBuildExtraDroppedWhenTight(t *testing.T) {
	core := chars(3600, 'c')  // 900 tokens
	extras := chars(800, 'e') // marker string distinct from core
	b := ninectx.New(ninectx.Config{Budget: 1000, ExtrasBudget: 200})
	// remaining after core = 100 tokens; extrasBudget = 200 → extras dropped
	req := b.Build(ninectx.BuildInput{
		SystemCore:   core,
		SystemExtras: extras,
	})
	if strings.Contains(req.System, extras) {
		t.Error("extras should have been dropped but were included")
	}
}

func TestBuildExtrasIncludedWhenRoomAvailable(t *testing.T) {
	core := tokStr(400) // 400 tokens
	extras := "EXTRAS_MARKER"
	b := ninectx.New(ninectx.Config{Budget: 1000, ExtrasBudget: 200})
	req := b.Build(ninectx.BuildInput{
		SystemCore:   core,
		SystemExtras: extras,
	})
	if !strings.Contains(req.System, extras) {
		t.Error("extras should have been included")
	}
}

func TestBuildReportMatchesAssembly(t *testing.T) {
	core := tokStr(400)
	b := ninectx.New(ninectx.Config{Budget: 1000, ExtrasBudget: 200})
	input := ninectx.BuildInput{
		SystemCore:   core,
		SystemExtras: "EXTRAS_MARKER",
		History:      []llm.Message{{Role: "user", Text: tokStr(50)}},
	}

	// BuildReport must produce the exact same assembled request/usage as
	// BuildWithUsage — they share one implementation.
	req, used := b.BuildWithUsage(input)
	rep := b.BuildReport(input)
	if rep.Used != used {
		t.Errorf("report.Used = %d, want %d (BuildWithUsage)", rep.Used, used)
	}
	if rep.Budget != 1000 {
		t.Errorf("report.Budget = %d, want 1000", rep.Budget)
	}
	if rep.System != req.System {
		t.Errorf("report.System diverged from assembled request system")
	}
	if len(rep.Messages) != len(req.Messages) {
		t.Errorf("report.Messages = %d, want %d", len(rep.Messages), len(req.Messages))
	}

	// Every priority band is represented as a section, in order.
	wantNames := []string{"system-core", "tools", "self-model", "enrichment", "history", "scratchpad", "plan", "extras"}
	if len(rep.Sections) != len(wantNames) {
		t.Fatalf("sections = %d, want %d (%v)", len(rep.Sections), len(wantNames), rep.Sections)
	}
	for i, name := range wantNames {
		if rep.Sections[i].Name != name {
			t.Errorf("section[%d] = %q, want %q", i, rep.Sections[i].Name, name)
		}
	}
	// system-core is always included and non-zero; history is included here.
	if !rep.Sections[0].Included || rep.Sections[0].Tokens == 0 {
		t.Errorf("system-core section = %+v, want included & non-zero", rep.Sections[0])
	}
	if !sectionByName(rep, "history").Included {
		t.Errorf("history section should be included")
	}
}

func sectionByName(rep ninectx.Report, name string) ninectx.Section {
	for _, s := range rep.Sections {
		if s.Name == name {
			return s
		}
	}
	return ninectx.Section{}
}

func TestBuildHistoryTrimming(t *testing.T) {
	core := tokStr(800) // 800 tokens; 200 remaining
	// Two messages: old (300 tokens) and recent (50 tokens).
	old := llm.Message{Role: "user", Text: tokStr(300)}
	recent := llm.Message{Role: "assistant", Text: tokStr(50)}

	b := ninectx.New(ninectx.Config{Budget: 1000})
	req := b.Build(ninectx.BuildInput{
		SystemCore: core,
		History:    []llm.Message{old, recent},
	})

	// recent fits (50 < 200), old does not (300 > 200 - 50 = 150 remaining after recent).
	// Actually: remaining=200, recent costs 50, leaving 150 for old (300 > 150) → old dropped.
	hasOld := false
	hasRecent := false
	for _, m := range req.Messages {
		if m.Text == old.Text {
			hasOld = true
		}
		if m.Text == recent.Text {
			hasRecent = true
		}
	}
	if hasOld {
		t.Error("old message should have been dropped")
	}
	if !hasRecent {
		t.Error("recent message should have been kept")
	}
}

func TestBuildScratchpadTrimming(t *testing.T) {
	core := tokStr(800)                                  // 200 tokens left
	old := ninectx.ScratchpadEntry{Thought: tokStr(250)} // too big
	recent := ninectx.ScratchpadEntry{Thought: tokStr(50)}

	b := ninectx.New(ninectx.Config{Budget: 1000})
	req := b.Build(ninectx.BuildInput{
		SystemCore: core,
		Scratchpad: []ninectx.ScratchpadEntry{old, recent},
	})

	// Find recent entry in messages.
	foundRecent := false
	for _, m := range req.Messages {
		if m.Text == recent.Thought {
			foundRecent = true
		}
		if m.Text == old.Thought {
			t.Error("old scratchpad entry should have been dropped")
		}
	}
	if !foundRecent {
		t.Error("recent scratchpad entry should be present")
	}
}

func TestBuildToolRelevance(t *testing.T) {
	// 10 tools with orthogonal one-hot vectors.
	// Query is similar to tools 0 and 1.
	dim := 10
	var tools []ninectx.ToolWithVector
	for i := range 10 {
		tools = append(tools, ninectx.ToolWithVector{
			Tool: llm.ToolDef{
				Name:        "tool" + string(rune('A'+i)),
				Description: "desc",
				InputSchema: json.RawMessage(`{}`),
			},
			Vector: makeVec(dim, i),
		})
	}

	// Query points at tools 0 and 1 equally.
	query := make([]float32, dim)
	query[0] = 1.0
	query[1] = 1.0

	b := ninectx.New(ninectx.Config{Budget: 10000, ToolTopN: 2})
	req := b.Build(ninectx.BuildInput{
		SystemCore:  "system",
		Tools:       tools,
		QueryVector: query,
	})

	if len(req.Tools) != 2 {
		t.Fatalf("expected 2 tools (ToolTopN=2), got %d: %v", len(req.Tools), req.Tools)
	}
	got := make(map[string]bool)
	for _, t := range req.Tools {
		got[t.Name] = true
	}
	if !got["toolA"] || !got["toolB"] {
		t.Errorf("expected toolA and toolB (most similar), got %v", req.Tools)
	}
}

func TestBuildAlwaysToolsExempt(t *testing.T) {
	// 5 normal tools and 2 "memory_*" always-include tools.
	// Budget allows only 2 non-always tools (ToolTopN=2).
	// Query is similar to normal tools only.
	dim := 7
	var tools []ninectx.ToolWithVector
	for i := range 5 {
		tools = append(tools, ninectx.ToolWithVector{
			Tool:   llm.ToolDef{Name: "normal_" + string(rune('a'+i)), Description: "d", InputSchema: json.RawMessage(`{}`)},
			Vector: makeVec(dim, i),
		})
	}
	// Always-include tools with zero-similarity vectors.
	for _, name := range []string{"memory_get", "memory_set"} {
		tools = append(tools, ninectx.ToolWithVector{
			Tool:   llm.ToolDef{Name: name, Description: "d", InputSchema: json.RawMessage(`{}`)},
			Vector: makeVec(dim, 5), // low similarity to query
		})
	}

	// Query similar to normal_a only.
	query := makeVec(dim, 0)

	b := ninectx.New(ninectx.Config{
		Budget:      10000,
		ToolTopN:    2,
		AlwaysTools: []string{"memory_get", "memory_set"},
	})
	req := b.Build(ninectx.BuildInput{
		SystemCore:  "s",
		Tools:       tools,
		QueryVector: query,
	})

	got := make(map[string]bool)
	for _, t := range req.Tools {
		got[t.Name] = true
	}
	if !got["memory_get"] {
		t.Error("memory_get should always be included")
	}
	if !got["memory_set"] {
		t.Error("memory_set should always be included")
	}
	// Also 2 non-always tools (ToolTopN=2).
	nonAlways := 0
	for _, t := range req.Tools {
		if t.Name != "memory_get" && t.Name != "memory_set" {
			nonAlways++
		}
	}
	if nonAlways != 2 {
		t.Errorf("expected 2 non-always tools, got %d", nonAlways)
	}
}

func TestBuildScratchpadWithToolCall(t *testing.T) {
	// Scratchpad entry with a tool call becomes two messages: assistant (with
	// tool_use) and user (with tool_result).
	entry := ninectx.ScratchpadEntry{
		Thought:     "I need to check the weather.",
		ToolName:    "get_weather",
		ToolCallID:  "call_1",
		ToolArgs:    json.RawMessage(`{"city":"Paris"}`),
		Observation: `{"temp": 20}`,
	}
	b := ninectx.New(ninectx.Config{Budget: 10000})
	req := b.Build(ninectx.BuildInput{
		SystemCore: "system",
		Scratchpad: []ninectx.ScratchpadEntry{entry},
	})

	if len(req.Messages) != 2 {
		t.Fatalf("expected 2 messages for tool-call entry, got %d", len(req.Messages))
	}
	aMsg := req.Messages[0]
	uMsg := req.Messages[1]

	if aMsg.Role != "assistant" || aMsg.Text != entry.Thought {
		t.Errorf("assistant message = %+v", aMsg)
	}
	if len(aMsg.ToolCalls) != 1 || aMsg.ToolCalls[0].Name != "get_weather" {
		t.Errorf("assistant tool call = %+v", aMsg.ToolCalls)
	}
	if uMsg.Role != "user" || len(uMsg.ToolResults) != 1 {
		t.Errorf("user message = %+v", uMsg)
	}
	if uMsg.ToolResults[0].Content != entry.Observation {
		t.Errorf("tool result content = %q", uMsg.ToolResults[0].Content)
	}
}

func TestBuildPriorityOrder(t *testing.T) {
	// Fill nearly all the budget with core (900/1000 tokens).
	// Verify: extras dropped, scratchpad dropped, single small history message kept.
	core := tokStr(900) // 900 tokens; 100 left
	// Small history message that fits.
	smallMsg := llm.Message{Role: "user", Text: tokStr(40)}
	// Scratchpad entry that would push us over if included.
	bigEntry := ninectx.ScratchpadEntry{Thought: tokStr(80)}
	extras := "SHOULD_NOT_APPEAR"

	b := ninectx.New(ninectx.Config{Budget: 1000, ExtrasBudget: 200})
	req := b.Build(ninectx.BuildInput{
		SystemCore:   core,
		SystemExtras: extras,
		History:      []llm.Message{smallMsg},
		Scratchpad:   []ninectx.ScratchpadEntry{bigEntry},
	})

	if strings.Contains(req.System, extras) {
		t.Error("extras should be dropped (priority 5)")
	}
	hasSmallMsg := false
	for _, m := range req.Messages {
		if m.Text == smallMsg.Text {
			hasSmallMsg = true
		}
		if m.Text == bigEntry.Thought {
			t.Error("big scratchpad entry should be dropped (priority 4)")
		}
	}
	if !hasSmallMsg {
		t.Error("small history message should be kept (priority 3)")
	}
}

func TestBuildEnrichmentIncludedWhenRoomAvailable(t *testing.T) {
	b := ninectx.New(ninectx.Config{Budget: 1000})
	req := b.Build(ninectx.BuildInput{
		SystemCore:       tokStr(100),
		SystemEnrichment: "RELATED_SESSION_MARKER",
	})
	if !strings.Contains(req.System, "RELATED_SESSION_MARKER") {
		t.Error("enrichment should be surfaced when budget allows")
	}
}

func TestBuildEnrichmentDroppedWhenTight(t *testing.T) {
	// Core alone nearly exhausts the budget, leaving no room for enrichment.
	b := ninectx.New(ninectx.Config{Budget: 1000})
	req := b.Build(ninectx.BuildInput{
		SystemCore:       tokStr(1000),
		SystemEnrichment: "RELATED_SESSION_MARKER",
	})
	if strings.Contains(req.System, "RELATED_SESSION_MARKER") {
		t.Error("enrichment should be dropped when budget is tight")
	}
}

func TestBuildEnrichmentCapped(t *testing.T) {
	// A 500-token enrichment is capped at 300 tokens (~1200 chars) so it can
	// never crowd out the turn even with ample budget.
	b := ninectx.New(ninectx.Config{Budget: 100_000})
	huge := tokStr(500)
	req := b.Build(ninectx.BuildInput{
		SystemCore:       tokStr(10),
		SystemEnrichment: huge,
	})
	if strings.Contains(req.System, huge) {
		t.Error("enrichment should be truncated to the cap, not included whole")
	}
	if !strings.Contains(req.System, "x") {
		t.Error("a truncated prefix of the enrichment should still be present")
	}
}

func TestBuildPlanIncludedWhenRoomAvailable(t *testing.T) {
	b := ninectx.New(ninectx.Config{Budget: 20000})
	req := b.Build(ninectx.BuildInput{
		SystemCore: tokStr(10000),
		SystemPlan: "PLAN_MARKER",
	})
	if !strings.Contains(req.System, "PLAN_MARKER") {
		t.Error("plan should be included when budget allows")
	}
}

func TestBuildPlanDroppedWhenTight(t *testing.T) {
	b := ninectx.New(ninectx.Config{Budget: 300})

	input := ninectx.BuildInput{
		SystemCore: tokStr(50),
		SystemPlan: tokStr(400) + "PLAN_MARKER",
		Scratchpad: []ninectx.ScratchpadEntry{{
			ToolName:    "test-tool",
			Observation: tokStr(50) + "SCRATCH_MARKER",
			Thought:     "",
		}},
	}

	req := b.Build(input)

	found := false
	for _, m := range req.Messages {
		for _, r := range m.ToolResults {
			if strings.Contains(r.Content, "SCRATCH_MARKER") {
				found = true
			}
		}
	}
	if !found {
		t.Error("scratchpad should be retained when the plan is dropped")
	}

	if strings.Contains(req.System, "PLAN_MARKER") {
		t.Error("plan should yield (be dropped) rather than crowd out scratchpad")
	}
}

func TestBuildReportIncludesPlanSection(t *testing.T) {
	b := ninectx.New(ninectx.Config{Budget: 10000})

	input := ninectx.BuildInput{
		SystemCore: tokStr(1001),
		SystemPlan: "PLAN_MARKER",
	}

	rep := b.BuildReport(input)

	for _, section := range rep.Sections {
		if section.Name == "plan" && section.Included {
			return
		}
	}
	t.Error("plan section not found in report")
}
