package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"nine/internal/llm"
)

// Suite ties the pieces together: it runs Track R (replay fixtures) and Track L
// (the live model matrix) over a set of cases and produces a Report with a
// rendered cases × models grid (docs/evals.md §8).
type Suite struct {
	Harness     *Harness
	Models      []string                                 // requested matrix (Track L)
	ProviderFor func(model string) (llm.Provider, error) // builds a provider per model
	JudgeFn     JudgeFunc                                // may be nil
	ReplayRoot  string                                   // tests/evals/replay
}

// Report is the full result of a suite run: per-(case,model) live verdicts and
// per-case replay verdicts, plus the timing envelope.
type Report struct {
	StartedAt  time.Time          `json:"started_at"`
	FinishedAt time.Time          `json:"finished_at"`
	Models     []string           `json:"models"`
	Live       []CaseModelResult  `json:"live"`
	Replay     []ReplayCaseResult `json:"replay"`
}

// ReplayCaseResult is one Track-R case's verdict.
type ReplayCaseResult struct {
	CaseID     string `json:"case_id"`
	Pass       bool   `json:"pass"`
	Divergence string `json:"divergence,omitempty"`
	Err        string `json:"error,omitempty"`
}

// Fatal reports whether the suite should fail CI: any replay divergence/error or
// any at-or-above-class live threshold miss.
func (r *Report) Fatal() bool {
	for _, rc := range r.Replay {
		if !rc.Pass {
			return true
		}
	}
	for _, lc := range r.Live {
		if lc.Fatal {
			return true
		}
	}
	return false
}

// RunReplay executes every Track-R (replay/both) case that has a recorded
// fixture and returns the verdicts. Cases without a fixture yet are reported as
// errors so a missing recording is visible rather than silently skipped.
func (s *Suite) RunReplay(ctx context.Context, cases []*Case) []ReplayCaseResult {
	var out []ReplayCaseResult
	for _, c := range cases {
		if c.Track != TrackReplay && c.Track != TrackBoth {
			continue
		}
		dir := ReplayDir(s.ReplayRoot, c.ID)
		rc := ReplayCaseResult{CaseID: c.ID}
		if _, err := os.Stat(filepath.Join(dir, journalFile)); err != nil {
			rc.Err = "no recorded fixture (record it first)"
			out = append(out, rc)
			continue
		}
		rr, err := ReplayFixture(ctx, dir)
		if err != nil {
			rc.Err = err.Error()
		} else {
			rc.Pass = rr.Pass()
			rc.Divergence = rr.Divergence
		}
		out = append(out, rc)
	}
	return out
}

// RunLive executes every Track-L (live/both) case against its applicable models
// and returns the aggregated verdicts. A model with no provider (e.g. missing
// API key) yields a fatal-free error result so the grid still renders.
func (s *Suite) RunLive(ctx context.Context, cases []*Case) []CaseModelResult {
	var out []CaseModelResult
	for _, c := range cases {
		if c.Track != TrackLive && c.Track != TrackBoth {
			continue
		}
		// Recorded, not dropped: a case that never ran must not read as a pass.
		if missing := c.MissingEnv(); len(missing) > 0 {
			for _, model := range c.ApplicableModels(s.Models) {
				out = append(out, CaseModelResult{
					CaseID: c.ID, Model: model, Class: ClassOf(model).String(),
					Tier: string(c.Tier), Threshold: c.PassThreshold,
					Skipped: "requires " + strings.Join(missing, ", "),
				})
			}
			continue
		}
		for _, model := range c.ApplicableModels(s.Models) {
			provider, err := s.ProviderFor(model)
			if err != nil {
				out = append(out, CaseModelResult{
					CaseID: c.ID, Model: model, Class: ClassOf(model).String(),
					Tier: string(c.Tier), Threshold: c.PassThreshold,
					Runs: []RunOutcome{{Err: err.Error()}},
				})
				continue
			}
			out = append(out, RunCaseModel(ctx, s.Harness, c, model, provider, s.JudgeFn))
		}
	}
	return out
}

// Run executes both tracks and returns a Report.
func (s *Suite) Run(ctx context.Context, cases []*Case) *Report {
	r := &Report{StartedAt: time.Now(), Models: s.Models}
	r.Replay = s.RunReplay(ctx, cases)
	r.Live = s.RunLive(ctx, cases)
	r.FinishedAt = time.Now()
	return r
}

// WriteJSON writes the report as pretty JSON to path (creating parent dirs).
func (r *Report) WriteJSON(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// RenderGrid renders the live results as a cases × models table, with a leading
// Track-R column. A cell shows the pass fraction; a trailing "!" marks a fatal
// (at-or-above-class) miss, "~" a tolerated below-class miss.
func (r *Report) RenderGrid() string {
	// Collect the case order and the model columns actually present.
	var caseIDs []string
	seenCase := map[string]bool{}
	byCaseModel := map[string]CaseModelResult{}
	modelSet := map[string]bool{}
	for _, lc := range r.Live {
		if !seenCase[lc.CaseID] {
			seenCase[lc.CaseID] = true
			caseIDs = append(caseIDs, lc.CaseID)
		}
		byCaseModel[lc.CaseID+"\x00"+lc.Model] = lc
		modelSet[lc.Model] = true
	}
	replayByCase := map[string]ReplayCaseResult{}
	for _, rc := range r.Replay {
		replayByCase[rc.CaseID] = rc
		if !seenCase[rc.CaseID] {
			seenCase[rc.CaseID] = true
			caseIDs = append(caseIDs, rc.CaseID)
		}
	}
	models := r.Models
	if len(models) == 0 {
		for m := range modelSet {
			models = append(models, m)
		}
		sort.Strings(models)
	}

	var b strings.Builder
	// Header.
	fmt.Fprintf(&b, "%-32s %-8s", "case", "replay")
	for _, m := range models {
		fmt.Fprintf(&b, " %-14s", truncate(m, 14))
	}
	b.WriteByte('\n')
	// Rows.
	for _, id := range caseIDs {
		fmt.Fprintf(&b, "%-32s ", truncate(id, 32))
		rc, hasReplay := replayByCase[id]
		switch {
		case !hasReplay:
			fmt.Fprintf(&b, "%-8s", "-")
		case rc.Pass:
			fmt.Fprintf(&b, "%-8s", "pass")
		default:
			fmt.Fprintf(&b, "%-8s", "FAIL")
		}
		for _, m := range models {
			lc, ok := byCaseModel[id+"\x00"+m]
			if !ok {
				fmt.Fprintf(&b, " %-14s", "-")
				continue
			}
			if lc.Skipped != "" {
				fmt.Fprintf(&b, " %-14s", "skip")
				continue
			}
			cell := lc.PassFraction()
			switch {
			case lc.ThresholdOK:
				// clean
			case lc.Fatal:
				cell += " !"
			default:
				cell += " ~"
			}
			fmt.Fprintf(&b, " %-14s", cell)
		}
		b.WriteByte('\n')
	}
	b.WriteString("\nlegend: ! = fatal (>= expected class)   ~ = tolerated (below expected class)\n")
	return b.String()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}
