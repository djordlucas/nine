package toolvm

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
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

	sawWASI, sawNine := false, false
	for _, fn := range mod.ImportedFunctions() {
		module, name, _ := fn.Import()
		switch module {
		case "wasi_snapshot_preview1":
			sawWASI = true
		case hostModule:
			sawNine = true
			// The whole host surface, and it is meant to stay this short. `log`
			// is granted to every tool; `http` is checked per call against the
			// tool's grant (host.hostHTTP); `caps` only describes a grant and
			// confers nothing (host.hostCaps). Anything else appearing here is
			// reach the capability table does not describe.
			//
			// Note what is NOT here: the filesystem and the environment. Those
			// reach a `js` tool through libc and WASI — pre-opens wazero enforces
			// itself — precisely so that containment never becomes a check of ours
			// in a host function (docs/rich-js-tools.md §6.4).
			if name != "log" && name != "http" && name != "caps" {
				t.Errorf("blob imports %s.%s; the host module is only log, http, and caps", module, name)
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

	// _initialize is the reactor entry point wazero calls itself.
	allowed := map[string]bool{exportAlloc: true, exportRun: true, "_initialize": true}
	for name := range mod.ExportedFunctions() {
		if !allowed[name] {
			t.Errorf("blob exports an unexpected function: %s", name)
		}
	}
	for _, name := range []string{exportAlloc, exportRun} {
		if _, ok := mod.ExportedFunctions()[name]; !ok {
			t.Errorf("blob does not export %q", name)
		}
	}
}
