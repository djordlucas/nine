package integration_test

import (
	"os/exec"
	"strings"
	"testing"
)

// nineCLI runs `nine <args>` inside the container and returns its combined
// output, whatever the exit status: a refusal is an outcome these tests read.
func nineCLI(t *testing.T, args ...string) string {
	t.Helper()
	all := append([]string{"exec", containerID, "nine"}, args...)
	out, _ := exec.Command("docker", all...).CombinedOutput()
	s := strings.TrimSpace(string(out))
	t.Logf("nine %s →\n%s", strings.Join(args, " "), s)
	return s
}

// The operator's roster and control of processes, against the real daemon:
// the self-reflection process every default daemon runs is listed, stopped,
// shown stopped by the operator, started again, and an unknown id is refused
// by name (docs/processes.md).
func TestProcessOperatorCommands(t *testing.T) {
	const id = "self-reflection"

	if out := nineCLI(t, "process"); !strings.Contains(out, id) {
		t.Fatalf("the roster lacks %s", id)
	}
	if out := nineCLI(t, "process", "stop", id); !strings.Contains(out, "stopped") {
		t.Fatalf("stop did not report stopping %s", id)
	}
	if out := nineCLI(t, "process", "show", id); !strings.Contains(out, "stopped by operator") {
		t.Errorf("show does not say %s was stopped by the operator", id)
	}
	if out := nineCLI(t, "process", "start", id); !strings.Contains(out, "started") {
		t.Fatalf("start did not report starting %s", id)
	}
	if out := nineCLI(t, "process", "show", id); !strings.Contains(out, "running") {
		t.Errorf("show does not say %s is running after the start", id)
	}
	if out := nineCLI(t, "process", "show", "no-such-process"); !strings.Contains(out, "no such process") {
		t.Errorf("an unknown id was not refused by name")
	}
	// A send is taken only by a live process waiting for work, and refused
	// with a reason otherwise; either way the daemon answers.
	out := nineCLI(t, "process", "send", id, "reflect on the last hour")
	if !strings.Contains(out, "sent to") && !strings.Contains(out, "busy") && !strings.Contains(out, "not running yet") {
		t.Errorf("send gave neither a delivery nor a refusal")
	}
}

// The commands the roster replaced name their replacement.
func TestRetiredStandingCommandsNameTheirReplacement(t *testing.T) {
	for _, args := range [][]string{
		{"tools", "standing"},
		{"tool", "status", "x"},
		{"tool", "stop", "x"},
	} {
		if out := nineCLI(t, args...); !strings.Contains(out, "nine process") {
			t.Errorf("nine %s does not name `nine process`", strings.Join(args, " "))
		}
	}
}
