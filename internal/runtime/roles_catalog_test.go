package runtime

import (
	"testing"

	"nine/internal/agent"
)

func TestRoleInfosOrchestratorNarrowed(t *testing.T) {
	reg := NewRoleRegistry(nil, "")
	infos := reg.RoleInfos()
	byName := map[string]agent.RoleInfo{}
	for _, ri := range infos {
		byName[ri.Name] = ri
	}
	orch, ok := byName[OrchestratorRole]
	if !ok {
		t.Fatalf("orchestrator role missing from %v", infos)
	}
	if orch.AllTools {
		t.Fatal("orchestrator must not have all tools (adr/orchestrator-board.md §2.2)")
	}
	for _, want := range []string{"board_post", "board_list", "role_search", "role_list", "memory_set", "agent_stop"} {
		found := false
		for _, got := range orch.Tools {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("orchestrator allowlist missing %s: %v", want, orch.Tools)
		}
	}
	for _, banned := range []string{"shell", "write_file", "read_file"} {
		for _, got := range orch.Tools {
			if got == banned {
				t.Errorf("orchestrator allowlist must not contain %s", banned)
			}
		}
	}
	// The default leaf keeps the full surface.
	if !byName[DefaultLeafRoleName].AllTools {
		t.Error("executor must keep all tools")
	}
}
