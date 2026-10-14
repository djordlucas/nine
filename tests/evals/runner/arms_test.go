package runner

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"nine/internal/memory"
)

func TestParseArms(t *testing.T) {
	if arms, err := ParseArms(""); err != nil || arms != nil {
		t.Errorf(`ParseArms("") = %v, %v; want none`, arms, err)
	}
	if arms, err := ParseArms(" ranked "); err != nil || len(arms) != 1 || arms[0] != ArmRanked {
		t.Errorf("ParseArms(ranked) = %v, %v", arms, err)
	}
	// deferred is not an arm until the mode exists, so a run cannot ask for it
	// and silently get ranked.
	if _, err := ParseArms("ranked,deferred"); err == nil {
		t.Error("an unknown arm must be refused")
	}
	if _, err := ParseArms("ranked,ranked"); err == nil {
		t.Error("a repeated arm must be refused")
	}
}

func TestApplicableArms(t *testing.T) {
	free := &Case{}
	pinned := &Case{Session: Session{Config: map[string]any{ArmKey: ArmRanked}}}
	for _, tc := range []struct {
		name      string
		c         *Case
		requested []string
		want      string
	}{
		{"no arms runs once", free, nil, ""},
		{"every requested arm", free, []string{ArmRanked}, ArmRanked},
		{"a pinned case runs in its arm", pinned, []string{ArmRanked}, ArmRanked},
		{"a pinned case outside the request is not run", pinned, []string{"other"}, "<none>"},
	} {
		got := strings.Join(tc.c.ApplicableArms(tc.requested), ",")
		if got == "" && len(tc.c.ApplicableArms(tc.requested)) == 0 {
			got = "<none>"
		}
		if got != tc.want {
			t.Errorf("%s: ApplicableArms = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestWithArm(t *testing.T) {
	c := &Case{Session: Session{Config: map[string]any{"tools.agent.enabled": true}}}
	got := c.withArm(ArmRanked)
	if got.PinnedArm() != ArmRanked || got.Session.Config["tools.agent.enabled"] != true {
		t.Errorf("withArm config = %v", got.Session.Config)
	}
	if c.PinnedArm() != "" {
		t.Error("withArm must not change the case it copies")
	}
	if c.withArm("") != c {
		t.Error(`withArm("") must return the case itself`)
	}
}

func TestCaseValidate_Arm(t *testing.T) {
	c := &Case{ID: "arm", Prompts: []string{"x"}, Expect: Expect{Answer: Answer{Contains: []string{"x"}}},
		Session: Session{Config: map[string]any{ArmKey: "deferred"}}}
	c.defaults()
	if err := c.validate(); err == nil {
		t.Error("a case pinned to an unknown arm must not load")
	}
}

// event builds a journal event with a JSON payload.
func event(typ, span string, payload any) memory.SessionEvent {
	b, _ := json.Marshal(payload)
	return memory.SessionEvent{Type: typ, SpanID: span, Turn: 1, Payload: b}
}

// exposureEvents is a one-turn journal: two LLM calls, one tool_search that
// returned restore_file, one promotion, and a restore_file call.
func exposureEvents() []memory.SessionEvent {
	return []memory.SessionEvent{
		event("turn_start", "t1", map[string]any{"trigger": "user"}),
		event("llm_request", "t1.llm1", map[string]any{
			"tool_names": []string{"read_file", "tool_search"}, "tool_index": []string{"restore_file", "trash_list"},
			"tool_tokens": 1800, "tokens_used": 5000}),
		event("llm_response", "t1.llm1", map[string]any{"input_tokens": 5200}),
		event("tool_start", "t1.tool1", map[string]any{"name": "tool_search"}),
		event("tool_end", "t1.tool1", map[string]any{"name": "tool_search",
			"output": `{"results":[{"name":"restore_file","description":"x"}]}`}),
		event("tool_promoted", "t1", map[string]any{"name": "restore_file"}),
		event("llm_request", "t1.llm2", map[string]any{
			"tool_names": []string{"read_file", "tool_search", "restore_file"}, "tool_index": []string{"trash_list"},
			"tool_tokens": 2100, "tokens_used": 5600}),
		event("llm_response", "t1.llm2", map[string]any{}),
		event("tool_start", "t1.tool2", map[string]any{"name": "restore_file"}),
		event("turn_end", "t1", map[string]any{}),
	}
}

func TestTraceMetrics(t *testing.T) {
	m := metricsOf(newTrace(exposureEvents()))
	want := RunMetrics{Turns: 1, LLMCalls: 2, ToolSearches: 1, Promotions: 1,
		// The provider's count for the first call, the estimate for the second.
		PromptTokens: 5200 + 5600, ToolTokensMax: 2100}
	if m.ToolCalls["tool_search"] != 1 || m.ToolCalls["restore_file"] != 1 {
		t.Errorf("tool calls = %v", m.ToolCalls)
	}
	m.ToolCalls = nil
	if !reflect.DeepEqual(m, want) {
		t.Errorf("metrics = %+v, want %+v", m, want)
	}
}

func TestGrade_ExposureAssertions(t *testing.T) {
	n := func(v int) *int { return &v }
	grade := func(tr Trajectory) GradeResult {
		c := &Case{ID: "exposure", Expect: Expect{Trajectory: tr}}
		return Grade(c, &RunResult{Events: exposureEvents()}, nil)
	}
	pass := []Trajectory{
		{ToolCalls: map[string]Bounds{"tool_search": {Max: n(1)}, "restore_file": {Min: n(1)}}},
		{Promotions: &Bounds{Min: n(1), Max: n(2)}},
		{LLMRequest: &LLMRequestExpect{ToolAdvertisedAllOf: []string{"restore_file"}}},
		{LLMRequest: &LLMRequestExpect{ToolIndexedNoneOf: []string{"shell"}}},
		{LLMRequest: &LLMRequestExpect{ToolTokensMax: n(2100)}},
	}
	for i, tr := range pass {
		if g := grade(tr); !g.Pass {
			t.Errorf("pass[%d]: %v", i, g.Failures)
		}
	}
	fail := map[string]Trajectory{
		"tool_calls":               {ToolCalls: map[string]Bounds{"tool_search": {Max: n(0)}}},
		"promotions":               {Promotions: &Bounds{Max: n(0)}},
		"tool_advertised_all_of":   {LLMRequest: &LLMRequestExpect{ToolAdvertisedAllOf: []string{"trash_list"}}},
		"tool_indexed_none_of":     {LLMRequest: &LLMRequestExpect{ToolIndexedNoneOf: []string{"trash_list"}}},
		"tool_tokens_max":          {LLMRequest: &LLMRequestExpect{ToolTokensMax: n(2000)}},
		"none_of, in the index":    {LLMRequest: &LLMRequestExpect{ToolAdvertisedNoneOf: []string{"trash_list"}}},
		"none_of, in a search hit": {LLMRequest: &LLMRequestExpect{ToolAdvertisedNoneOf: []string{"restore_file"}}},
	}
	for name, tr := range fail {
		g := grade(tr)
		if g.Pass {
			t.Errorf("%s: expected a failure", name)
		}
	}
}

func TestSummarizeAndToolUse(t *testing.T) {
	run := func(pass bool, prompt, turns int, calls map[string]int) RunOutcome {
		return RunOutcome{Pass: pass, Metrics: RunMetrics{PromptTokens: prompt, Turns: turns, LLMCalls: turns, ToolTokensMax: 2000, ToolCalls: calls}}
	}
	results := []CaseModelResult{
		{CaseID: "a", Model: "m", Arm: ArmRanked, Runs: []RunOutcome{
			run(true, 100, 1, map[string]int{"read_file": 2}),
			run(true, 300, 3, map[string]int{"read_file": 1, "tool_search": 1}),
			run(false, 9999, 9, map[string]int{"read_file": 1}),
			{Err: "timeout"},
		}},
		{CaseID: "b", Model: "m", Arm: ArmRanked, Runs: []RunOutcome{run(true, 200, 2, nil)}},
	}
	sum := summarize(results)
	if len(sum) != 1 {
		t.Fatalf("summary = %+v", sum)
	}
	s := sum[0]
	// Medians over the three passed runs only; the aborted run is not graded.
	if s.Passed != 3 || s.Runs != 4 || s.MedianPromptTokens != 200 || s.MedianTurns != 2 || s.ToolTokensMax != 2000 {
		t.Errorf("summary = %+v", s)
	}
	use, runs := toolUse(results)
	if runs != 4 || len(use) != 2 || use[0] != (ToolUse{Name: "read_file", Runs: 3, Calls: 4}) ||
		use[1] != (ToolUse{Name: "tool_search", Runs: 1, Calls: 1}) {
		t.Errorf("tool use = %+v over %d runs", use, runs)
	}
}

func TestRenderGrid_Arms(t *testing.T) {
	r := &Report{
		Models: []string{"qwen3.5:4b"},
		Arms:   []string{ArmRanked},
		Live: []CaseModelResult{
			{CaseID: "kv-case", Model: "qwen3.5:4b", Arm: ArmRanked, Runs: make([]RunOutcome, 3), Passes: 2, ThresholdOK: true},
		},
	}
	grid := r.RenderGrid()
	if !strings.Contains(grid, "qwen3.5:4b ranked") || !strings.Contains(grid, "2/3") {
		t.Errorf("grid missing the arm column:\n%s", grid)
	}
}
