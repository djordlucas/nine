package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"nine/internal/memory"
	"nine/internal/replay"
)

// Track R — deterministic replay (docs/evals.md §4). A case's journal is recorded
// once from a live (or scripted) run and stored as a fixture; CI then re-executes
// it against a recorded provider/dispatcher and asserts the reproduced answers
// equal the recorded ones. This makes zero API/tool calls and guards the loop,
// dispatcher, and context assembly — the parts we own — not the model.

// ReplayDir returns the fixture directory for a case id under root
// (tests/evals/replay/<id>).
func ReplayDir(root, id string) string { return filepath.Join(root, id) }

// replayableEvents keeps only events belonging to a turn that has a turn_start,
// preserving order. A live journal carries turn-0 control-plane rows (supervisor
// events) that replay.FromEvents would otherwise treat as a spurious empty turn,
// making an extra LLM call the recording never scripted. Filtering to real turns
// keeps a fixture's replay deterministic.
func replayableEvents(events []memory.SessionEvent) []memory.SessionEvent {
	realTurn := map[int]bool{}
	for _, e := range events {
		if e.Type == "turn_start" {
			realTurn[e.Turn] = true
		}
	}
	out := make([]memory.SessionEvent, 0, len(events))
	for _, e := range events {
		if realTurn[e.Turn] {
			out = append(out, e)
		}
	}
	return out
}

// journalFile is the fixture filename holding a recorded session's journal.
const journalFile = "journal.json"

// RecordFixture writes res's journal to <dir>/journal.json, creating dir. It is
// how a Track-R fixture is captured: run the case live once, then persist the
// events the replay will re-execute. Only replayable turns are stored (see
// replayableEvents) so the fixture round-trips deterministically.
func RecordFixture(dir string, events []memory.SessionEvent) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	events = replayableEvents(events)
	b, err := json.MarshalIndent(events, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return os.WriteFile(filepath.Join(dir, journalFile), b, 0o644)
}

// LoadJournal reads a recorded journal fixture into events in seq order.
func LoadJournal(dir string) ([]memory.SessionEvent, error) {
	b, err := os.ReadFile(filepath.Join(dir, journalFile))
	if err != nil {
		return nil, err
	}
	var events []memory.SessionEvent
	if err := json.Unmarshal(b, &events); err != nil {
		return nil, fmt.Errorf("%s: %w", dir, err)
	}
	return events, nil
}

// ReplayResult is the outcome of re-executing a recorded fixture.
type ReplayResult struct {
	Reproduced []string // answers produced by the replay, per turn
	Recorded   []string // the recorded golden answers, per turn
	Divergence string   // "" when reproduced == recorded; else the first mismatch
}

// Pass reports whether the replay reproduced every recorded answer.
func (r ReplayResult) Pass() bool { return r.Divergence == "" }

// ReplayFixture re-executes the recorded journal at dir against a recorded
// provider/dispatcher (internal/replay) and compares the reproduced answers to
// the recorded ones. A mismatch means the loop/dispatcher/context code changed
// the recorded behavior — a real Track-R regression (or a fixture that must be
// deliberately re-recorded, docs/evals.md §4).
func ReplayFixture(ctx context.Context, dir string) (ReplayResult, error) {
	events, err := LoadJournal(dir)
	if err != nil {
		return ReplayResult{}, err
	}
	rec, err := replay.FromEvents(replayableEvents(events))
	if err != nil {
		return ReplayResult{}, fmt.Errorf("from events: %w", err)
	}
	reproduced, err := replay.Session(ctx, rec)
	if err != nil {
		return ReplayResult{}, fmt.Errorf("replay session: %w", err)
	}

	recorded := make([]string, len(rec.Turns))
	for i, t := range rec.Turns {
		recorded[i] = t.Result
	}

	res := ReplayResult{Reproduced: reproduced, Recorded: recorded}
	res.Divergence = firstDivergence(reproduced, recorded)
	return res, nil
}

// firstDivergence returns a description of the first turn whose reproduced answer
// differs from the recorded one (trimmed), or "" if all match. A differing turn
// count is itself a divergence.
func firstDivergence(reproduced, recorded []string) string {
	if len(reproduced) != len(recorded) {
		return fmt.Sprintf("turn count: reproduced %d, recorded %d", len(reproduced), len(recorded))
	}
	for i := range recorded {
		got := strings.TrimSpace(reproduced[i])
		want := strings.TrimSpace(recorded[i])
		if got != want {
			return fmt.Sprintf("turn %d: reproduced %q, recorded %q", i+1, got, want)
		}
	}
	return ""
}
