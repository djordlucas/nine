package runner

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"

	"nine/internal/memory"
	"nine/internal/workflow"
)

// readWorkspaceFile reads a workspace file, reporting absence via ok=false so a
// missing side-effect file is a graded failure rather than a hard error.
func readWorkspaceFile(abs string) (content string, ok bool) {
	b, err := os.ReadFile(abs)
	if err != nil {
		return "", false
	}
	return string(b), true
}

// Grade evaluates every assertion a case declares against one execution and
// returns the verdict. A case passes when all present assertions hold
// (docs/evals.md §2). Judge assertions are handled separately (they need a
// provider); Grade records a pending note when a judge is declared and judgeFn is
// nil, so a live run that forgot to wire the judge fails loudly rather than
// silently passing.
//
// judgeFn, when non-nil, scores the final answer against the case's rubric and
// returns (pass, detail).
func Grade(c *Case, res *RunResult, judgeFn JudgeFunc) GradeResult {
	g := &grader{c: c, res: res, tr: newTrace(res.Events)}
	g.gradeSideEffects()
	g.gradeTrajectory()
	g.gradeAnswer(judgeFn)
	return GradeResult{Pass: len(g.failures) == 0, Failures: g.failures}
}

// GradeResult is the verdict for one execution: pass plus the human-readable
// reasons any assertion failed (empty on pass).
type GradeResult struct {
	Pass     bool
	Failures []string
}

// JudgeFunc scores an answer against a rubric, returning whether it passes and a
// short explanation. Implemented in judge.go.
type JudgeFunc func(c *Case, answer string) (pass bool, detail string, err error)

type grader struct {
	c        *Case
	res      *RunResult
	tr       *trace
	failures []string
}

func (g *grader) fail(format string, args ...any) {
	g.failures = append(g.failures, fmt.Sprintf(format, args...))
}

// finalAnswer is the last turn's answer (the case's overall result).
func (g *grader) finalAnswer() string {
	if len(g.res.Answers) == 0 {
		return ""
	}
	return g.res.Answers[len(g.res.Answers)-1]
}

// ── side-effects ────────────────────────────────────────────────────────────

func (g *grader) gradeSideEffects() {
	se := g.c.Expect.SideEffects
	store := g.res.Store

	for path, m := range se.Files {
		abs, err := workspacePath(g.res.Workspace, path)
		if err != nil {
			g.fail("side_effect file %s: %v", path, err)
			continue
		}
		content, ok := readWorkspaceFile(abs)
		if m.Absent {
			if ok {
				g.fail("side_effect file %s: still exists", path)
			}
			continue
		}
		if !ok {
			g.fail("side_effect file %s: not found", path)
			continue
		}
		if why, ok := m.match(content); !ok {
			g.fail("side_effect file %s: %s", path, why)
		}
	}

	// stored_files is keyed by a path prefix and passes when SOME file under it
	// matches, because a spill path carries an unpredictable random suffix.
	for prefix, m := range se.StoredFiles {
		paths, err := store.FileList(prefix)
		if err != nil {
			g.fail("side_effect stored_file %s: %v", prefix, err)
			continue
		}
		if len(paths) == 0 {
			g.fail("side_effect stored_file %s: no stored file under that prefix", prefix)
			continue
		}
		matched := false
		var why string
		for _, p := range paths {
			content, found, err := store.FileFetch(p)
			if err != nil || !found {
				continue
			}
			if reason, ok := m.match(content); ok {
				matched = true
				break
			} else {
				why = reason
			}
		}
		if !matched {
			g.fail("side_effect stored_file %s: none of %d file(s) matched: %s",
				prefix, len(paths), why)
		}
	}

	for key, m := range se.KV {
		val, ok, err := store.Get(key)
		if err != nil {
			g.fail("side_effect kv %s: %v", key, err)
			continue
		}
		if !ok {
			g.fail("side_effect kv %s: key absent", key)
			continue
		}
		if why, ok := m.match(val); !ok {
			g.fail("side_effect kv %s: %s", key, why)
		}
	}

	if w := se.Workflows; w != nil {
		wfs, err := store.WorkflowList(g.res.AgentID)
		if err != nil {
			g.fail("side_effect workflows: %v", err)
		} else if !matchWorkflow(wfs, w) {
			g.fail("side_effect workflows: no workflow with status=%q min_steps=%d (found %d)",
				w.Status, w.MinSteps, len(wfs))
		}
	}

	if gexp := se.Goals; gexp != nil {
		goals, err := store.GoalList()
		if err != nil {
			g.fail("side_effect goals: %v", err)
		} else {
			created := len(goals) - g.res.PreGoals
			if gexp.Created > 0 && created < gexp.Created {
				g.fail("side_effect goals.created: created %d, want >= %d", created, gexp.Created)
			}
			if gexp.PursueSpawned && !pursueSpawned(store) {
				g.fail("side_effect goals.pursue_spawned: no active pursue session")
			}
		}
	}

	if n := se.Notifications; n != nil {
		notes, err := store.UserNotificationList(false)
		if err != nil {
			g.fail("side_effect notifications: %v", err)
		} else if len(notes) < n.Min {
			g.fail("side_effect notifications: %d, want >= %d", len(notes), n.Min)
		}
	}

	if v := se.Vectors; v != nil {
		count, err := store.VectorCount(v.Namespace)
		if err != nil {
			g.fail("side_effect vectors: %v", err)
		} else if count < v.Min {
			g.fail("side_effect vectors[%s]: %d, want >= %d", v.Namespace, count, v.Min)
		}
	}
}

// matchWorkflow reports whether any workflow satisfies both the status (if set)
// and the minimum step count.
func matchWorkflow(wfs []workflow.Workflow, w *WorkflowExpect) bool {
	for _, wf := range wfs {
		if w.Status != "" && wf.Status != w.Status {
			continue
		}
		if len(wf.Steps) < w.MinSteps {
			continue
		}
		return true
	}
	return false
}

// pursueSpawned reports whether an active session plan carries a pursue routine,
// the durable marker that goal_create spawned a background pursue session
// (docs/goal-sessions.md).
func pursueSpawned(store *memory.Store) bool {
	plans, err := store.SessionPlanListActive()
	if err != nil {
		return false
	}
	for _, p := range plans {
		for _, st := range p.Routines {
			if st.Name == "pursue" || st.Kind == "pursue" {
				return true
			}
		}
	}
	return false
}

// ── trajectory ──────────────────────────────────────────────────────────────

func (g *grader) gradeTrajectory() {
	t := g.c.Expect.Trajectory
	tr := g.tr

	for _, name := range t.ToolsAllOf {
		if !tr.calledTool(name) {
			g.fail("trajectory tools_all_of: %q never called", name)
		}
	}
	if len(t.ToolsAnyOf) > 0 {
		any := false
		for _, name := range t.ToolsAnyOf {
			if tr.calledTool(name) {
				any = true
				break
			}
		}
		if !any {
			g.fail("trajectory tools_any_of: none of %v called", t.ToolsAnyOf)
		}
	}
	for _, name := range t.ToolsNoneOf {
		if tr.calledTool(name) {
			g.fail("trajectory tools_none_of: %q was called", name)
		}
	}

	if t.MaxTurns != nil && tr.userTurns > *t.MaxTurns {
		g.fail("trajectory max_turns: %d turns, want <= %d", tr.userTurns, *t.MaxTurns)
	}
	if t.MinTurns != nil && tr.userTurns < *t.MinTurns {
		g.fail("trajectory min_turns: %d turns, want >= %d", tr.userTurns, *t.MinTurns)
	}
	if t.NoStall && tr.stalled {
		g.fail("trajectory no_stall: a turn ended with a stall")
	}
	if t.Spills != nil && tr.spills < t.Spills.Min {
		g.fail("trajectory spills: %d tool result(s) spilled, want >= %d",
			tr.spills, t.Spills.Min)
	}
	if t.GapReport != nil {
		if *t.GapReport && !tr.gapReported {
			g.fail("trajectory gap_report: expected a gap_report, none fired")
		}
		if !*t.GapReport && tr.gapReported {
			g.fail("trajectory gap_report: unexpected gap_report fired")
		}
	}
	if sa := t.SubAgents; sa != nil {
		n := tr.subAgents
		if sa.Count != nil && n != *sa.Count {
			g.fail("trajectory sub_agents.count: %d, want %d", n, *sa.Count)
		}
		if sa.Min != nil && n < *sa.Min {
			g.fail("trajectory sub_agents.min: %d, want >= %d", n, *sa.Min)
		}
		if sa.Max != nil && n > *sa.Max {
			g.fail("trajectory sub_agents.max: %d, want <= %d", n, *sa.Max)
		}
	}
	if lr := t.LLMRequest; lr != nil {
		for _, sub := range lr.SystemContains {
			if !tr.systemContains(sub) {
				g.fail("trajectory llm_request.system_contains: %q absent from every system prompt", sub)
			}
		}
		for _, sub := range lr.SystemNotContains {
			if tr.systemContains(sub) {
				g.fail("trajectory llm_request.system_not_contains: %q present in a system prompt", sub)
			}
		}
		for _, name := range lr.ToolAdvertisedNoneOf {
			if tr.toolAdvertised(name) {
				g.fail("trajectory llm_request.tool_advertised_none_of: %q was advertised", name)
			}
		}
	}
}

// ── answer ──────────────────────────────────────────────────────────────────

func (g *grader) gradeAnswer(judgeFn JudgeFunc) {
	a := g.c.Expect.Answer
	ans := g.finalAnswer()
	lower := strings.ToLower(ans)

	for _, sub := range a.Contains {
		if !strings.Contains(lower, strings.ToLower(sub)) {
			g.fail("answer.contains: %q absent", sub)
		}
	}
	for _, sub := range a.NotContains {
		if strings.Contains(lower, strings.ToLower(sub)) {
			g.fail("answer.not_contains: %q present", sub)
		}
	}
	if a.Matches != "" {
		re, err := regexp.Compile(a.Matches)
		if err != nil {
			g.fail("answer.matches: bad regexp %q: %v", a.Matches, err)
		} else if !re.MatchString(ans) {
			g.fail("answer.matches: %q did not match", a.Matches)
		}
	}
	if a.Judge != nil {
		if judgeFn == nil {
			g.fail("answer.judge: declared but no judge configured for this run")
			return
		}
		pass, detail, err := judgeFn(g.c, ans)
		if err != nil {
			g.fail("answer.judge: %v", err)
		} else if !pass {
			g.fail("answer.judge: %s", detail)
		}
	}
}

// ── string matching ─────────────────────────────────────────────────────────

// match applies the StringMatch predicate to s, returning ("", true) on a match
// or a reason on failure. Assumes validate() already enforced exactly one field.
func (m StringMatch) match(s string) (why string, ok bool) {
	switch {
	case m.Equals != nil:
		if s == *m.Equals {
			return "", true
		}
		return fmt.Sprintf("equals %q, got %q", *m.Equals, s), false
	case m.Contains != "":
		if strings.Contains(s, m.Contains) {
			return "", true
		}
		return fmt.Sprintf("does not contain %q", m.Contains), false
	case m.Matches != "":
		re, err := regexp.Compile(m.Matches)
		if err != nil {
			return fmt.Sprintf("bad regexp %q: %v", m.Matches, err), false
		}
		if re.MatchString(s) {
			return "", true
		}
		return fmt.Sprintf("does not match %q", m.Matches), false
	}
	return "no predicate", false
}

// ── journal trace ───────────────────────────────────────────────────────────

// trace is a digest of a run's journal in the shapes the trajectory assertions
// need, computed once from the flat event list (docs/evals.md §3 mappings).
type trace struct {
	tools       map[string]bool // tool_start names seen
	advertised  map[string]bool // union of llm_request tool_names
	systems     []string        // llm_request system prompts
	userTurns   int             // count of user-triggered turn_end events
	stalled     bool            // any turn_end.error == "stall"
	gapReported bool            // gap_report tool_start (or supervisor gap)
	subAgents   int             // spawned sub-agents (see subAgents accounting below)
	spills      int             // tool_end events whose output was spilled to the file store
}

func newTrace(events []memory.SessionEvent) *trace {
	t := &trace{tools: map[string]bool{}, advertised: map[string]bool{}}
	turnTrigger := map[int]string{}
	// Sub-agents are counted two ways and reconciled at the end: sub_agent_start
	// progress events (accurate per-child, if journaled) and the tool calls that
	// spawn them (run_agent = 1 child; run_agents = len(tasks)). We take the max
	// so a journal missing either source still yields the right count.
	starts := 0
	fromCalls := 0
	for _, e := range events {
		switch e.Type {
		case "turn_start":
			var p struct {
				Trigger string `json:"trigger"`
			}
			_ = json.Unmarshal(e.Payload, &p)
			turnTrigger[e.Turn] = p.Trigger
		case "turn_end":
			// Count only user-triggered turns (idle/background turns don't count
			// against a case's turn budget). Absent trigger defaults to user.
			if trig := turnTrigger[e.Turn]; trig == "" || trig == "user" {
				t.userTurns++
			}
			var p struct {
				Error string `json:"error"`
			}
			_ = json.Unmarshal(e.Payload, &p)
			if p.Error == "stall" {
				t.stalled = true
			}
		case "tool_start":
			var p struct {
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			}
			_ = json.Unmarshal(e.Payload, &p)
			if p.Name == "" {
				continue
			}
			t.tools[p.Name] = true
			switch p.Name {
			case "gap_report":
				t.gapReported = true
			case "run_agent":
				fromCalls++
			case "run_agents":
				fromCalls += countTasks(p.Input)
			}
		case "tool_end":
			var p struct {
				SpillPath string `json:"spill_path"`
			}
			_ = json.Unmarshal(e.Payload, &p)
			if p.SpillPath != "" {
				t.spills++
			}
		case "llm_request":
			var p struct {
				System    string   `json:"system"`
				ToolNames []string `json:"tool_names"`
			}
			_ = json.Unmarshal(e.Payload, &p)
			t.systems = append(t.systems, p.System)
			for _, n := range p.ToolNames {
				t.advertised[n] = true
			}
		case "sub_agent_start":
			starts++
		}
	}
	t.subAgents = starts
	if fromCalls > t.subAgents {
		t.subAgents = fromCalls
	}
	return t
}

// countTasks returns the number of tasks in a run_agents tool input, or 1 when
// the shape is unexpected (a call still spawned at least one child).
func countTasks(input json.RawMessage) int {
	var p struct {
		Tasks []json.RawMessage `json:"tasks"`
	}
	if err := json.Unmarshal(input, &p); err != nil || len(p.Tasks) == 0 {
		return 1
	}
	return len(p.Tasks)
}

func (t *trace) calledTool(name string) bool     { return t.tools[name] }
func (t *trace) toolAdvertised(name string) bool { return t.advertised[name] }

func (t *trace) systemContains(sub string) bool {
	for _, s := range t.systems {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// Note: run_agents fans out to multiple sub_agent_start events, so subAgents
// counts children accurately even when a single tool call spawns several. The
// tool-call increment above is a fallback for journals that predate sub-agent
// progress events; when both are present the sub_agent_start events dominate.
