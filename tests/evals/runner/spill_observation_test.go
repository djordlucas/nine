package runner

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"nine/internal/llm"
)

// TestSpillObservationFromRealTool is the end-to-end proof of the large-output
// path with the model removed from the equation: a real plugin (shell) produces
// ~128 KB, the real dispatcher spills it to the real store, and this asserts on
// exactly what a model would have received as its observation.
//
// It exists because a live-model eval cannot distinguish "the spill machinery
// broke" from "the model failed to read its own observation"
// (adr/tool-output-spill.md §8).
func TestSpillObservationFromRealTool(t *testing.T) {
	nineBin := os.Getenv("NINE_BINARY")
	if nineBin == "" {
		t.Skip("set NINE_BINARY (make build) to run the shell plugin")
	}

	c := &Case{
		ID:      "spill-observation",
		Prompts: []string{"run it"},
		Expect:  Expect{Trajectory: Trajectory{NoStall: true}},
	}
	c.defaults()
	h := requireHarness(t)
	h.NineBin = nineBin

	res, err := h.Run(context.Background(), c, scriptedProvider([]llm.Response{
		{ToolCalls: []llm.ToolCall{toolCall("1", "shell", map[string]any{
			"command": "seq 1 20000; echo SPILL-TAIL-MARKER",
		})}, StopReason: "tool_use"},
		{Text: "done", StopReason: "end_turn"},
	}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer res.Close()

	var p struct {
		Name        string `json:"name"`
		Output      string `json:"output"`
		Truncated   bool   `json:"truncated"`
		SpillPath   string `json:"spill_path"`
		OutputChars int    `json:"output_chars"`
		Error       string `json:"error"`
	}
	found := false
	for _, e := range res.Events {
		if e.Type != "tool_end" {
			continue
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("unmarshal tool_end: %v", err)
		}
		if p.Name == "shell" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("no shell tool_end event in the journal")
	}
	if p.Error != "" {
		t.Fatalf("shell failed: %s", p.Error)
	}

	// 1. The result was recognized as over-cap and spilled.
	if !p.Truncated {
		t.Error("a 128 KB result was not marked truncated")
	}
	if p.SpillPath == "" {
		t.Fatal("a 128 KB result was not spilled")
	}
	if p.OutputChars < 100_000 {
		t.Errorf("output_chars = %d, want the real ~128k size", p.OutputChars)
	}

	// 2. The observation is a SMALL preview, far under the 8192-char cap: once
	//    the output is retrievable, spending context on it is waste (and enough
	//    of it drowns the request). See adr/tool-output-spill.md §3.
	const cap8k = 2048 * 4
	if len(p.Output) > cap8k/2 {
		t.Errorf("observation is %d bytes; a spilled preview should be far under the %d-char cap",
			len(p.Output), cap8k)
	}

	// 3. The tail survived — the behavior head-only truncation lacked, and the
	//    reason a model can answer "what did it print last?" at all.
	if !strings.Contains(p.Output, "SPILL-TAIL-MARKER") {
		t.Error("the observation lost the end of the output; head+tail preview is broken")
	}
	// 4. The head survived too.
	if !strings.Contains(p.Output, `"stdout":"1\n2\n`) {
		t.Error("the observation lost the start of the output")
	}
	// 5. The notice tells the model where the rest is.
	if !strings.Contains(p.Output, p.SpillPath) {
		t.Error("the observation does not name the spill path")
	}

	// 6. The full output really is in the store, intact.
	stored, ok, err := res.Store.FileFetch(p.SpillPath)
	if err != nil || !ok {
		t.Fatalf("spilled file missing: err=%v found=%v", err, ok)
	}
	if len(stored) < 100_000 {
		t.Errorf("stored %d chars, want the full ~128k", len(stored))
	}
	if !strings.Contains(stored, "SPILL-TAIL-MARKER") {
		t.Error("the stored spill is missing the end of the output")
	}
	// The middle — elided from the observation — is only in the store.
	if !strings.Contains(stored, `10000\n`) {
		t.Error("the stored spill is missing its middle")
	}
}
