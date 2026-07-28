package agent

import (
	"context"
	"fmt"
	"log/slog"
	"unicode/utf8"
)

// SpillPathPrefix is the reserved file-store namespace that over-cap tool
// output is written to. It is daemon-owned: `file_store` refuses to write
// under it (see RegisterMemoryTools) so a spill is always exactly what a tool
// returned, never something the model composed. Nothing under this prefix is
// embedded into the vector pool either, so a spilled blob can never be
// pull-surfaced into a later turn's context — it enters context only when the
// model explicitly reads it back.
const SpillPathPrefix = "spill/"

// SpillFn persists the full output of an over-cap tool call and returns the
// file-store path it was written to. A non-nil error makes the dispatcher fall
// back to plain truncation, so a spill failure degrades rather than failing the
// tool call (R-DISP.2).
type SpillFn func(ctx context.Context, toolName string, output string) (string, error)

// SetSpill registers fn as the overflow sink for over-cap tool output. With no
// sink registered the dispatcher truncates as it always has.
func (d *Dispatcher) SetSpill(fn SpillFn) { d.spill = fn }

// SetMaxOutputTokens overrides the per-result output cap. Values <= 0 are
// ignored, keeping DefaultMaxOutputTokens.
func (d *Dispatcher) SetMaxOutputTokens(n int) {
	if n > 0 {
		d.maxOutputTokens = n
	}
}

// maxOutputChars is the cap in characters, using the 4-chars-per-token estimate
// the context builder uses (internal/context/builder.go).
func (d *Dispatcher) maxOutputChars() int { return d.maxOutputTokens * 4 }

// spilledPreviewChars is how much of the output the preview keeps when the
// spill SUCCEEDED — far less than the cap, deliberately.
//
// The cap is sized for the pre-spill world, where truncation had to keep as
// much as possible because everything past the cut was destroyed. Once the full
// output is in the store, the remainder is one tool call away, so a large
// preview stops being insurance and becomes pure cost — and an actively harmful
// one: a live 12B model handed 8 KB of newline-escaped digits lost track of the
// request entirely and answered "no specific task was provided in your prompt."
// A small preview plus a clear pointer leaves room for the task to survive.
//
// When the spill FAILS the full cap still applies (see capOrSpill): there the
// data really is being discarded, so keeping as much as possible is right.
const spilledPreviewChars = 1536

// spilledHeadFraction splits the spilled preview between head and tail. The
// tail gets a third — enough for the closing structure, errors, and totals that
// a head-only cut would lose.
const spilledHeadFraction = 2.0 / 3.0

// capOrSpill enforces the output cap. Output within the cap passes through
// untouched. Over-cap output is written to the spill sink (when one is
// registered) and replaced with a head+tail preview naming the path; without a
// sink, or if the sink fails, it falls back to a head-only truncation.
func (d *Dispatcher) capOrSpill(ctx context.Context, toolName, output string) CallResult {
	maxChars := d.maxOutputChars()
	if len(output) <= maxChars {
		return CallResult{Output: output}
	}

	res := CallResult{Truncated: true, OutputChars: utf8.RuneCountInString(output)}
	if d.spill == nil {
		res.Output = clipHead(output, maxChars) + "\n[output truncated]"
		return res
	}

	path, err := d.spill(ctx, toolName, output)
	if err != nil {
		// Graceful degradation: the model still gets the truncated output it
		// would have got before spilling existed.
		slog.Warn("tool output spill failed, truncating instead",
			"tool", toolName, "bytes", len(output), "err", err)
		res.Output = clipHead(output, maxChars) + "\n[output truncated]"
		return res
	}

	res.SpillPath = path
	// Preview budget is the smaller spilled size, not the cap — the rest is
	// retrievable now, so context spent on it is wasted (see
	// spilledPreviewChars). Never exceed the cap for a tiny over-cap result.
	previewChars := min(spilledPreviewChars, maxChars)
	res.Output = spillPreview(output, previewChars, path)
	slog.Info("tool output spilled", "tool", toolName, "bytes", len(output), "path", path)
	return res
}

// spillPreview renders the over-cap output as a leading banner, a head slice, a
// short elision marker, and a tail slice.
//
// The tail matters: errors, totals, and closing structure live at the end of a
// long output, and a head-only cut throws exactly that away.
//
// The banner leads rather than sitting only at the elision point, because the
// head can be several thousand characters — a model reading top-down would
// otherwise consume a wall of data before learning it was truncated or that the
// rest is retrievable. Stating it first is what makes the preview actionable.
//
// The two slices together total previewChars, so the preview's context cost is
// bounded and predictable. Counts are in characters, matching the unit
// file_fetch's offset/limit address, so the model can slice the remainder
// without a unit conversion.
func spillPreview(output string, previewChars int, path string) string {
	head := clipHead(output, int(float64(previewChars)*spilledHeadFraction))
	tail := clipTail(output, previewChars-len(head))
	total := utf8.RuneCountInString(output)
	headChars := utf8.RuneCountInString(head)
	tailChars := utf8.RuneCountInString(tail)

	// The instructions name tools to CALL, with their arguments spelled out as
	// arguments. An earlier revision wrote them as function signatures —
	// "file_fetch(path, offset, limit)" — and live models responded by writing
	// code (a Python snippet, a JSON fragment) instead of issuing a tool call.
	// Phrasing shapes behavior here, so keep it imperative and tool-shaped.
	banner := fmt.Sprintf(
		"[nine: this output was %d characters — too large to show in full.\n"+
			"The COMPLETE output is saved in the memory file store at this path:\n"+
			"    %s\n"+
			"Below you see only the first %d characters and the last %d; the middle is missing.\n"+
			"If what you need is not visible below, do NOT guess and do NOT write code —\n"+
			"call one of these tools now:\n"+
			"  * file_search_text — set query to the text you are looking for and path to\n"+
			"    the path above, to find where it occurs in the full output.\n"+
			"  * file_fetch — set path to the path above, plus offset and limit, to read\n"+
			"    one window of the full output at a time.\n"+
			"You can also give the path above to another tool's *_ref argument to hand it\n"+
			"the entire content without reading it yourself.]\n\n",
		total, path, headChars, tailChars)

	marker := fmt.Sprintf("\n\n[... %d characters elided ...]\n\n", total-headChars-tailChars)

	return banner + head + marker + tail
}

// clipHead returns the longest prefix of s at most n bytes long that does not
// split a UTF-8 rune.
func clipHead(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// clipTail returns the longest suffix of s at most n bytes long that does not
// split a UTF-8 rune.
func clipTail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	i := len(s) - n
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return s[i:]
}
