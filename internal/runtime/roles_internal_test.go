package runtime

import (
	"context"
	"testing"
	"time"

	"nine/internal/memory/memtest"
)

// A session a process drives runs under that process's role
// (adr/process-sessions.md): the pursue role for a goal session, a standing
// agent's declared work role, the reflection role for self-reflection. Any
// other session is a conversation.
func TestSessionRoleComesFromItsOwningProcess(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	d := New("", nil, nil)
	d.ConfigureProcesses(store, nil)
	ctx := context.Background()

	if _, err := d.SpawnGoalSession(ctx, "tidy"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.SpawnStandingSession(ctx, "sec-watch", "monitor", true, time.Hour, "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := d.SpawnStandingSession(ctx, "plain", "", false, time.Hour, "", nil); err != nil {
		t.Fatal(err)
	}
	if err := ReconcileSelfReflection(store, time.Hour); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		session string
		want    RoleParams
	}{
		{"tidy", RoleParams{Role: PursueRole, OwnsGoal: true}},
		{"sec-watch", RoleParams{Role: "monitor", OwnsGoal: true, Delegates: true}},
		{"plain", RoleParams{Role: PursueRole, OwnsGoal: true}},
		{SelfReflectionAgentID, RoleParams{Role: ReflectionRole}},
	}
	for _, tc := range cases {
		got, ok := d.processRole(tc.session)
		if !ok || got != tc.want {
			t.Errorf("%s: processRole = %+v, %v; want %+v", tc.session, got, ok, tc.want)
		}
	}
	if _, ok := d.processRole("a-conversation"); ok {
		t.Error("a conversation resolved to a process role")
	}
}
