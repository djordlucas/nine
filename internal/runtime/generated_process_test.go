package runtime

import (
	"strings"
	"testing"

	"nine/internal/agent"
	"nine/internal/memory"
	"nine/internal/memory/memtest"
)

const liveSource = `import { next, turn } from "nine:process";
export default () => { for (;;) { next(); turn("summarize"); } };`

func processSpec(name string, p agent.ProcessRequest) agent.GeneratedToolSpec {
	return agent.GeneratedToolSpec{Name: name, Source: liveSource, Process: &p}
}

func processStore(t *testing.T, policy GeneratedPolicy) (*generatedTools, *memory.Store) {
	t.Helper()
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	if policy.MaxRunning == 0 {
		policy.MaxRunning = 4
	}
	if len(policy.ProcessRoles) == 0 {
		policy.ProcessRoles = []string{ProcessRole}
	}
	policy.Budget.TurnsPerDay, policy.Budget.TokensPerDay = 200, 2_000_000
	return &generatedTools{store: store, policy: policy}, store
}

// Every refusal of a process request names what to change, before anything is
// stored.
func TestProcessRequestRefusals(t *testing.T) {
	on := GeneratedPolicy{AllowProcesses: true}
	cases := []struct {
		name   string
		policy GeneratedPolicy
		spec   agent.GeneratedToolSpec
		want   string
	}{
		{"off", GeneratedPolicy{}, processSpec("p", agent.ProcessRequest{Every: "1h"}), "allow_processes"},
		{"both clocks", on, processSpec("p", agent.ProcessRequest{Every: "1h", Schedule: "0 7 * * *"}), "not both"},
		{"bad every", on, processSpec("p", agent.ProcessRequest{Every: "soon"}), "duration"},
		{"bad schedule", on, processSpec("p", agent.ProcessRequest{Schedule: "daily"}), "schedule"},
		{"not a live program", on, agent.GeneratedToolSpec{Name: "p", Source: "export default () => 1;",
			Process: &agent.ProcessRequest{Every: "1h"}}, "nine:process"},
		{"slice without a clock", on, agent.GeneratedToolSpec{Name: "p", Source: "x", Resumable: true,
			Process: &agent.ProcessRequest{}}, "every or schedule"},
		{"role not allowed", on, processSpec("p", agent.ProcessRequest{Every: "1h", Role: "orchestrator"}), "process_roles"},
		{"budget above", on, processSpec("p", agent.ProcessRequest{Every: "1h",
			Budget: &agent.ProcessBudget{TurnsPerDay: 500}}), "never raise"},
		{"pipe to nowhere", on, processSpec("p", agent.ProcessRequest{Every: "1h", ReportTo: "nobody"}), "report_to"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g, _ := processStore(t, tc.policy)
			err := g.checkProcessRequest(tc.spec)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// A role the operator lists is accepted, and a process with no clock is: it
// wakes on messages.
func TestProcessRequestAccepted(t *testing.T) {
	g, _ := processStore(t, GeneratedPolicy{AllowProcesses: true, ProcessRoles: []string{"process", "writer"}})
	for _, p := range []agent.ProcessRequest{{Every: "1h"}, {Role: "writer", Schedule: "0 7 * * *"}, {}} {
		if err := g.checkProcessRequest(processSpec("p", p)); err != nil {
			t.Errorf("%+v refused: %v", p, err)
		}
	}
}

// A written process pipes only into another live process Nine wrote: never
// into a goal session or a declared process.
func TestProcessPipesOnlyToNinesOwn(t *testing.T) {
	g, store := processStore(t, GeneratedPolicy{AllowProcesses: true})
	if err := store.ProcessUpsertDefinition(memory.Process{ID: "sec-watch", Tool: "pursue", Mode: memory.ProcessLive}); err != nil {
		t.Fatal(err)
	}
	if err := g.checkProcessRequest(processSpec("p", agent.ProcessRequest{ReportTo: "sec-watch"})); err == nil {
		t.Error("a pipe into a declared process was accepted")
	}
	if err := g.recordProcess(processSpec("sink", agent.ProcessRequest{})); err != nil {
		t.Fatal(err)
	}
	if err := g.checkProcessRequest(processSpec("p", agent.ProcessRequest{ReportTo: "sink"})); err != nil {
		t.Errorf("a pipe into Nine's own process was refused: %v", err)
	}
}

// The cap is max_running, the one cap on processes running at once: a
// declared process counts against it as Nine's own do, a stopped one does
// not, and a rewrite of a running process does not count twice.
func TestWrittenProcessCountsAgainstMaxRunning(t *testing.T) {
	g, store := processStore(t, GeneratedPolicy{AllowProcesses: true, MaxRunning: 2})
	spec := processSpec("p", agent.ProcessRequest{Every: "1h"})
	if err := store.ProcessUpsertDefinition(memory.Process{ID: "op-a", Tool: "x", IntervalSecs: 60}); err != nil {
		t.Fatal(err)
	}
	if err := g.checkProcessRequest(spec); err != nil {
		t.Fatalf("one running of two allowed refused the request: %v", err)
	}
	if err := g.recordProcess(spec); err != nil {
		t.Fatal(err)
	}
	if err := g.checkProcessRequest(spec); err != nil {
		t.Errorf("rewriting the running process counted it twice: %v", err)
	}
	if err := g.checkProcessRequest(processSpec("q", agent.ProcessRequest{Every: "1h"})); err == nil ||
		!strings.Contains(err.Error(), "max_running") {
		t.Errorf("err = %v, want max_running to refuse", err)
	}
}

// A written process is recorded live, in its own session, under its role and
// budget; a rewrite keeps its run state, so one the operator stopped stays
// stopped.
func TestRecordProcess(t *testing.T) {
	g, store := processStore(t, GeneratedPolicy{AllowProcesses: true})
	spec := processSpec("digest", agent.ProcessRequest{Every: "30m", Budget: &agent.ProcessBudget{TurnsPerDay: 5}})
	if err := g.recordProcess(spec); err != nil {
		t.Fatal(err)
	}
	p, ok, _ := store.ProcessGet("gen:digest")
	if !ok || p.Mode != memory.ProcessLive || p.SessionID != "gen:digest" || p.Role != ProcessRole ||
		p.IntervalSecs != 1800 || p.BudgetTurns != 5 || !p.Generated || p.State != memory.ProcessRunning {
		t.Fatalf("process = %+v", p)
	}
	if _, err := store.ProcessStop("gen:digest", ByOperator); err != nil {
		t.Fatal(err)
	}
	if err := g.recordProcess(spec); err != nil {
		t.Fatal(err)
	}
	if p, _, _ := store.ProcessGet("gen:digest"); p.State != memory.ProcessStopped {
		t.Errorf("a rewrite restarted a process the operator stopped: %q", p.State)
	}
	all, _ := store.ProcessList()
	if len(all) != 1 {
		t.Errorf("%d processes after two writes, want 1", len(all))
	}
}

// Deleting a tool deletes its process. A model may not delete one the operator
// stopped, since that would undo the stop; the operator may.
func TestDeletingAToolRemovesItsProcess(t *testing.T) {
	g, store := processStore(t, GeneratedPolicy{AllowProcesses: true})
	if err := store.GeneratedToolUpsert(memory.GeneratedTool{Name: "g1", Source: liveSource, Live: true}); err != nil {
		t.Fatal(err)
	}
	if err := g.recordProcess(processSpec("g1", agent.ProcessRequest{Every: "1h"})); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ProcessStop("gen:g1", ByOperator); err != nil {
		t.Fatal(err)
	}
	if err := g.Delete(t.Context(), "g1"); err == nil || !strings.Contains(err.Error(), "only the operator") {
		t.Fatalf("a model's delete of an operator-stopped process = %v", err)
	}
	if err := g.DeleteByOperator(t.Context(), "g1"); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := store.ProcessGet("gen:g1"); found {
		t.Fatal("the process outlived the tool it runs")
	}
}

// Source that does not parse is refused at write time, with where; source
// that parses — imports of nine:* modules included — passes.
func TestCheckSyntax(t *testing.T) {
	if err := checkSyntax(liveSource); err != nil {
		t.Errorf("a valid live program was refused: %v", err)
	}
	err := checkSyntax("import { next } from \"nine:process\";\nexport default () => { next(;; };")
	if err == nil || !strings.Contains(err.Error(), "line 2") || !strings.Contains(err.Error(), "Nothing was written") {
		t.Errorf("err = %v, want a refusal naming line 2", err)
	}
}
