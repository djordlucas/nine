package runtime

import (
	"context"
	"strings"
	"testing"
	"time"

	"nine/internal/memory"
	"nine/internal/memory/memtest"
)

// A standing agent's [[agent.routine]] entries become processes attached to its
// session (adr/process-sessions.md §4): reflection beside pursuit runs in the
// agent's session, with its history, at its own cadence. The owner keeps the
// session's role.
func TestStandingRoutinesBecomeAttachedProcesses(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	d := New("", nil, nil)
	d.ConfigureProcesses(store, nil)

	if _, err := d.SpawnStandingSession(context.Background(), "sec-watch", "monitor", false, 0, "0 9 * * 1-5",
		[]RoutineDecl{{Kind: "idle-reflection", Interval: 30 * time.Minute}}); err != nil {
		t.Fatal(err)
	}
	procs, err := store.ProcessesOfSession("sec-watch")
	if err != nil || len(procs) != 2 {
		t.Fatalf("processes of the session = %+v, %v; want the owner and one attached", procs, err)
	}
	owner, attached := procs[0], procs[1]
	if !owner.Owner || owner.Tool != "pursue" || owner.Role != "monitor" || owner.Schedule != "0 9 * * 1-5" {
		t.Errorf("owner = %+v", owner)
	}
	if attached.Owner || attached.Tool != "reflect" || attached.IntervalSecs != 1800 ||
		attached.GoalID != "sec-watch" || attached.Mode != memory.ProcessLive {
		t.Errorf("attached = %+v", attached)
	}
}

// Without routines, a standing agent is its pursue process alone.
func TestStandingAgentWithoutRoutinesIsOneProcess(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	d := New("", nil, nil)
	d.ConfigureProcesses(store, nil)
	if _, err := d.SpawnStandingSession(context.Background(), "sec-watch", "monitor", false, time.Hour, "", nil); err != nil {
		t.Fatal(err)
	}
	if procs, _ := store.ProcessesOfSession("sec-watch"); len(procs) != 1 {
		t.Errorf("processes = %+v, want the pursue process alone", procs)
	}
}

func TestValidateRoutinesRejectsBadDeclarations(t *testing.T) {
	cases := []struct {
		name     string
		routines []RoutineDecl
		want     string
	}{
		{"no kind", []RoutineDecl{{Interval: time.Hour}}, "no kind"},
		{"unknown kind", []RoutineDecl{{Kind: "nonsense", Interval: time.Hour}}, "not a known routine kind"},
		{"duplicate pursue", []RoutineDecl{{Kind: "pursue", Interval: time.Hour}}, "duplicates the session's own pursue process"},
		{"no cadence", []RoutineDecl{{Kind: "idle-reflection"}}, "would never wake"},
		{"both cadences", []RoutineDecl{{Kind: "idle-reflection", Interval: time.Hour, Schedule: "0 9 * * *"}}, "mutually exclusive"},
		{"declared twice", []RoutineDecl{
			{Kind: "idle-reflection", Interval: time.Hour},
			{Kind: "idle-reflection", Interval: time.Minute},
		}, "declared twice"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateRoutineDecls(tc.routines)
			if err == nil {
				t.Fatalf("ValidateRoutineDecls accepted %+v", tc.routines)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestValidateRoutinesAcceptsGoodDeclarations(t *testing.T) {
	for _, a := range [][]RoutineDecl{
		nil,
		{{Kind: "idle-reflection", Interval: time.Hour}},
		{{Kind: "idle-reflection", Schedule: "0 9 * * 1-5"}},
	} {
		if err := ValidateRoutineDecls(a); err != nil {
			t.Errorf("ValidateRoutineDecls rejected %+v: %v", a, err)
		}
	}
}
