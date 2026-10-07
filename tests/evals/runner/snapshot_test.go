package runner

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"nine/internal/embed/keyword"
	"nine/internal/llm"
)

// updateSnapshots rewrites the golden files instead of comparing against them:
//
//	go test ./tests/evals/runner/ -run TestTurnSnapshots -update
var updateSnapshots = flag.Bool("update", false, "rewrite the turn snapshots in testdata/snapshots")

// The turn snapshots freeze what the model receives — system prompt, messages,
// advertised tools with their schemas, token limits, thinking — and the journal
// each session leaves, for each kind of session, through the production assembly the evals use. They exist to
// show that a refactor of how sessions are built (adr/agent-boundary.md, phase
// 1) changes nothing a model can see. A deliberate change re-records them with
// -update and shows up in review as a diff of these files.

// snapshotProvider records every request and answers from a fixed script, so
// a session's requests depend only on the code that assembles them.
type snapshotProvider struct {
	mu     sync.Mutex
	calls  []llm.Request
	script func(n int, req llm.Request) llm.Response
}

func (p *snapshotProvider) Complete(_ context.Context, req llm.Request) (llm.Response, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, req)
	return p.script(len(p.calls), req), nil
}

func answer(text string) llm.Response {
	return llm.Response{Text: text, StopReason: "end_turn"}
}

func snapshotToolCall(name string, input map[string]any) llm.Response {
	raw, _ := json.Marshal(input)
	return llm.Response{
		StopReason: "tool_use",
		ToolCalls:  []llm.ToolCall{{ID: "call-1", Name: name, Input: raw}},
	}
}

// oneToolThenAnswer calls one tool on the first request and answers on every
// later one, so a snapshot covers a tool result going back to the model.
func oneToolThenAnswer(name string, input map[string]any) func(int, llm.Request) llm.Response {
	return func(n int, _ llm.Request) llm.Response {
		if n == 1 {
			return snapshotToolCall(name, input)
		}
		return answer("done")
	}
}

type snapshotCase struct {
	name   string
	c      Case
	script func(int, llm.Request) llm.Response
}

func snapshotCases() []snapshotCase {
	setup := Setup{
		KV:    map[string]string{"user/colour": "The user's favourite colour is teal."},
		Files: map[string]string{"notes/plan.md": "# Plan\n\nShip the boundary.\n"},
	}
	return []snapshotCase{
		{
			name: "orchestrator",
			c:    Case{Setup: setup, Prompts: []string{"What is my favourite colour? Save the answer under user/answer."}},
			script: oneToolThenAnswer("memory_set",
				map[string]any{"key": "user/answer", "value": "teal"}),
		},
		{
			name:   "orchestrator-interactive",
			c:      Case{Setup: setup, Session: Session{Interactive: true}, Prompts: []string{"Read notes/plan.md."}},
			script: oneToolThenAnswer("read_file", map[string]any{"path": "notes/plan.md"}),
		},
		{
			name: "orchestrator-delegates",
			c:    Case{Setup: setup, Prompts: []string{"Have a sub-agent list the workspace files."}},
			script: func(n int, req llm.Request) llm.Response {
				switch n {
				case 1:
					return snapshotToolCall("run_agent", map[string]any{"task": "List the workspace files.", "role": "executor"})
				default:
					return answer("done")
				}
			},
		},
		{
			name:   "executor",
			c:      Case{Setup: setup, Session: Session{Role: "executor"}, Prompts: []string{"List the workspace files."}},
			script: oneToolThenAnswer("list_files", map[string]any{}),
		},
		{
			name:   "reflection",
			c:      Case{Setup: setup, Session: Session{Role: "reflection"}, Prompts: []string{"Reflect on recent work."}},
			script: func(int, llm.Request) llm.Response { return answer("reflected") },
		},
		{
			name:   "pursue",
			c:      Case{Setup: setup, Session: Session{Role: "pursue"}, Prompts: []string{"Pursue: tidy the notes."}},
			script: func(int, llm.Request) llm.Response { return answer("pursued") },
		},
	}
}

func TestTurnSnapshots(t *testing.T) {
	for _, sc := range snapshotCases() {
		t.Run(sc.name, func(t *testing.T) {
			p := &snapshotProvider{script: sc.script}
			h := &Harness{Embedder: keyword.New(), BaseDir: t.TempDir()}
			c := sc.c
			c.ID = "snapshot-" + sc.name
			c.TimeoutSecs = 60

			res, err := h.Run(context.Background(), &c, p)
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			defer res.Close()

			checkSnapshot(t, sc.name, renderSnapshot(t, p.calls, res))
		})
	}
}

// checkSnapshot compares got with testdata/snapshots/<name>.json, or rewrites
// that file under -update.
func checkSnapshot(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", "snapshots", name+".json")
	if *updateSnapshots {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read snapshot (record with -update): %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("%s differs at line %s; review with -update and git diff", path, firstDiff(string(want), string(got)))
	}
}

// snapshotRequest is what a request carries to the model; callbacks are left out.
type snapshotRequest struct {
	System    string        `json:"system"`
	MaxTokens int           `json:"max_tokens"`
	Think     *bool         `json:"think,omitempty"`
	Messages  []llm.Message `json:"messages"`
	Tools     []llm.ToolDef `json:"tools"`
}

// snapshotEvent is a journal entry without its sequence number, span ids and
// timestamp, which differ between runs.
type snapshotEvent struct {
	Turn    int             `json:"turn"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

type snapshot struct {
	Requests []snapshotRequest `json:"requests"`
	Journal  []snapshotEvent   `json:"journal"`
}

// Values that differ between runs of the same code: ids, temporary paths, clock
// readings and durations. Each becomes a stable placeholder.
var (
	uuidPattern = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	timePattern = regexp.MustCompile(`\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}(:\d{2}(\.\d+)?)?(Z|[+-]\d{2}:?\d{2}| UTC)?`)
	datePattern = regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}\b`)
	dayPattern  = regexp.MustCompile(`\b(Monday|Tuesday|Wednesday|Thursday|Friday|Saturday|Sunday)\b`)
	msPattern   = regexp.MustCompile(`"duration_ms": \d+`)
)

func renderSnapshot(t *testing.T, calls []llm.Request, res *RunResult) []byte {
	t.Helper()
	out := make([]snapshotRequest, 0, len(calls))
	for _, req := range calls {
		out = append(out, snapshotRequest{
			System:    req.System,
			MaxTokens: req.MaxTokens,
			Think:     req.Think,
			Messages:  req.Messages,
			Tools:     req.Tools,
		})
	}
	journal := make([]snapshotEvent, 0, len(res.Events))
	for _, ev := range res.Events {
		journal = append(journal, snapshotEvent{Turn: ev.Turn, Type: ev.Type, Payload: ev.Payload})
	}
	raw, err := json.MarshalIndent(snapshot{Requests: out, Journal: journal}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	s = strings.ReplaceAll(s, res.Workspace, "<workspace>")
	s = uuidPattern.ReplaceAllString(s, "<uuid>")
	s = timePattern.ReplaceAllString(s, "<time>")
	s = datePattern.ReplaceAllString(s, "<date>")
	s = dayPattern.ReplaceAllString(s, "<day>")
	s = msPattern.ReplaceAllString(s, `"duration_ms": 0`)
	return []byte(s + "\n")
}

// firstDiff names the first line where two snapshots differ, with both versions.
func firstDiff(want, got string) string {
	w, g := strings.Split(want, "\n"), strings.Split(got, "\n")
	for i := 0; i < len(w) || i < len(g); i++ {
		var wl, gl string
		if i < len(w) {
			wl = w[i]
		}
		if i < len(g) {
			gl = g[i]
		}
		if wl != gl {
			return fmt.Sprintf("%d:\n  want %q\n  got  %q", i+1, wl, gl)
		}
	}
	return "(none)"
}
