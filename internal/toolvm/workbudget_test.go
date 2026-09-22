package toolvm

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// budgetHost opens a host whose deadline is long enough that anything stopping a
// tool here is the work budget and not the clock.
func budgetHost(t *testing.T, maxOps int, perTool map[string]int) *Host {
	t.Helper()
	h, err := Open(context.Background(), Config{
		Timeout:       30 * time.Second,
		MaxOps:        maxOps,
		MaxOpsPerTool: perTool,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { h.Close(context.Background()) }) //nolint:errcheck
	h.SetAgentConfig(AgentConfig{Enabled: true})
	return h
}

func spin(t *testing.T, h *Host, name, source string) error {
	t.Helper()
	h.LoadGenerated(context.Background(), []Generated{{Name: name, Source: source}}, nil)
	if h.Get(name) == nil {
		t.Fatalf("tool %q did not load: %+v", name, h.Status())
	}
	_, err := h.Call(context.Background(), name, json.RawMessage(`{}`))
	return err
}

// The bound the deadline never was: a spinning tool is stopped by the work it
// has done, not by how long the machine took to do it.
func TestASpinningToolMeetsItsWorkBudget(t *testing.T) {
	h := budgetHost(t, 1_000_000, nil)

	start := time.Now()
	err := spin(t, h, "spinner", `export default () => { while (true) {} };`)
	if err == nil {
		t.Fatal("a tool that never returns completed")
	}
	if !strings.Contains(err.Error(), "work budget") {
		t.Errorf("error = %q, want the work budget named", err)
	}
	// The deadline is 30s; meeting the budget must be what stopped it.
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("took %s — that is the deadline, not the budget", elapsed)
	}

	var ce *CallError
	if !errors.As(err, &ce) {
		t.Fatalf("error is %T, want *CallError so a caller can read the code", err)
	}
	if ce.Code() != CodeWorkBudget {
		t.Errorf("code = %q, want %q", ce.Code(), CodeWorkBudget)
	}
	if retryable, stated := ce.Retryable(); !stated || retryable {
		t.Error("a spent budget must say it is not retryable; retrying spends it again")
	}
}

// The property that makes this a bound rather than a suggestion. QuickJS marks
// the interrupt uncatchable, so a tool cannot wrap its loop in try/catch and
// carry on — which is exactly what a tool trying to evade a budget would do.
func TestAToolCannotCatchItsWayPastTheBudget(t *testing.T) {
	for _, tc := range []struct{ name, src string }{
		{"try_catch", `export default () => { try { while (true) {} } catch (e) { return "swallowed"; } };`},
		{"catch_in_loop", `export default () => { for (;;) { try { while (true) {} } catch (e) {} } };`},
		{"promise_catch", `export default async () => {
			try { await (async () => { while (true) {} })(); } catch (e) { return "swallowed"; }
		};`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := budgetHost(t, 1_000_000, nil)
			err := spin(t, h, "evader", tc.src)
			if err == nil {
				t.Fatal("the tool swallowed the interrupt and returned")
			}
			if !strings.Contains(err.Error(), "work budget") {
				t.Errorf("error = %q, want the work budget named", err)
			}
		})
	}
}

// A budget nothing honest meets is the whole design: every shipped tool and the
// harness itself must run far inside it.
func TestOrdinaryWorkIsNowhereNearTheBudget(t *testing.T) {
	ctx := context.Background()
	h := budgetHost(t, 0, nil) // the default
	h.SetShippedWorkspace(ShippedWorkspace{Host: t.TempDir()})
	h.LoadShipped(ctx, nil)

	if out := call(t, h, "time", `{}`); out == "" {
		t.Error("time returned nothing under the default budget")
	}
	// Genuinely heavy, and still meant to pass: 20k rows through nine:csv.
	err := spin(t, h, "heavy", `
import { parse } from "nine:csv";
export default () => {
  let rows = "a,b\n";
  for (let i = 0; i < 20000; i++) rows += i + "," + (i * 2) + "\n";
  return String(parse(rows).length);
};`)
	if err != nil {
		t.Errorf("parsing 20k CSV rows met the default budget: %v", err)
	}
}

// One tool that legitimately grinds must not have to raise the budget for every
// other tool, which is the argument per-tool timeouts already won.
func TestAToolCanBeGivenItsOwnBudget(t *testing.T) {
	const src = `export default () => { let s = 0; for (let i = 0; i < 3e6; i++) s += i; return String(s); };`

	if err := spin(t, budgetHost(t, 1_000_000, nil), "grind", src); err == nil {
		t.Fatal("precondition: this workload should not fit in the global budget")
	}
	h := budgetHost(t, 1_000_000, map[string]int{"grind": 50_000_000})
	if err := spin(t, h, "grind", src); err != nil {
		t.Errorf("the per-tool override did not apply: %v", err)
	}
}

// Turning the budget off has to be distinguishable from leaving it unset, or an
// operator cannot get the old behavior back.
func TestANegativeBudgetDisablesMetering(t *testing.T) {
	h := budgetHost(t, -1, nil)
	h.LoadGenerated(context.Background(), []Generated{{Name: "quick",
		Source: `export default () => { let s = 0; for (let i = 0; i < 3e6; i++) s += i; return String(s); };`}}, nil)
	if h.Get("quick").checks != 0 {
		t.Errorf("checks = %d, want 0 (unmetered)", h.Get("quick").checks)
	}
	if _, err := h.Call(context.Background(), "quick", json.RawMessage(`{}`)); err != nil {
		t.Errorf("an unmetered tool was stopped: %v", err)
	}
}
