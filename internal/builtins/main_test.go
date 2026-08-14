package builtins_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"nine/internal/plugin"
)

// nineBin is a nine binary built once for the whole package. The built-ins are
// served out of it (`nine plugin serve <name>`), so exercising them end-to-end
// means spawning it — the test binary itself has no such subcommand, which is
// what SetBuiltinBinary below exists for.
var nineBin string

func TestMain(m *testing.M) {
	// os.Exit skips deferred calls, so the temp dir is removed explicitly on
	// every path out. It holds a full nine binary — leaking one per test run
	// would quietly fill $TMPDIR.
	tmp, err := os.MkdirTemp("", "nine-builtins-test-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "temp dir: %v\n", err)
		os.Exit(1)
	}

	nineBin = filepath.Join(tmp, "nine")
	cmd := exec.Command("go", "build", "-o", nineBin, "./cmd/nine")
	cmd.Dir = moduleRoot()
	if b, err := cmd.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build: %v\n%s\n", err, b)
		os.RemoveAll(tmp) //nolint:errcheck // best-effort
		os.Exit(1)
	}

	code := m.Run()
	os.RemoveAll(tmp) //nolint:errcheck // best-effort
	os.Exit(code)
}

func moduleRoot() string {
	dir, _ := os.Getwd()
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			panic("go.mod not found")
		}
		dir = parent
	}
}

// start launches one built-in plugin through the real manager — the same spawn,
// describe, and HTTP-over-Unix-socket path the daemon uses — and stops it when
// the test ends.
func start(t *testing.T, name string, extraEnv ...string) (*plugin.Plugin, *plugin.Manager) {
	t.Helper()
	m := plugin.NewManager("")
	m.SetBuiltinBinary(nineBin)
	p, err := m.StartBuiltin(name, extraEnv...)
	if err != nil {
		t.Fatalf("start %s: %v", name, err)
	}
	t.Cleanup(func() { m.Stop(p) }) //nolint:errcheck // test cleanup
	return p, m
}
