package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

// bigOutput returns a string of n copies of a marker, long enough to exceed the
// dispatcher's cap.
func bigOutput(n int, marker string) string {
	return strings.Repeat(marker, n)
}

// spyDispatcher registers a handler returning out under the name "big".
func spyDispatcher(out string) *Dispatcher {
	d := New()
	d.InjectHandler("big", func(context.Context, json.RawMessage) (string, error) {
		return out, nil
	})
	return d
}

func TestSpillStoresFullOutputAndReturnsPath(t *testing.T) {
	full := bigOutput(DefaultMaxOutputTokens*4, "x") + "TAIL-MARKER"
	d := spyDispatcher(full)

	var gotTool, gotOutput string
	d.SetSpill(func(_ context.Context, tool, output string) (string, error) {
		gotTool, gotOutput = tool, output
		return "spill/a1/big-dead.txt", nil
	})

	res, err := d.Dispatch(context.Background(), "big", nil)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if gotTool != "big" {
		t.Errorf("spill sink got tool %q, want %q", gotTool, "big")
	}
	if gotOutput != full {
		t.Errorf("spill sink got %d chars, want the full %d", len(gotOutput), len(full))
	}
	if !res.Truncated {
		t.Error("over-cap result should be marked Truncated")
	}
	if res.SpillPath != "spill/a1/big-dead.txt" {
		t.Errorf("SpillPath = %q, want the sink's path", res.SpillPath)
	}
	if res.OutputChars != utf8.RuneCountInString(full) {
		t.Errorf("OutputChars = %d, want %d", res.OutputChars, utf8.RuneCountInString(full))
	}
	if !strings.Contains(res.Output, "spill/a1/big-dead.txt") {
		t.Error("preview must name the spill path so the model can read it back")
	}
}

// The tail of a long output is where errors and totals live; a head-only cut
// would drop it. This is the behavioral difference from plain truncation.
func TestSpillPreviewKeepsHeadAndTail(t *testing.T) {
	full := "HEAD-MARKER" + bigOutput(DefaultMaxOutputTokens*4, "x") + "TAIL-MARKER"
	d := spyDispatcher(full)
	d.SetSpill(func(context.Context, string, string) (string, error) {
		return "spill/a1/big-dead.txt", nil
	})

	res, err := d.Dispatch(context.Background(), "big", nil)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if !strings.Contains(res.Output, "HEAD-MARKER") {
		t.Error("preview lost the head of the output")
	}
	if !strings.Contains(res.Output, "TAIL-MARKER") {
		t.Error("preview lost the tail of the output — the reason head+tail exists")
	}
}

// maxPreviewOverhead bounds the fixed cost of the banner + elision marker, on
// top of the head/tail slices that total the cap. It is deliberately generous
// (~256 tokens): the banner earns its size by being explicit and imperative
// about which tools to call, which is what makes a model actually retrieve the
// rest instead of guessing (docs/tool-output-spill.md §3). The bound exists to
// stop that from drifting, not to keep it minimal.
const maxPreviewOverhead = 1024

// A successful spill keeps only a SMALL preview: the remainder is retrievable,
// so context spent on it is waste — and, at 8 KB of uniform data, actively
// drowns the task. This is the behavioral difference from the failure path.
func TestSpillPreviewIsSmallWhenSpilled(t *testing.T) {
	full := bigOutput(DefaultMaxOutputTokens*40, "x")
	d := spyDispatcher(full)
	d.SetSpill(func(context.Context, string, string) (string, error) {
		return "spill/a1/big-dead.txt", nil
	})

	res, err := d.Dispatch(context.Background(), "big", nil)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if over := len(res.Output) - spilledPreviewChars; over > maxPreviewOverhead {
		t.Errorf("spilled preview is %d chars over the %d-char preview budget, above the %d-char bound",
			over, spilledPreviewChars, maxPreviewOverhead)
	}
	// And it must be well under the full cap — that is the point.
	if len(res.Output) >= DefaultMaxOutputTokens*4 {
		t.Errorf("spilled preview is %d chars, not smaller than the %d-char cap",
			len(res.Output), DefaultMaxOutputTokens*4)
	}
}

// When the spill FAILS the data really is discarded, so the full cap applies —
// keeping as much as possible is right there.
func TestFailedSpillKeepsFullCap(t *testing.T) {
	full := bigOutput(DefaultMaxOutputTokens*40, "x")
	d := spyDispatcher(full)
	d.SetSpill(func(context.Context, string, string) (string, error) {
		return "", errors.New("store down")
	})

	res, err := d.Dispatch(context.Background(), "big", nil)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	maxChars := DefaultMaxOutputTokens * 4
	if len(res.Output) < maxChars {
		t.Errorf("truncated output is %d chars, want the full %d-char cap when data is lost",
			len(res.Output), maxChars)
	}
}

// A cap lowered below the spilled preview size must still be respected — the
// preview can never be larger than the cap it is replacing.
func TestSpilledPreviewNeverExceedsCap(t *testing.T) {
	full := bigOutput(4000, "x")
	d := spyDispatcher(full)
	d.SetMaxOutputTokens(100) // 400 chars, below spilledPreviewChars
	d.SetSpill(func(context.Context, string, string) (string, error) {
		return "spill/a1/big-dead.txt", nil
	})

	res, err := d.Dispatch(context.Background(), "big", nil)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if over := len(res.Output) - 400; over > maxPreviewOverhead {
		t.Errorf("preview is %d chars over the lowered 400-char cap", over)
	}
}

func TestSpillFailureFallsBackToTruncation(t *testing.T) {
	full := bigOutput(DefaultMaxOutputTokens*4, "x") + "TAIL"
	d := spyDispatcher(full)
	d.SetSpill(func(context.Context, string, string) (string, error) {
		return "", errors.New("store is down")
	})

	res, err := d.Dispatch(context.Background(), "big", nil)
	if err != nil {
		t.Fatalf("a spill failure must not fail the tool call, got: %v", err)
	}
	if !res.Truncated {
		t.Error("expected Truncated=true")
	}
	if res.SpillPath != "" {
		t.Errorf("SpillPath = %q, want empty after a sink failure", res.SpillPath)
	}
	if !strings.Contains(res.Output, "[output truncated]") {
		t.Error("expected the plain truncation marker as the fallback")
	}
}

// Without a sink registered the dispatcher must behave exactly as it did
// before spilling existed.
func TestNoSpillSinkTruncatesAsBefore(t *testing.T) {
	full := bigOutput(DefaultMaxOutputTokens*4, "x") + "TAIL"
	res, err := spyDispatcher(full).Dispatch(context.Background(), "big", nil)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if !res.Truncated || !strings.HasSuffix(res.Output, "[output truncated]") {
		t.Errorf("want legacy truncation, got truncated=%v output ending %q",
			res.Truncated, res.Output[max(0, len(res.Output)-40):])
	}
	if res.SpillPath != "" {
		t.Error("no sink means no spill path")
	}
}

func TestUnderCapOutputUntouched(t *testing.T) {
	const out = "small enough"
	res, err := spyDispatcher(out).Dispatch(context.Background(), "big", nil)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if res.Output != out || res.Truncated || res.SpillPath != "" || res.OutputChars != 0 {
		t.Errorf("under-cap result was modified: %+v", res)
	}
}

// Slicing an over-cap output at a byte offset must not split a multi-byte rune
// — the old truncation could, producing a broken observation.
func TestSpillPreviewIsRuneSafe(t *testing.T) {
	// "é" is two bytes, so a naive byte cut lands mid-rune for some lengths.
	full := strings.Repeat("é", DefaultMaxOutputTokens*4)
	d := spyDispatcher(full)
	d.SetSpill(func(context.Context, string, string) (string, error) {
		return "spill/a1/big-dead.txt", nil
	})

	res, err := d.Dispatch(context.Background(), "big", nil)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if !utf8.ValidString(res.Output) {
		t.Error("preview split a multi-byte rune")
	}
}

func TestSetMaxOutputTokens(t *testing.T) {
	full := bigOutput(200, "x") // 200 chars
	d := spyDispatcher(full)
	d.SetMaxOutputTokens(10) // 40 chars

	res, err := d.Dispatch(context.Background(), "big", nil)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if !res.Truncated {
		t.Error("a lowered cap should truncate a 200-char output")
	}

	d2 := spyDispatcher(full)
	d2.SetMaxOutputTokens(0) // ignored
	res2, err := d2.Dispatch(context.Background(), "big", nil)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if res2.Truncated {
		t.Error("a non-positive cap must be ignored, keeping the default")
	}
}

func TestClipHeadTailBoundaries(t *testing.T) {
	const s = "héllo wörld"
	for n := range len(s) + 2 {
		if got := clipHead(s, n); !utf8.ValidString(got) || len(got) > n {
			t.Errorf("clipHead(%d) = %q: invalid or too long", n, got)
		}
		if got := clipTail(s, n); !utf8.ValidString(got) || len(got) > n {
			t.Errorf("clipTail(%d) = %q: invalid or too long", n, got)
		}
	}
	if got := clipHead(s, len(s)+5); got != s {
		t.Errorf("clipHead beyond length = %q, want the whole string", got)
	}
	if got := clipTail(s, len(s)+5); got != s {
		t.Errorf("clipTail beyond length = %q, want the whole string", got)
	}
}

func TestSpillPathPrefixIsReserved(t *testing.T) {
	if !strings.HasSuffix(SpillPathPrefix, "/") {
		t.Fatalf("SpillPathPrefix %q must end in / so prefix matching cannot catch a sibling path",
			SpillPathPrefix)
	}
}

func ExampleDispatcher_SetSpill() {
	d := New()
	d.InjectHandler("fetch", func(context.Context, json.RawMessage) (string, error) {
		return strings.Repeat("data ", 4000), nil
	})
	d.SetSpill(func(_ context.Context, tool, output string) (string, error) {
		return "spill/demo/" + tool + "-01.txt", nil
	})

	res, _ := d.Dispatch(context.Background(), "fetch", nil)
	fmt.Println(res.SpillPath, res.Truncated)
	// Output: spill/demo/fetch-01.txt true
}
