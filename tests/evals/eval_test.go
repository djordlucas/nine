// Package evals_test is the runnable entry point for the Nine eval suite
// (docs/evals.md §8). It has three layers, each gated by how much infrastructure
// it needs:
//
//   - TestCasesValidate — loads and validates every case; no infra. Always runs.
//   - TestReplayFixtures — Track R: replays every recorded fixture and asserts the
//     reproduced answers equal the recorded ones. No live model, no database, no
//     plugins. Runs on every PR.
//   - TestLiveMatrix — Track L: the live model matrix. Gated by NINE_EVALS_LIVE=1
//     and a model list; needs Postgres (and plugins for tool cases).
//
// Layout (docs/evals.md §8):
//
//	tests/evals/cases/     the case corpus (*.yaml)
//	tests/evals/replay/    recorded Track-R fixtures (<id>/journal.json)
//	tests/evals/reports/   JSON + rendered grid per live run
package evals_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nine/tests/evals/runner"
)

const (
	casesDir   = "cases"
	replayRoot = "replay"
	reportsDir = "reports"
)

// TestCasesValidate loads the whole corpus, which validates every case against
// the schema (docs/evals.md §2). A malformed or duplicate-id case fails here.
func TestCasesValidate(t *testing.T) {
	cases, err := runner.LoadCases(casesDir)
	if err != nil {
		t.Fatalf("load cases: %v", err)
	}
	if len(cases) == 0 {
		t.Fatal("no cases found")
	}
	t.Logf("loaded %d cases", len(cases))
}

// TestReplayFixtures runs Track R for every replay/both case that has a recorded
// fixture. It is deterministic and infra-free — the every-PR gate.
func TestReplayFixtures(t *testing.T) {
	cases, err := runner.LoadCases(casesDir)
	if err != nil {
		t.Fatalf("load cases: %v", err)
	}
	ran := 0
	for _, c := range cases {
		if c.Track != runner.TrackReplay && c.Track != runner.TrackBoth {
			continue
		}
		dir := runner.ReplayDir(replayRoot, c.ID)
		if _, err := os.Stat(filepath.Join(dir, "journal.json")); err != nil {
			t.Logf("skip %s: no fixture recorded yet", c.ID)
			continue
		}
		ran++
		t.Run(c.ID, func(t *testing.T) {
			rr, err := runner.ReplayFixture(context.Background(), dir)
			if err != nil {
				t.Fatalf("replay: %v", err)
			}
			if !rr.Pass() {
				t.Fatalf("replay diverged from recorded: %s", rr.Divergence)
			}
		})
	}
	if ran == 0 {
		t.Skip("no recorded fixtures to replay")
	}
}

// TestLiveMatrix runs Track L across the requested models. Opt-in:
//
//	NINE_EVALS_LIVE=1 NINE_EVAL_MODELS=claude-haiku-4-5-20251001 \
//	  NINE_PLUGINS_BIN=$PWD/dist/bin go test ./tests/evals -run TestLiveMatrix
func TestLiveMatrix(t *testing.T) {
	if os.Getenv("NINE_EVALS_LIVE") != "1" {
		t.Skip("live matrix skipped (set NINE_EVALS_LIVE=1 to run)")
	}
	models := splitModels(os.Getenv("NINE_EVAL_MODELS"))
	if len(models) == 0 {
		t.Fatal("set NINE_EVAL_MODELS to a comma-separated model list")
	}

	cases, err := runner.LoadCases(casesDir)
	if err != nil {
		t.Fatalf("load cases: %v", err)
	}
	if only := os.Getenv("NINE_EVAL_TIER"); only != "" {
		cases = filterTier(cases, only)
	}
	// NINE_EVAL_CASES narrows to specific case ids — the fast loop when
	// iterating on one case rather than running a whole tier.
	if only := os.Getenv("NINE_EVAL_CASES"); only != "" {
		cases = filterIDs(cases, splitModels(only))
		if len(cases) == 0 {
			t.Fatalf("NINE_EVAL_CASES=%q matched no cases", only)
		}
	}

	suite := &runner.Suite{
		Harness:     &runner.Harness{PluginBin: os.Getenv("NINE_PLUGINS_BIN"), Embedder: runner.EvalEmbedder()},
		Models:      models,
		ProviderFor: runner.ProviderFor,
		JudgeFn:     runner.NewJudge(runner.ProviderFor),
		ReplayRoot:  replayRoot,
	}

	report := suite.Run(context.Background(), cases)

	stamp := time.Now().UTC().Format("20060102-150405")
	reportPath := filepath.Join(reportsDir, stamp+".json")
	if err := report.WriteJSON(reportPath); err != nil {
		t.Errorf("write report: %v", err)
	}
	t.Logf("report: %s\n\n%s", reportPath, report.RenderGrid())

	if report.Fatal() {
		t.Fatal("eval suite failed: an at-or-above-class case missed its threshold, or a replay diverged")
	}
}

func splitModels(s string) []string {
	var out []string
	for _, m := range strings.Split(s, ",") {
		if m = strings.TrimSpace(m); m != "" {
			out = append(out, m)
		}
	}
	return out
}

// filterIDs keeps only the cases whose id appears in ids.
func filterIDs(cases []*runner.Case, ids []string) []*runner.Case {
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	var out []*runner.Case
	for _, c := range cases {
		if want[c.ID] {
			out = append(out, c)
		}
	}
	return out
}

func filterTier(cases []*runner.Case, tier string) []*runner.Case {
	var out []*runner.Case
	for _, c := range cases {
		if string(c.Tier) == tier {
			out = append(out, c)
		}
	}
	return out
}
