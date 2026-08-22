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
	// Job is present only when this call is one of a long-running sequence. The
	// harness passes it to the tool as a second argument; an ordinary call omits
	// it and the tool sees undefined.
	Job *JobContext `json:"job,omitempty"`
}

// JobContext is what a resumable tool is told about the run it is part of.
//
// Cursor is whatever the tool itself returned last time — Nine persists it and
// never reads it. Call counts from 1, so a tool can tell its first call from a
// resumption without having to encode that into the cursor.
type JobContext struct {
	Cursor string `json:"cursor,omitempty"`
	Call   int    `json:"call"`
}

// inputForJob builds the bytes the host writes into guest memory for one call.
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
// inputForJob builds the guest input, optionally carrying a job context. A nil
// job is an ordinary call, which is almost all of them.
func (t *Tool) inputForJob(args json.RawMessage, job *JobContext) ([]byte, error) {
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	if !json.Valid(args) {
		return nil, fmt.Errorf("tool %q: arguments are not valid JSON", t.Name)
	}

	if t.Kind == KindWasm {
		// A wasm tool has no harness to hand a second argument to, so the job
		// context rides inside the JSON it already parses. Additive: a tool that
		// never opted into being resumable never sees the key.
		if job != nil {
			return mergeJobIntoArgs(args, job)
		}
		return args, nil
	}

	// Copied rather than aliased, and the tool's own source written last, so no
	// allowlist entry can shadow the entry point.
	modules := make(map[string]string, len(t.imports)+1)
	for k, v := range t.imports {
		modules[k] = v
	}
	modules[toolModuleSpecifier] = t.source

	return json.Marshal(envelope{Harness: harness(), Modules: modules, Args: args, Job: job})
}

// reservedJobKey is the argument name a resumable `wasm` tool receives its job
// context under. A `js` tool gets it as a second parameter and needs none of
// this; a wasm tool has no harness to hand one to.
const reservedJobKey = "nine_job"

// declaresReservedJobKey reports whether a JSON Schema declares reservedJobKey as
// a property. Checked at load for resumable wasm tools, so the key is reserved in
// fact and not only in a comment.
func declaresReservedJobKey(schema json.RawMessage) bool {
	if len(schema) == 0 {
		return false
	}
	var s struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(schema, &s); err != nil {
		return false
	}
	_, found := s.Properties[reservedJobKey]
	return found
}

// mergeJobIntoArgs adds the job context to a wasm tool's arguments under
// reservedJobKey, which compile() refuses to let a resumable wasm tool's own
// schema declare.
func mergeJobIntoArgs(args json.RawMessage, job *JobContext) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(args, &m); err != nil {
		return nil, fmt.Errorf("arguments must be a JSON object to carry a job cursor: %w", err)
	}
	if m == nil {
		m = map[string]json.RawMessage{}
	}
	raw, err := json.Marshal(job)
	if err != nil {
		return nil, err
	}
	m[reservedJobKey] = raw
	return json.Marshal(m)
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
