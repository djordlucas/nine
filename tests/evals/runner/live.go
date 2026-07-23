package runner

import (
	"context"
	"fmt"
	"time"

	"nine/internal/llm"
)

// Track L — live models (docs/evals.md §5, §6). A case is run against each
// applicable model `runs` times and marked passing when the pass fraction meets
// pass_threshold. A failure at or above the case's expected_pass_min_class is
// fatal to the suite; a failure below it is reported but tolerated (a 3B model
// missing a 3-hop task is expected signal, not a regression).

// RunOutcome records one repetition's verdict.
type RunOutcome struct {
	Pass     bool     `json:"pass"`
	Failures []string `json:"failures,omitempty"`
	Err      string   `json:"error,omitempty"`
}

// CaseModelResult is a case's aggregate verdict on one model across all runs.
type CaseModelResult struct {
	CaseID      string       `json:"case_id"`
	Model       string       `json:"model"`
	Class       string       `json:"class"`
	Tier        string       `json:"tier"`
	Runs        []RunOutcome `json:"runs"`
	Passes      int          `json:"passes"`
	Threshold   string       `json:"threshold"`
	ThresholdOK bool         `json:"threshold_ok"`
	// Fatal is true when the model failed the threshold at or above the case's
	// expected_pass_min_class — a real regression, versus a tolerated below-class miss.
	Fatal bool `json:"fatal"`
}

// PassFraction returns passes/total as a display string.
func (r CaseModelResult) PassFraction() string {
	return fmt.Sprintf("%d/%d", r.Passes, len(r.Runs))
}

// RunCaseModel runs c against one model `c.Runs` times and aggregates the verdict.
// A per-run timeout of c.TimeoutSecs bounds each repetition. judgeFn may be nil
// (cases without a judge don't need one; a case that declares a judge with a nil
// judgeFn fails in Grade, surfacing the misconfiguration).
func RunCaseModel(ctx context.Context, h *Harness, c *Case, model string, provider llm.Provider, judgeFn JudgeFunc) CaseModelResult {
	res := CaseModelResult{
		CaseID:    c.ID,
		Model:     model,
		Class:     ClassOf(model).String(),
		Tier:      string(c.Tier),
		Threshold: c.PassThreshold,
	}

	for i := 0; i < c.Runs; i++ {
		rctx, cancel := context.WithTimeout(ctx, time.Duration(c.TimeoutSecs)*time.Second)
		out := runOnce(rctx, h, c, provider, judgeFn)
		cancel()
		if out.Pass {
			res.Passes++
		}
		res.Runs = append(res.Runs, out)
	}

	res.ThresholdOK = thresholdMet(c.PassThreshold, res.Passes, len(res.Runs))
	if !res.ThresholdOK {
		expected, _ := ParseClass(c.Models.ExpectedPassMinClass)
		res.Fatal = ClassOf(model) >= expected
	}
	return res
}

// runOnce executes and grades a single repetition.
func runOnce(ctx context.Context, h *Harness, c *Case, provider llm.Provider, judgeFn JudgeFunc) RunOutcome {
	run, err := h.Run(ctx, c, provider)
	if err != nil {
		return RunOutcome{Err: err.Error()}
	}
	defer run.Close()
	g := Grade(c, run, judgeFn)
	return RunOutcome{Pass: g.Pass, Failures: g.Failures}
}

// thresholdMet reports whether passes/total satisfies a "k/n" threshold. The
// threshold's denominator is advisory; the actual denominator is total runs, so
// "2/3" means "at least ceil(2/3 * total) passes". "all" requires every run;
// "any" requires one.
func thresholdMet(threshold string, passes, total int) bool {
	if total == 0 {
		return false
	}
	switch threshold {
	case "all", "":
		return passes == total
	case "any":
		return passes >= 1
	}
	k, n, err := parseThreshold(threshold)
	if err != nil || n == 0 {
		return passes == total
	}
	// Scale the required fraction to the actual run count.
	required := (k*total + n - 1) / n // ceil(k/n * total)
	if required < 1 {
		required = 1
	}
	return passes >= required
}
