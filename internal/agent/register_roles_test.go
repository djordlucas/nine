package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

type fakeRoleCatalog struct{ roles []RoleInfo }

func (f fakeRoleCatalog) Roles() []RoleInfo { return f.roles }

func TestRoleListAndSearch(t *testing.T) {
	cat := fakeRoleCatalog{roles: []RoleInfo{
		{Name: "orchestrator", Description: "plans and dispatches", Tools: []string{"board_post", "run_agent"}},
		{Name: "software-dev", Description: "implement code", Tools: []string{"shell", "write_file"}},
		{Name: "monitor", Description: "read-only watch", AllTools: false, Tools: []string{"read_file"}},
	}}
	d := New()
	RegisterRoleTools(d, cat, []string{"role_list", "role_search"})

	out, err := d.Dispatch(context.Background(), "role_list", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("role_list: %v", err)
	}
	var all []RoleInfo
	if err := json.Unmarshal([]byte(out.Output), &all); err != nil || len(all) != 3 {
		t.Fatalf("role_list: %s err=%v", out.Output, err)
	}

	out, err = d.Dispatch(context.Background(), "role_search", json.RawMessage(`{"query":"shell"}`))
	if err != nil {
		t.Fatalf("role_search: %v", err)
	}
	if !strings.Contains(out.Output, "software-dev") || strings.Contains(out.Output, "monitor") {
		t.Fatalf("role_search shell: %s", out.Output)
	}

	// A role that does not grant the tools holds no handlers.
	d2 := New()
	RegisterRoleTools(d2, cat, nil)
	if _, err := d2.Dispatch(context.Background(), "role_list", json.RawMessage(`{}`)); err == nil {
		t.Fatal("role_list without grant: want error")
	}
}
