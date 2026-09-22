package toolvm

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"testing"
)

// The daemon embeds quickjs/harness.bc, not quickjs/harness.js, so an edit to
// the JavaScript that skips `make harness-bc` ships a harness lagging its own
// source — and would do it silently, since the stale bytecode still loads and
// still runs. build.sh records the source hash; this is what makes the mismatch
// loud, the same way qjs.wasm.sha256 makes a stale blob loud.
func TestHarnessBytecodeMatchesItsSource(t *testing.T) {
	src, err := os.ReadFile("quickjs/harness.js")
	if err != nil {
		t.Fatalf("read harness.js: %v", err)
	}
	recorded, err := os.ReadFile("quickjs/harness.bc.sha256")
	if err != nil {
		t.Fatalf("read harness.bc.sha256: %v", err)
	}

	sum := sha256.Sum256(src)
	want := hex.EncodeToString(sum[:])
	if got := strings.Fields(string(recorded))[0]; got != want {
		t.Errorf("harness.js has changed since harness.bc was built\n"+
			"  recorded %s\n  actual   %s\nrun `make harness-bc`", got, want)
	}
}

// The embedded artifact has to be bytecode the interpreter will accept, not an
// empty file or a stray copy of the JavaScript — both of which would only show
// up at the first tool call.
func TestEmbeddedHarnessIsBytecode(t *testing.T) {
	bc := harness()
	if len(bc) == 0 {
		t.Fatal("no harness bytecode embedded")
	}
	// BC_VERSION is the first byte of a QuickJS object stream; source would
	// start with a comment or an import.
	if bc[0] == byte('/') || bc[0] == byte('i') {
		t.Errorf("harness.bc looks like JavaScript, not bytecode (starts %q)", bc[:8])
	}
}
