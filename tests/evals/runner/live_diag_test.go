package runner

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// TestDiagLiveTrajectory runs one case ONCE against a live model and prints the
// full trajectory, so a failure can be attributed to the model's choices rather
// than guessed at. Temporary diagnostic; not part of the suite.
func TestDiagLiveTrajectory(t *testing.T) {
	model := os.Getenv("DIAG_MODEL")
	caseID := os.Getenv("DIAG_CASE")
	if model == "" || caseID == "" {
		t.Skip("set DIAG_MODEL and DIAG_CASE")
	}

	cases, err := LoadCases("../cases")
	if err != nil {
		t.Fatalf("load cases: %v", err)
	}
	var c *Case
	for _, cc := range cases {
		if cc.ID == caseID {
			c = cc
		}
	}
	if c == nil {
		t.Fatalf("case %q not found", caseID)
	}

	provider, err := ProviderFor(model)
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	h := requireHarness(t)
	h.NineBin = os.Getenv("NINE_BINARY")
	h.Embedder = EvalEmbedder()
	h.ContextBudget = EvalContextBudget()

	res, err := h.Run(context.Background(), c, provider)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer res.Close()

	t.Logf("=== case %s on %s ===", caseID, model)
	for _, e := range res.Events {
		switch e.Type {
		case "turn_start":
			var p struct {
				Input   string `json:"input"`
				Trigger string `json:"trigger"`
			}
			_ = json.Unmarshal(e.Payload, &p)
			t.Logf("TURN (%s): %s", p.Trigger, p.Input)
		case "llm_request":
			// The tools the model was offered: a tool it never called may simply
			// not have been in the list.
			var p struct {
				ToolNames []string `json:"tool_names"`
			}
			_ = json.Unmarshal(e.Payload, &p)
			t.Logf("OFFERED %d tools: %s", len(p.ToolNames), strings.Join(p.ToolNames, " "))
		case "tool_start":
			var p struct {
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			}
			_ = json.Unmarshal(e.Payload, &p)
			in := string(p.Input)
			if len(in) > 160 {
				in = in[:160] + "…"
			}
			t.Logf("CALL %s %s", p.Name, in)
		case "tool_end":
			var p struct {
				Name      string `json:"name"`
				Output    string `json:"output"`
				SpillPath string `json:"spill_path"`
				Error     string `json:"error"`
			}
			_ = json.Unmarshal(e.Payload, &p)
			out := p.Output
			if len(out) > 160 {
				out = out[:160] + "…"
			}
			t.Logf("DONE %s spill=%q err=%q out=%s", p.Name, p.SpillPath, p.Error, out)
		case "turn_end":
			var p struct {
				Result string `json:"result"`
				Error  string `json:"error"`
			}
			_ = json.Unmarshal(e.Payload, &p)
			r := p.Result
			if len(r) > 300 {
				r = r[:300] + "…"
			}
			t.Logf("ANSWER (err=%q): %s", p.Error, r)
		}
	}
	g := Grade(c, res, nil)
	t.Logf("VERDICT pass=%v failures=%v", g.Pass, g.Failures)
}
