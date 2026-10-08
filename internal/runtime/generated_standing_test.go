package runtime

import (
	"strings"
	"testing"

	"nine/internal/agent"
	"nine/internal/memory"
	"nine/internal/memory/memtest"
)

// checkStandingRequest is where a promotion is refused, before anything is
// persisted, so the model gets a message it can act on rather than a tool that
// exists and never runs.
func TestStandingRequestRefusals(t *testing.T) {
	base := agent.GeneratedToolSpec{Name: "grinder", Resumable: true}
	withStanding := func(s agent.StandingRequest) agent.GeneratedToolSpec {
		spec := base
		spec.Standing = &s
		return spec
	}

	cases := []struct {
		name          string
		allow         bool
		spec          agent.GeneratedToolSpec
		wantSubstring string
	}{
		{
			name: "off by default", allow: false,
			spec:          withStanding(agent.StandingRequest{Interval: "10s"}),
			wantSubstring: "allow_standing",
		},
		{
			name: "must also be resumable", allow: true,
			spec: func() agent.GeneratedToolSpec {
				s := withStanding(agent.StandingRequest{Interval: "10s"})
				s.Resumable = false
				return s
			}(),
			wantSubstring: "resumable",
		},
		{
			name: "needs a cadence", allow: true,
			spec:          withStanding(agent.StandingRequest{}),
			wantSubstring: "interval or schedule",
		},
		{
			name: "not both", allow: true,
			spec:          withStanding(agent.StandingRequest{Interval: "10s", Schedule: "* * * * *"}),
			wantSubstring: "not both",
		},
		{
			name: "interval must parse", allow: true,
			spec:          withStanding(agent.StandingRequest{Interval: "soon"}),
			wantSubstring: "duration",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, err := memtest.Open(t)
			if err != nil {
				t.Fatal(err)
			}
			g := &generatedTools{store: store, allowStanding: tc.allow, maxRunning: 4}
			err = g.checkStandingRequest(tc.spec)
			if err == nil {
				t.Fatal("the request was accepted")
			}
			if !strings.Contains(err.Error(), tc.wantSubstring) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.wantSubstring)
			}
		})
	}
}

// The cap is max_running, the one cap on processes running at once: an
// operator's declared processes count against it as Nine's own do, and a
// stopped process does not.
func TestGeneratedStandingCapCountsEveryRunningProcess(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	g := &generatedTools{store: store, allowStanding: true, maxRunning: 2}
	spec := agent.GeneratedToolSpec{
		Name: "g1", Resumable: true,
		Standing: &agent.StandingRequest{Interval: "10s"},
	}

	if err := store.ProcessUpsertDefinition(memory.Process{ID: "op-a", Tool: "x", IntervalSecs: 60}); err != nil {
		t.Fatal(err)
	}
	if err := store.ProcessUpsertDefinition(memory.Process{ID: "gen:stopped", Tool: "x", IntervalSecs: 60, Generated: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ProcessStop("gen:stopped", "model"); err != nil {
		t.Fatal(err)
	}
	if err := g.checkStandingRequest(spec); err != nil {
		t.Fatalf("one running process of two allowed refused the request: %v", err)
	}

	if err := store.ProcessUpsertDefinition(memory.Process{ID: "op-b", Tool: "x", IntervalSecs: 60}); err != nil {
		t.Fatal(err)
	}
	err = g.checkStandingRequest(spec)
	if err == nil || !strings.Contains(err.Error(), "max_running") {
		t.Fatalf("err = %v, want max_running to refuse", err)
	}
}

// Rewriting a tool replaces its standing run rather than accumulating one per
// write — the id is derived from the tool name for exactly this reason.
func TestRewritingAStandingToolReplacesItsRun(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	g := &generatedTools{store: store, allowStanding: true, maxRunning: 1}
	spec := agent.GeneratedToolSpec{
		Name: "g1", Resumable: true,
		Standing: &agent.StandingRequest{Interval: "10s"},
	}
	if err := g.promoteToStanding(spec); err != nil {
		t.Fatal(err)
	}
	// The cap is 1, and rewriting the same tool must not trip it.
	if err := g.checkStandingRequest(spec); err != nil {
		t.Fatalf("rewriting the same standing tool was refused: %v", err)
	}
	if err := g.promoteToStanding(spec); err != nil {
		t.Fatal(err)
	}

	runs, err := store.ProcessList()
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("%d standing runs after two writes, want 1", len(runs))
	}
}

// Deleting a generated tool must delete its standing run. An orphaned run is a
// row the driver picks up every tick and fails on forever with "unknown
// sandboxed tool" — eventually tripping the breaker and notifying a human about
// a tool that no longer exists.
func TestDeletingAToolRemovesItsStandingRun(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	g := &generatedTools{store: store, allowStanding: true, maxRunning: 4}
	if err := store.GeneratedToolUpsert(memory.GeneratedTool{Name: "g1", Source: "export default () => 1;"}); err != nil {
		t.Fatal(err)
	}
	spec := agent.GeneratedToolSpec{
		Name: "g1", Resumable: true,
		Standing: &agent.StandingRequest{Interval: "10s"},
	}
	if err := g.promoteToStanding(spec); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := store.ProcessGet(standingIDFor("g1")); !found {
		t.Fatal("precondition: the standing run should exist")
	}

	if err := g.Delete(t.Context(), "g1"); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := store.ProcessGet(standingIDFor("g1")); found {
		t.Fatal("the standing run outlived the tool it runs")
	}
}
