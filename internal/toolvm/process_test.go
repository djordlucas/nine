package toolvm

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeProcess is a ProcessHandler fed by the test: triggers arrive on a
// channel, turns answer "reply:<text>" (or spend the budget on "spend"), and
// reports are recorded.
type fakeProcess struct {
	triggers chan Trigger
	mu       sync.Mutex
	reports  []string
}

func newFakeProcess() *fakeProcess { return &fakeProcess{triggers: make(chan Trigger, 8)} }

func (f *fakeProcess) Next(ctx context.Context) (Trigger, error) {
	select {
	case tr := <-f.triggers:
		return tr, nil
	case <-ctx.Done():
		return Trigger{}, ErrStopped
	}
}

func (f *fakeProcess) Turn(_ context.Context, text string) (string, error) {
	if text == "spend" {
		return "", ErrBudget
	}
	return "reply:" + text, nil
}

func (f *fakeProcess) Report(_ context.Context, text string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reports = append(f.reports, text)
	return true, nil
}

func (f *fakeProcess) reported() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.reports...)
}

func liveHost(t *testing.T, cfg Config, tools ...Generated) *Host {
	t.Helper()
	if cfg.Timeout == 0 {
		cfg.Timeout = 2 * time.Second
	}
	h, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { h.Close(context.Background()) }) //nolint:errcheck
	h.SetAgentConfig(AgentConfig{Enabled: true})
	h.LoadGenerated(context.Background(), tools, nil)
	for _, g := range tools {
		if h.Get(g.Name) == nil {
			t.Fatalf("tool %q did not load: %+v", g.Name, h.Status())
		}
	}
	return h
}

func waitLive(t *testing.T, l *Live) (Output, error) {
	t.Helper()
	select {
	case <-l.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("live process did not end")
	}
	return l.Wait()
}

// A live process survives across triggers — longer than the call timeout —
// waiting in next(), asking the model with turn(), and reporting.
func TestLiveProcessRunsAcrossTriggers(t *testing.T) {
	h := liveHost(t, Config{Timeout: 500 * time.Millisecond}, Generated{Name: "relay", Source: `
import { next, turn, report } from "nine:process";
export default () => {
  for (let i = 0; i < 3; i++) {
    const trigger = next();
    report(turn(trigger.text));
  }
  return "done";
};`})
	f := newFakeProcess()
	l, err := h.StartLive(context.Background(), "relay", json.RawMessage(`{}`), f)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"a", "b", "c"} {
		// Spaced past the call timeout: a live instance has none.
		time.Sleep(300 * time.Millisecond)
		f.triggers <- Trigger{Kind: "message", Text: text, At: time.Now()}
	}
	out, err := waitLive(t, l)
	if err != nil {
		t.Fatalf("live process failed: %v", err)
	}
	if out.Text != "done" {
		t.Errorf("result = %q, want done", out.Text)
	}
	if got := strings.Join(f.reported(), ","); got != "reply:a,reply:b,reply:c" {
		t.Errorf("reports = %q", got)
	}
}

// Stop ends a process blocked in next(), and frees its slot in the live pool.
func TestStoppingALiveProcessEndsItsPendingNext(t *testing.T) {
	src := `
import { next } from "nine:process";
export default () => { for (;;) next(); };`
	h := liveHost(t, Config{MaxLive: 1}, Generated{Name: "waiter", Source: src})

	l, err := h.StartLive(context.Background(), "waiter", json.RawMessage(`{}`), newFakeProcess())
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	l.Stop()
	if _, err := waitLive(t, l); !errors.Is(err, ErrStopped) {
		t.Errorf("stopped process ended with %v, want ErrStopped", err)
	}

	again, err := h.StartLive(context.Background(), "waiter", json.RawMessage(`{}`), newFakeProcess())
	if err != nil {
		t.Fatalf("the stopped process's slot was not freed: %v", err)
	}
	again.Stop()
	waitLive(t, again) //nolint:errcheck
}

// The live pool is bounded, and apart from the call pool: a full live pool
// refuses another process, and ordinary calls still run.
func TestLivePoolIsBoundedAndSeparate(t *testing.T) {
	h := liveHost(t, Config{MaxLive: 1, MaxConcurrent: 1},
		Generated{Name: "waiter", Source: `
import { next } from "nine:process";
export default () => { for (;;) next(); };`},
		Generated{Name: "echo", Source: `export default (a) => a.v;`})

	l, err := h.StartLive(context.Background(), "waiter", json.RawMessage(`{}`), newFakeProcess())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { l.Stop(); waitLive(t, l) }() //nolint:errcheck

	if _, err := h.StartLive(context.Background(), "waiter", json.RawMessage(`{}`), newFakeProcess()); err == nil ||
		!strings.Contains(err.Error(), "already running") {
		t.Errorf("second live process: err = %v, want the pool refusing", err)
	}
	if out := call(t, h, "echo", `{"v":"still here"}`); out != "still here" {
		t.Errorf("ordinary call while a process waits = %q", out)
	}
}

// A tool the model calls cannot block in next(): nine:process refuses it.
func TestNineProcessRefusesAnOrdinaryCall(t *testing.T) {
	h := liveHost(t, Config{}, Generated{Name: "sneaky", Source: `
import { next } from "nine:process";
export default () => next();`})
	_, err := h.Call(context.Background(), "sneaky", json.RawMessage(`{}`))
	if err == nil || !strings.Contains(err.Error(), "live processes") {
		t.Errorf("ordinary call of next(): err = %v, want a refusal", err)
	}
}

// A spent budget reaches the process as a catchable error with a code.
func TestLiveTurnBudgetErrorIsCatchable(t *testing.T) {
	h := liveHost(t, Config{}, Generated{Name: "careful", Source: `
import { next, turn, report } from "nine:process";
export default () => {
  next();
  try { turn("spend"); } catch (e) { report(e.code); }
  return "ok";
};`})
	f := newFakeProcess()
	l, err := h.StartLive(context.Background(), "careful", json.RawMessage(`{}`), f)
	if err != nil {
		t.Fatal(err)
	}
	f.triggers <- Trigger{Kind: "clock", At: time.Now()}
	if _, err := waitLive(t, l); err != nil {
		t.Fatal(err)
	}
	if got := f.reported(); len(got) != 1 || got[0] != "E_BUDGET" {
		t.Errorf("reports = %v, want [E_BUDGET]", got)
	}
}

// The work budget bounds the work per trigger: next() refills it, so a process
// doing modest work on each of many triggers is never stopped by the sum, while
// the same work without a trigger between still meets the budget.
func TestLiveWorkBudgetIsPerTrigger(t *testing.T) {
	const burn = `
function burn() { let x = 0; for (let i = 0; i < 400000; i++) x += i; return x; }`
	h := liveHost(t, Config{Timeout: 30 * time.Second, MaxOps: 2_000_000},
		Generated{Name: "steady", Source: `
import { next } from "nine:process";` + burn + `
export default () => { for (let i = 0; i < 6; i++) { next(); burn(); } return "ok"; };`},
		Generated{Name: "greedy", Source: burn + `
export default () => { for (let i = 0; i < 6; i++) burn(); return "ok"; };`})

	if _, err := h.Call(context.Background(), "greedy", json.RawMessage(`{}`)); err == nil ||
		!strings.Contains(err.Error(), "work budget") {
		t.Fatalf("calibration: six burns in one call should exceed the budget, got %v", err)
	}

	f := newFakeProcess()
	l, err := h.StartLive(context.Background(), "steady", json.RawMessage(`{}`), f)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		f.triggers <- Trigger{Kind: "clock", At: time.Now()}
	}
	if _, err := waitLive(t, l); err != nil {
		t.Errorf("six burns across six triggers: %v, want each trigger to refill the budget", err)
	}
}
