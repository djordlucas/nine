package toolvm

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

const counterJobManifest = `
name = "batcher"
kind = "js"
entrypoint = "./batcher.js"
description = "Process a range in bounded batches."
resumable = true
`

// The shape the whole feature is for: bounded work per call, a cursor carrying
// the resume point, a final result when there is nothing left.
const counterJobSrc = `
import { again } from "nine:job";
export default function (args, job) {
  const at = Number(job?.cursor ?? 0);
  const next = at + 2;
  if (next >= args.total) return "done at " + args.total + " after call " + job.call;
  return again({ cursor: String(next), progress: next + "/" + args.total, afterMs: 10 });
}
`

func TestJobCallReturnsAContinuation(t *testing.T) {
	dir := t.TempDir()
	writeTool(t, dir, "batcher", counterJobManifest, counterJobSrc)
	h := openHost(t, dir, nil)

	out, err := h.CallJob(context.Background(), "batcher",
		json.RawMessage(`{"total":6}`), JobContext{Call: 1})
	if err != nil {
		t.Fatal(err)
	}
	if out.Continue == nil {
		t.Fatalf("first call produced %+v, want a continuation", out)
	}
	if out.Continue.Cursor != "2" {
		t.Errorf("cursor = %q, want 2", out.Continue.Cursor)
	}
	if out.Continue.Progress != "2/6" {
		t.Errorf("progress = %q, want 2/6", out.Continue.Progress)
	}
	if out.Continue.AfterMS != 10 {
		t.Errorf("after_ms = %d, want 10", out.Continue.AfterMS)
	}
	if out.Text != "" {
		t.Errorf("a continuation must carry no output, got %q", out.Text)
	}
}

// The cursor round-trips: what the tool returned last call is what it is handed
// next call. This is the only state that crosses, and nothing in the instance
// survives to carry it.
func TestJobCursorRoundTrips(t *testing.T) {
	dir := t.TempDir()
	writeTool(t, dir, "batcher", counterJobManifest, counterJobSrc)
	h := openHost(t, dir, nil)

	var (
		cursor string
		calls  int
		final  string
	)
	for range 10 {
		calls++
		out, err := h.CallJob(context.Background(), "batcher",
			json.RawMessage(`{"total":6}`), JobContext{Cursor: cursor, Call: calls})
		if err != nil {
			t.Fatal(err)
		}
		if out.Continue == nil {
			final = out.Text
			break
		}
		cursor = out.Continue.Cursor
	}

	if final != "done at 6 after call 3" {
		t.Fatalf("final = %q, want the run to finish on call 3", final)
	}
	if calls != 3 {
		t.Fatalf("took %d calls, want 3", calls)
	}
}

// A tool cannot acquire a lifecycle by returning a field. The manifest is
// authoritative for shape (R-TVM.10), so an undeclared tool returning `continue`
// is an error rather than a job that runs forever.
func TestContinueRequiresResumableInTheManifest(t *testing.T) {
	dir := t.TempDir()
	writeTool(t, dir, "sneaky", `
name = "sneaky"
kind = "js"
entrypoint = "./sneaky.js"
description = "Return a continuation without declaring one."
`, `
import { again } from "nine:job";
export default function () { return again({ cursor: "1" }); }
`)
	h := openHost(t, dir, nil)

	_, err := h.Call(context.Background(), "sneaky", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("an undeclared tool returned a continuation and was accepted")
	}
	if !strings.Contains(err.Error(), "resumable") {
		t.Fatalf("error = %v, want it to name the missing manifest field", err)
	}
}

// CallJob refuses a tool that is not resumable, so the driver cannot be pointed
// at an ordinary tool by a stale registry row.
func TestCallJobRefusesANonResumableTool(t *testing.T) {
	dir := t.TempDir()
	writeTool(t, dir, "echo", echoManifest, `export default (a) => String(a.value);`)
	h := openHost(t, dir, nil)

	if _, err := h.CallJob(context.Background(), "echo",
		json.RawMessage(`{"value":1}`), JobContext{Call: 1}); err == nil {
		t.Fatal("CallJob ran a tool that never declared itself resumable")
	}
}

// An ordinary call hands the tool no job context at all, so a tool that was
// never part of a job cannot mistake one for a resumption.
func TestOrdinaryCallHasNoJobContext(t *testing.T) {
	dir := t.TempDir()
	writeTool(t, dir, "probe", `
name = "probe"
kind = "js"
entrypoint = "./probe.js"
description = "Report whether a job context was passed."
`, `export default function (args, job) { return job === undefined ? "none" : "present"; }`)
	h := openHost(t, dir, nil)

	if got := call(t, h, "probe", `{}`); got != "none" {
		t.Fatalf("ordinary call saw a job context (%q)", got)
	}
}

// The generated tier's gate. The capability ceiling bounds reach and cannot
// express duration, so running forever needs its own switch.
func TestGeneratedResumableNeedsTheOperatorsSwitch(t *testing.T) {
	src := `import { again } from "nine:job";
export default function (a, j) { return again({ cursor: "1" }); }`
	gen := Generated{
		Name: "grinder", Description: "grind", Source: src,
		InputSchema: json.RawMessage(`{"type":"object"}`), Resumable: true,
	}

	t.Run("off by default", func(t *testing.T) {
		h := openHost(t, t.TempDir(), nil)
		h.SetAgentConfig(AgentConfig{Enabled: true})
		h.LoadGenerated(context.Background(), []Generated{gen}, nil)

		if h.Get("grinder") != nil {
			t.Fatal("a resumable generated tool registered with allow_long_running unset")
		}
		if !skippedFor(h, "grinder", "allow_long_running") {
			t.Fatalf("status does not name the switch: %+v", h.Status())
		}
	})

	t.Run("on when enabled", func(t *testing.T) {
		h := openHost(t, t.TempDir(), nil)
		h.SetAgentConfig(AgentConfig{Enabled: true, AllowLongRunning: true})
		h.LoadGenerated(context.Background(), []Generated{gen}, nil)

		got := h.Get("grinder")
		if got == nil {
			t.Fatalf("tool did not register: %+v", h.Status())
		}
		if !got.Resumable {
			t.Error("registered tool is not marked resumable")
		}
	})

	t.Run("a non-resumable generated tool is unaffected", func(t *testing.T) {
		h := openHost(t, t.TempDir(), nil)
		h.SetAgentConfig(AgentConfig{Enabled: true})
		plain := gen
		plain.Resumable = false
		plain.Source = `export default () => "ok";`
		h.LoadGenerated(context.Background(), []Generated{plain}, nil)

		if h.Get("grinder") == nil {
			t.Fatalf("the gate blocked an ordinary generated tool: %+v", h.Status())
		}
	})
}

// js_eval persists nothing by definition, and a job is persistence: there is no
// row to carry a cursor and nothing to resume. A continuation is refused.
func TestJSEvalRefusesAContinuation(t *testing.T) {
	h := openHost(t, t.TempDir(), nil)
	h.SetAgentConfig(AgentConfig{Enabled: true, AllowLongRunning: true})

	_, err := h.EvalGenerated(context.Background(),
		`import { again } from "nine:job";
		 export default () => again({ cursor: "1" });`,
		Declaration{}, json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("js_eval accepted a continuation; there is nowhere to resume from")
	}
	if !strings.Contains(err.Error(), "resumable") {
		t.Fatalf("error = %v, want it to explain why", err)
	}
}
