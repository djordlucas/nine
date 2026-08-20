package runtime

import (
	"encoding/json"
	"testing"
	"time"

	"nine/internal/memory"
)

func mustConfig(t *testing.T, sc aspectConfig) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(sc)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestStageNextWakeInterval(t *testing.T) {
	cfg := mustConfig(t, aspectConfig{IdleIntervalSeconds: 300})
	now := time.Now()
	// Fired 60s ago ⇒ 240s remaining on a 300s interval.
	remaining, ok := aspectNextWake(cfg, now.Add(-60*time.Second), now)
	if !ok {
		t.Fatal("interval stage should be scheduled")
	}
	if remaining < 239*time.Second || remaining > 241*time.Second {
		t.Errorf("remaining = %v, want ~240s", remaining)
	}
	// Overdue ⇒ clamped to 0 (due now).
	remaining, _ = aspectNextWake(cfg, now.Add(-10*time.Minute), now)
	if remaining != 0 {
		t.Errorf("overdue interval remaining = %v, want 0", remaining)
	}
}

func TestStageNextWakeCron(t *testing.T) {
	cfg := mustConfig(t, aspectConfig{Schedule: "0 9 * * 1-5"})
	// Monday 08:00 UTC ⇒ next weekday-9am is the same day at 09:00 (1h away).
	now := time.Date(2026, 7, 6, 8, 0, 0, 0, time.UTC)
	remaining, ok := aspectNextWake(cfg, now, now)
	if !ok {
		t.Fatal("cron stage should be scheduled")
	}
	if remaining != time.Hour {
		t.Errorf("remaining = %v, want 1h", remaining)
	}

	// An unparseable schedule is not schedulable (and must not panic).
	bad := mustConfig(t, aspectConfig{Schedule: "not a cron"})
	if _, ok := aspectNextWake(bad, now, now); ok {
		t.Error("malformed cron should not be schedulable")
	}
}

func TestPlanNeedsResumeCron(t *testing.T) {
	plan := memory.SessionPlan{
		Status: "active",
		Aspects: []memory.SessionAspect{{
			Name:   "pursue",
			Kind:   "pursue",
			Status: "active",
			Config: mustConfig(t, aspectConfig{Schedule: "0 9 * * 1-5", Role: "monitor"}),
		}},
	}
	if !planNeedsResume(plan) {
		t.Error("a cron-scheduled active pursue plan must be resumed at startup")
	}
}

func TestNewStandingPursuePlanCron(t *testing.T) {
	plan, err := newStandingPursuePlan("sec-watch", "monitor", false, 0, "0 9 * * 1-5", nil)
	if err != nil {
		t.Fatal(err)
	}
	var sc aspectConfig
	if err := json.Unmarshal(plan.Aspects[0].Config, &sc); err != nil {
		t.Fatal(err)
	}
	if sc.Schedule != "0 9 * * 1-5" {
		t.Errorf("schedule = %q, want the cron expr", sc.Schedule)
	}
	if sc.IdleIntervalSeconds != 0 {
		t.Errorf("idle interval = %d, want 0 for a cron-scheduled agent", sc.IdleIntervalSeconds)
	}
	if got := roleNameForPlan(&sessionPlanState{plan: plan}); got != "monitor" {
		t.Errorf("roleNameForPlan = %q, want monitor", got)
	}
}
