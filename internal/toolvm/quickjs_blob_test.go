package toolvm

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/tetratelabs/wazero"
)

// A binary artifact in the tree is only acceptable if changing it is loud. CI
// runs `make quickjs-verify`; this makes the same check part of `go test`, so a
// blob that drifts from its recorded hash cannot reach main through a path that
// skipped the Makefile.
func TestQuickJSBlobMatchesItsRecordedHash(t *testing.T) {
	sum := sha256.Sum256(quickJSBlob())
	got := hex.EncodeToString(sum[:])

	f, err := os.Open("quickjs/qjs.wasm.sha256")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() //nolint:errcheck

	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		t.Fatal("qjs.wasm.sha256 is empty")
	}
	want, _, _ := strings.Cut(strings.TrimSpace(sc.Text()), " ")

	if got != want {
		t.Errorf("qjs.wasm hash = %s, recorded %s\n"+
			"The committed interpreter does not match its recorded hash. If you rebuilt it "+
			"deliberately (make quickjs-wasm), the new hash belongs in the same PR as the "+
			"blob, where a human reviews both.", got, want)
	}
}

// R-TVM.9, read off the module itself rather than through the guest: the blob
// must import nothing but WASI and our own single-function host module, and must
// export nothing but the ABI.
//
// A new import appearing here means the interpreter gained a way to reach outside
// that the capability table (R-TVM.5) does not describe — which is exactly the
// "a table that lies" failure §4.1 is about. A new *export* means the host gained
// an entry point nobody reviewed.
func TestQuickJSBlobImportsAndExportsAreClosed(t *testing.T) {
	ctx := context.Background()
	rt := wazero.NewRuntime(ctx)
	defer rt.Close(ctx) //nolint:errcheck

	mod, err := rt.CompileModule(ctx, quickJSBlob())
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	// Adding to this list is a deliberate act: each entry is reach a guest gains
	// that the capability table has to describe.
	hostFunctions := []string{"log", "http", "caps", "state"}

	sawWASI, sawNine := false, false
	for _, fn := range mod.ImportedFunctions() {
		module, name, _ := fn.Import()
		switch module {
		case "wasi_snapshot_preview1":
			sawWASI = true
		case hostModule:
			sawNine = true
			// The whole host surface, and it is meant to stay this short. `log`
			// is granted to every tool; `http` and `state` are checked per call
			// against the tool's grant (host.hostHTTP, host.hostState); `caps`
			// only describes a grant and confers nothing (host.hostCaps).
			// Anything else appearing here is reach the capability table does not
			// describe.
			//
			// Note what is NOT here: the filesystem and the environment. Those
			// reach a `js` tool through libc and WASI — pre-opens wazero enforces
			// itself — precisely so that containment never becomes a check of ours
			// in a host function (adr/rich-js-tools.md §6.4).
			//
			// `state` cannot follow them, and that is worth being explicit about:
			// there is no WASI facility for a scoped key/value store, and the
			// scoping is a primary key in Nine's own database rather than
			// something an operating system enforces. So it is a host function,
			// and Nine owns the bugs in it.
			if !slices.Contains(hostFunctions, name) {
				t.Errorf("blob imports %s.%s, which is not in the host module's surface %v",
					module, name, hostFunctions)
			}
		default:
			t.Errorf("blob imports an unexpected module: %s.%s", module, name)
		}
	}
	if !sawWASI {
		t.Error("blob does not import wasi_snapshot_preview1; is this the right artifact?")
	}
	if !sawNine {
		t.Errorf("blob does not import the %q host module", hostModule)
	}

	// _initialize is the reactor entry point wazero calls itself. nine_harness
	// is the blob's own, not part of the tool ABI: it names the precompiled
	// harness the host has written into linear memory, and confers nothing — it
	// stores a pointer the host just chose, in an instance that is destroyed when
	// the call returns.
	allowed := map[string]bool{
		exportAlloc: true, exportRun: true, exportHarness: true, "_initialize": true,
	}
	for name := range mod.ExportedFunctions() {
		if !allowed[name] {
			t.Errorf("blob exports an unexpected function: %s", name)
		}
	}
	for _, name := range []string{exportAlloc, exportRun, exportHarness} {
		if _, ok := mod.ExportedFunctions()[name]; !ok {
			t.Errorf("blob does not export %q", name)
		}
	}
}
