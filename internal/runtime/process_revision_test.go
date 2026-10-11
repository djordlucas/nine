package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"

	"nine/internal/agent"
	"nine/internal/memory"
	"nine/internal/memory/memtest"
)

const brokenSource = `import { next } from "nine:process";
export default () => { throw new Error("broken v2"); };`

// revisionSetup is a generated-tool store with a host, linked to a runner
// whose live processes run against fake sessions.
func revisionSetup(t *testing.T, policy GeneratedPolicy) (*generatedTools, *StandingRunner, *memory.Store) {
	t.Helper()
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	host := OpenSandboxedTools(context.Background(), enabledAgentCfg(), store, nil)
	t.Cleanup(func() { _ = host.Close(context.Background()) })
	policy.AllowProcesses = true
	g := NewGeneratedToolStoreWithPolicy(store, host, nil, nil, false, policy).(*generatedTools)
	r := NewStandingRunner(store, host, 1, 2)
	r.SetSessions(&fakeSessions{})
	t.Cleanup(r.StopLive)
	LinkStandingTools(g, r)
	return g, r, store
}

func running(r *StandingRunner, id string) bool {
	r.liveMu.Lock()
	defer r.liveMu.Unlock()
	return r.lives[id] != nil
}

// A rewrite of a process's program keeps the previous version, puts the new
// one on probation, and restarts the running instance on it. When the new
// version fails rollback_after times before it has run, the previous version
// is restored — a new version with the old program — the failures are
// cleared, and the rollback is journaled and reported.
func TestFailingRevisionRollsBack(t *testing.T) {
	g, r, store := revisionSetup(t, GeneratedPolicy{})
	ctx := context.Background()
	if _, err := g.Write(ctx, processSpec("digest", agent.ProcessRequest{Every: "1h"})); err != nil {
		t.Fatal(err)
	}
	r.tickLive(ctx)
	eventually(t, "v1 running", func() bool { return running(r, "gen:digest") })
	// v1 has run: it is the version a rollback restores.
	st, _, _ := store.ProcessGet("gen:digest")
	r.proveVersion(st, 1)

	spec := processSpec("digest", agent.ProcessRequest{Every: "1h"})
	spec.Source, spec.WrittenBy = brokenSource, "conv-1"
	res, err := g.Write(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Revised || res.Version != 2 {
		t.Fatalf("rewrite = %+v, want version 2, revised", res)
	}
	if running(r, "gen:digest") {
		t.Error("the old instance still runs after the rewrite")
	}
	cur, _, _ := store.GeneratedToolGet("digest")
	if !cur.Probation {
		t.Fatal("the new version is not on probation")
	}

	st, _, _ = store.ProcessGet("gen:digest")
	st.Failures = r.rollbackAfterOrDefault() - 1
	r.recordFailure(st, errors.New("broken v2"))

	cur, _, _ = store.GeneratedToolGet("digest")
	if cur.Version != 3 || cur.Source != liveSource || cur.Probation || cur.WrittenBy != ByRollback {
		t.Errorf("after rollback: version %d, probation %v, by %q, source v1: %v",
			cur.Version, cur.Probation, cur.WrittenBy, cur.Source == liveSource)
	}
	if st, _, _ = store.ProcessGet("gen:digest"); st.Failures != 0 || st.State != memory.ProcessRunning {
		t.Errorf("process after rollback: failures %d, state %s", st.Failures, st.State)
	}
	evs, _ := store.SessionEventsByAgent("gen:digest")
	var rolled string
	for _, ev := range evs {
		if ev.Type == evProcessRolledBack {
			rolled = string(ev.Payload)
		}
	}
	if !strings.Contains(rolled, `"from":2`) || !strings.Contains(rolled, `"to":1`) || !strings.Contains(rolled, "broken v2") {
		t.Errorf("journal = %q", rolled)
	}
	if feed := feedText(t, store); !strings.Contains(feed, "rolled back to version 1") {
		t.Errorf("feed = %q", feed)
	}
}

// A version that has run is no longer on probation, and failing then is an
// ordinary failure, not a rollback.
func TestProvenVersionDoesNotRollBack(t *testing.T) {
	g, r, store := revisionSetup(t, GeneratedPolicy{})
	ctx := context.Background()
	if _, err := g.Write(ctx, processSpec("digest", agent.ProcessRequest{Every: "1h"})); err != nil {
		t.Fatal(err)
	}
	spec := processSpec("digest", agent.ProcessRequest{Every: "2h"})
	spec.Source = liveSource + "\n// v2"
	if _, err := g.Write(ctx, spec); err != nil {
		t.Fatal(err)
	}
	st, _, _ := store.ProcessGet("gen:digest")
	r.proveVersion(st, 2)
	st.Failures = 5
	r.recordFailure(st, errors.New("later trouble"))
	if cur, _, _ := store.GeneratedToolGet("digest"); cur.Version != 2 {
		t.Errorf("a proven version was rolled back to %d", cur.Version)
	}
}

// The operator may restore any kept version; it becomes current as a new
// version with its process definition.
func TestRestoreAVersion(t *testing.T) {
	g, _, store := revisionSetup(t, GeneratedPolicy{})
	ctx := context.Background()
	if _, err := g.Write(ctx, processSpec("digest", agent.ProcessRequest{Every: "1h"})); err != nil {
		t.Fatal(err)
	}
	spec := processSpec("digest", agent.ProcessRequest{Every: "2h"})
	spec.Source = liveSource + "\n// v2"
	if _, err := g.Write(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if err := g.Restore(ctx, "digest", 1, ByOperator); err != nil {
		t.Fatal(err)
	}
	cur, _, _ := store.GeneratedToolGet("digest")
	p, _, _ := store.ProcessGet("gen:digest")
	if cur.Version != 3 || cur.Source != liveSource || p.IntervalSecs != 3600 {
		t.Errorf("restored: version %d, v1 source %v, interval %d", cur.Version, cur.Source == liveSource, p.IntervalSecs)
	}
	if err := g.Restore(ctx, "digest", 9, ByOperator); err == nil || !strings.Contains(err.Error(), "no version 9") {
		t.Errorf("restoring a missing version: %v", err)
	}
}

// From a process's turn, writing or rewriting a process is revision and needs
// allow_revision; an ordinary tool does not. Nobody may rewrite a process the
// operator stopped.
func TestRevisionGates(t *testing.T) {
	g, _, store := revisionSetup(t, GeneratedPolicy{})
	ctx := context.Background()
	if _, err := g.Write(ctx, processSpec("digest", agent.ProcessRequest{Every: "1h"})); err != nil {
		t.Fatal(err)
	}

	fromProcess := processSpec("digest", agent.ProcessRequest{Every: "2h"})
	fromProcess.Source, fromProcess.FromProcess = liveSource+"\n// v2", true
	if _, err := g.Write(ctx, fromProcess); err == nil || !strings.Contains(err.Error(), "allow_revision") {
		t.Errorf("revision without allow_revision: %v", err)
	}
	plain := agent.GeneratedToolSpec{Name: "helper", Description: "d", Source: "export default () => 1;", FromProcess: true}
	if _, err := g.Write(ctx, plain); err != nil {
		t.Errorf("an ordinary tool from a process's turn: %v", err)
	}

	g.policy.AllowRevision = true
	if _, err := g.Write(ctx, fromProcess); err != nil {
		t.Errorf("revision with allow_revision: %v", err)
	}

	if ok, err := store.ProcessStop("gen:digest", ByOperator); err != nil || !ok {
		t.Fatal(err)
	}
	fromProcess.Source += "\n// v3"
	if _, err := g.Write(ctx, fromProcess); err == nil || !strings.Contains(err.Error(), "operator stopped") {
		t.Errorf("rewriting an operator-stopped process: %v", err)
	}
}

// A rewrite of a failing process starts the new version with a clean count:
// the old version's failures and backoff are not the new one's.
func TestRevisionClearsTheOldFailures(t *testing.T) {
	g, r, store := revisionSetup(t, GeneratedPolicy{})
	ctx := context.Background()
	spec := processSpec("digest", agent.ProcessRequest{Every: "1h"})
	spec.Source = brokenSource
	if _, err := g.Write(ctx, spec); err != nil {
		t.Fatal(err)
	}
	st, _, _ := store.ProcessGet("gen:digest")
	r.recordFailure(st, errors.New("broken"))
	st, _, _ = store.ProcessGet("gen:digest")
	r.recordFailure(st, errors.New("broken"))
	if st, _, _ = store.ProcessGet("gen:digest"); st.Failures != 2 {
		t.Fatalf("failures = %d", st.Failures)
	}
	if _, err := g.Write(ctx, processSpec("digest", agent.ProcessRequest{Every: "1h"})); err != nil {
		t.Fatal(err)
	}
	if st, _, _ = store.ProcessGet("gen:digest"); st.Failures != 0 || st.LastError != "" {
		t.Errorf("after the fix: failures %d, last error %q", st.Failures, st.LastError)
	}
	r.tickLive(ctx)
	eventually(t, "the fixed version running", func() bool { return running(r, "gen:digest") })
}

// A rewrite that leaves out the process block still revises the process the
// tool runs: it stays live, goes on probation, and restarts.
func TestRewriteWithoutTheProcessBlockRevises(t *testing.T) {
	g, r, store := revisionSetup(t, GeneratedPolicy{})
	ctx := context.Background()
	if _, err := g.Write(ctx, processSpec("digest", agent.ProcessRequest{Every: "1h"})); err != nil {
		t.Fatal(err)
	}
	r.tickLive(ctx)
	eventually(t, "v1 running", func() bool { return running(r, "gen:digest") })
	res, err := g.Write(ctx, agent.GeneratedToolSpec{Name: "digest", Description: "d", Source: liveSource + "\n// v2"})
	if err != nil {
		t.Fatal(err)
	}
	cur, _, _ := store.GeneratedToolGet("digest")
	p, _, _ := store.ProcessGet("gen:digest")
	if !res.Revised || res.Process != "gen:digest" || !cur.Live || !cur.Probation || p.IntervalSecs != 3600 {
		t.Errorf("result %+v; tool live %v probation %v; interval %d", res, cur.Live, cur.Probation, p.IntervalSecs)
	}
	if running(r, "gen:digest") {
		t.Error("the old instance still runs")
	}
}

// A rollback restores the newest version that ran, skipping versions replaced
// while still on probation; with none that ran, there is no rollback.
func TestRollbackSkipsVersionsThatNeverRan(t *testing.T) {
	g, r, store := revisionSetup(t, GeneratedPolicy{})
	ctx := context.Background()
	write := func(src string) {
		t.Helper()
		spec := processSpec("digest", agent.ProcessRequest{Every: "1h"})
		spec.Source = src
		if _, err := g.Write(ctx, spec); err != nil {
			t.Fatal(err)
		}
	}
	fail := func() {
		t.Helper()
		st, _, _ := store.ProcessGet("gen:digest")
		st.Failures = r.rollbackAfterOrDefault() - 1
		r.recordFailure(st, errors.New("broken"))
	}

	write(brokenSource)          // v1, never runs
	write(brokenSource + "\n//") // v2, never runs
	fail()
	if cur, _, _ := store.GeneratedToolGet("digest"); cur.Version != 2 {
		t.Fatalf("rolled back to an unproven version: now at %d", cur.Version)
	}

	write(liveSource) // v3, runs
	st, _, _ := store.ProcessGet("gen:digest")
	r.proveVersion(st, 3)
	write(brokenSource + "\n// v4") // v4, fails
	fail()
	cur, _, _ := store.GeneratedToolGet("digest")
	if cur.Version != 5 || cur.Source != liveSource {
		t.Errorf("after rollback: version %d, the v3 source %v", cur.Version, cur.Source == liveSource)
	}
}
