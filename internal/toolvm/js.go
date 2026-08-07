package toolvm

import (
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"sync"
)

// The interpreter and its harness are compiled into the binary, so the binary
// always matches the JS environment of its version — the same property docs/ and
// spec/ already have.
//
// quickjs/qjs.wasm is built by quickjs/build.sh from pinned tags and committed;
// `make build` needs no wasi-sdk and the runtime image gains no toolchain
// (docs/sandboxed-tools.md §10.1).
//
//go:embed quickjs/qjs.wasm quickjs/harness.js
var quickjsFS embed.FS

// toolModuleSpecifier is what harness.js imports to reach the author's code. It
// lives in the reserved `nine:` namespace so it cannot collide with a future
// stdlib module, and tool code cannot import it usefully — the host only ever
// binds it to the *calling* tool's own source.
const toolModuleSpecifier = "nine:tool"

func quickJSBlob() []byte {
	b, err := quickjsFS.ReadFile("quickjs/qjs.wasm")
	if err != nil {
		// Unreachable: go:embed fails the build if the file is absent.
		panic("toolvm: quickjs blob missing from binary: " + err.Error())
	}
	return b
}

var (
	harnessOnce sync.Once
	harnessSrc  string
)

func harness() string {
	harnessOnce.Do(func() {
		b, err := quickjsFS.ReadFile("quickjs/harness.js")
		if err != nil {
			panic("toolvm: harness missing from binary: " + err.Error())
		}
		harnessSrc = string(b)
	})
	return harnessSrc
}

// envelope is the input a `js` tool's module receives. It is the tool's "args"
// as far as the ABI is concerned — the QuickJS blob is an ordinary wasm tool
// whose arguments happen to describe a JavaScript program.
type envelope struct {
	Harness string            `json:"harness"`
	Modules map[string]string `json:"modules"`
	Args    json.RawMessage   `json:"args"`
}

// input builds the bytes the host writes into guest memory for one call.
//
// For a `wasm` tool that is just the model's arguments: the module is the tool.
// For a `js` tool it is the envelope above, and building it here is where §4.3's
// rule is enforced — the module map *is* the allowlist, resolved host-side,
// before instantiation. The guest's resolver serves only from this map, so a
// relative import, an absolute path, a URL, and a dynamic import() of anything
// absent all fail identically.
//
// For a developer tool the map holds only the tool's own source. That is not an
// omission: a developer pre-bundles their dependencies at development time, so
// by the time Nine loads the file it has no imports left (§4.2). Nine has no
// package manager, no lockfile, and no network at load time, and adding any of
// those would undo docs/self-modification.md exactly as a compiler would. The
// curated `nine:*` standard library exists for the *generated* tier, which does
// not yet exist here.
func (t *Tool) input(args json.RawMessage) ([]byte, error) {
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	if !json.Valid(args) {
		return nil, fmt.Errorf("tool %q: arguments are not valid JSON", t.Name)
	}

	if t.Kind == KindWasm {
		return args, nil
	}

	// Copied rather than aliased, and the tool's own source written last, so no
	// allowlist entry can shadow the entry point.
	modules := make(map[string]string, len(t.imports)+1)
	for k, v := range t.imports {
		modules[k] = v
	}
	modules[toolModuleSpecifier] = t.source

	return json.Marshal(envelope{Harness: harness(), Modules: modules, Args: args})
}

// Imports returns the module specifiers this tool may import, sorted — for
// `nine tools show`, so a tool's import surface is inspectable rather than
// folklore. Empty for a developer tool.
func (t *Tool) Imports() []string {
	out := make([]string, 0, len(t.imports))
	for k := range t.imports {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// lookupEnv reads one environment key. Indirected so tests can exercise the env
// capability without mutating the process environment.
var lookupEnv = os.LookupEnv

func sortToolsByName(ts []*Tool) {
	sort.Slice(ts, func(i, j int) bool { return ts[i].Name < ts[j].Name })
}
