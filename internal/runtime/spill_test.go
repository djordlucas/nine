package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"nine/internal/agent"
	"nine/internal/memory/memtest"
)

func TestSpillPathShape(t *testing.T) {
	p := spillPath("agent-42", "http_get")
	if !strings.HasPrefix(p, agent.SpillPathPrefix) {
		t.Errorf("path %q must live under %q so file_store's guard and the sweep both catch it",
			p, agent.SpillPathPrefix)
	}
	if !strings.HasPrefix(p, "spill/agent-42/http_get-") || !strings.HasSuffix(p, ".txt") {
		t.Errorf("path %q does not have the spill/<agent>/<tool>-<rand>.txt shape", p)
	}
	// Parallel tool calls in one turn must not collide.
	if p == spillPath("agent-42", "http_get") {
		t.Error("two spills of the same tool produced identical paths")
	}
}

func TestSanitizePathSegment(t *testing.T) {
	tests := map[string]string{
		"http_get":      "http_get",
		"a/../../etc":   "a-------etc", // no segment can introduce a directory boundary
		"weird name!":   "weird-name-",
		"":              "unknown",
		"Agent-42_beta": "Agent-42_beta",
	}
	for in, want := range tests {
		if got := sanitizePathSegment(in); got != want {
			t.Errorf("sanitizePathSegment(%q) = %q, want %q", in, got, want)
		}
	}
	if strings.Contains(sanitizePathSegment("a/b"), "/") {
		t.Error("a sanitized segment must never contain a path separator")
	}
}

// The whole point of the feature, end to end against a real store: a tool
// returns more than the cap, the daemon stores it and hands the model a path,
// and a second tool receives the full payload through that path without the
// bytes ever appearing in an observation.
func TestSpillThenRefRoundTrip(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}

	payload := strings.Repeat("PAYLOAD-", agent.DefaultMaxOutputTokens) // well over the cap
	d := agent.New()
	d.InjectHandler("fetch_big", func(context.Context, json.RawMessage) (string, error) {
		return payload, nil
	})
	var consumed string
	// `content` is declared as a ref exactly as a plugin would declare it in the
	// input schema it advertises.
	d.InjectHandlerWithSchema("consume",
		json.RawMessage(`{"type":"object","properties":{"content":{"type":"string","x-nine-ref":true}}}`),
		func(_ context.Context, args json.RawMessage) (string, error) {
			var req struct {
				Content string `json:"content"`
			}
			if err := json.Unmarshal(args, &req); err != nil {
				return "", err
			}
			consumed = req.Content
			return "consumed", nil
		})
	registerLargeOutput(d, store, "agent-42")

	// Step 1: the over-cap fetch spills.
	res, err := d.Dispatch(context.Background(), "fetch_big", nil)
	if err != nil {
		t.Fatalf("fetch_big: %v", err)
	}
	if res.SpillPath == "" {
		t.Fatal("over-cap output was not spilled")
	}
	if strings.Contains(res.Output, payload) {
		t.Error("the observation contains the whole payload; it should be a preview")
	}
	if !strings.Contains(res.Output, res.SpillPath) {
		t.Error("the observation must name the path so the model can route it")
	}

	// The full output really is in the store, in one piece.
	stored, found, err := store.FileFetch(res.SpillPath)
	if err != nil || !found {
		t.Fatalf("spilled file not in the store: err=%v found=%v", err, found)
	}
	if stored != payload {
		t.Errorf("stored %d chars, want the full %d", len(stored), len(payload))
	}

	// Step 2: the model passes the path to the tool's declared ref parameter.
	args, _ := json.Marshal(map[string]string{"content": res.SpillPath})
	if _, err := d.Dispatch(context.Background(), "consume", args); err != nil {
		t.Fatalf("consume: %v", err)
	}
	if consumed != payload {
		t.Errorf("the second tool received %d chars, want the full %d payload",
			len(consumed), len(payload))
	}
}

func TestSweepSpillsRemovesOnlySpills(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FileStore("spill/agent-1/tool-aa.txt", "debris"); err != nil {
		t.Fatal(err)
	}
	if err := store.FileStore("runbooks/keep.md", "durable"); err != nil {
		t.Fatal(err)
	}

	// A fresh spill survives the real retention window.
	sweepSpills(store)
	if _, found, _ := store.FileFetch("spill/agent-1/tool-aa.txt"); !found {
		t.Error("the sweep deleted a spill inside the retention window")
	}
	if _, found, _ := store.FileFetch("runbooks/keep.md"); !found {
		t.Error("the sweep deleted a non-spill file")
	}
}

func TestRegisterLargeOutputWithNilStore(t *testing.T) {
	d := agent.New()
	d.InjectHandler("big", func(context.Context, json.RawMessage) (string, error) {
		return strings.Repeat("x", agent.DefaultMaxOutputTokens*8), nil
	})
	registerLargeOutput(d, nil, "agent-1") // must not panic or register a sink

	res, err := d.Dispatch(context.Background(), "big", nil)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if res.SpillPath != "" {
		t.Error("no store means no spilling")
	}
	if !res.Truncated {
		t.Error("without a store the output should still be capped")
	}
}
