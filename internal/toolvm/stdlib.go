package toolvm

import (
	"embed"
	"strings"
	"sync"
)

// The `nine:*` standard library (docs/sandboxed-tools.md §4.2): a small, pinned,
// vendored set of pure-ES2023 modules a generated tool may import without any
// resolver or network. Authored in-house rather than pulled from npm, so each is
// known to run under the trimmed QuickJS blob (no std/os; R-TVM.9) and carries no
// transitive surface. The files are embedded, so the binary always matches the JS
// environment of its version — the property docs/ and the blob already have.
//
//go:embed stdlib/*.js
var stdlibFS embed.FS

// stdlibSpecifiers maps each `nine:` import specifier to the file that backs it.
// Adding a module is a two-line change here plus the file; the reserved `nine:`
// namespace (see toolModuleSpecifier) keeps these from colliding with a tool's
// own entry point.
var stdlibSpecifiers = map[string]string{
	"nine:csv":  "stdlib/csv.js",
	"nine:date": "stdlib/date.js",
	"nine:diff": "stdlib/diff.js",
	// html is text extraction, not a DOM: a tokenizer that flattens a page and
	// pulls out elements by class. Enough for the two things Nine does with
	// HTML, and honest about not being a parser (see the module's own header).
	"nine:html": "stdlib/html.js",
	// fs and env are capability-gated rather than pure, which makes them
	// different in kind from the three above. They still belong here: importing
	// one grants nothing, the modules are equally embedded and dependency-free,
	// and a tool with no grant gets a sentence saying so rather than reach.
	"nine:fs":  "stdlib/fs.js",
	"nine:env": "stdlib/env.js",
	// state is capability-gated like fs and env, and is the one module here that
	// can outlive the call. Importing it still grants nothing: the store is the
	// host's, the namespace is this tool's, and a tool with no grant gets a
	// sentence saying so.
	"nine:state": "stdlib/state.js",
	// job is neither pure nor capability-gated: it is how a tool declared
	// `resumable` ends a call with "ask me again". Importing it grants nothing —
	// a tool whose manifest does not say resumable has its envelope refused.
	"nine:job": "stdlib/job.js",
	// process is how a live process drives its session: wait for the next
	// trigger, run a model turn, report to a pipe. Importing it grants nothing —
	// the host refuses every call unless the tool was started as a live process.
	"nine:process": "stdlib/process.js",
}

var (
	stdlibOnce  sync.Once
	stdlibCache map[string]string
)

// stdlibModules returns the `nine:*` module map (specifier -> source), read once
// from the embedded files. The map is the generated tier's import allowlist: a
// tool may import any key and nothing else, resolved host-side before the call
// (§4.3). The returned map is shared and MUST NOT be mutated — input() copies it
// per call.
func stdlibModules() map[string]string {
	stdlibOnce.Do(func() {
		stdlibCache = make(map[string]string, len(stdlibSpecifiers))
		for spec, path := range stdlibSpecifiers {
			b, err := stdlibFS.ReadFile(path)
			if err != nil {
				// Unreachable: go:embed fails the build if a listed file is absent.
				panic("toolvm: stdlib module missing from binary: " + path + ": " + err.Error())
			}
			stdlibCache[spec] = string(b)
		}
	})
	return stdlibCache
}

// reachableStdlib is the subset of the `nine:*` library a given source can name,
// and it is what a call ships in place of the whole library.
//
// The module map crosses the ABI as JSON on every call, so a module the tool
// never imports is not free: the eight together are 30 KB of text the host
// marshals and the guest JSON-parses before a line of the tool runs, which
// measures at about 1 ms of a 7 ms call — paid in full by `time`, which imports
// none of them. Narrowing the map is the same allowlist minus the entries this
// source has no way to reach, so no tool loses an import it could have used.
//
// The test is whether the specifier appears in the source at all, not whether it
// appears in an import statement. `await import("nine:csv")` is a legitimate way
// to reach a module (§4.3 resolves dynamic imports from the same map), and a scan
// that only understood static imports would break it. Over-inclusion is the safe
// direction and the only cost is the byte count this is trying to reduce — a
// mention in a comment ships the module, which is fine.
//
// One shape a substring scan cannot see is a specifier the source never spells:
// `import("nine:" + kind)`. That falls back to the whole library rather than to a
// resolver failure the author would have no way to read.
func reachableStdlib(source string) map[string]string {
	all := stdlibModules()
	if hasComputedImport(source) {
		return all
	}

	out := make(map[string]string, 4)
	// A worklist rather than one pass: a stdlib module may import another, and a
	// tool that reaches `nine:a` must get whatever `nine:a` itself imports. None
	// do today, which is exactly why this is worth writing down — the next one
	// will not come with a reminder.
	pending := []string{source}
	for len(pending) > 0 {
		src := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		for spec, mod := range all {
			if _, seen := out[spec]; seen {
				continue
			}
			if strings.Contains(src, spec) {
				out[spec] = mod
				pending = append(pending, mod)
			}
		}
	}
	return out
}

// hasComputedImport reports whether the source calls import() on anything but a
// complete string literal. Such a call can name a module this package cannot
// predict, so its presence turns the narrowing above off for that tool.
//
// "Complete" is the whole point, and the reason this is not a one-line check for
// a leading quote: `import("nine:" + kind)` opens with a quote and is computed
// all the same, and reading it as a literal ships nothing the tool can use. So
// the argument counts as literal only if the closing quote is followed by the
// closing parenthesis — anything else between them is an expression.
func hasComputedImport(source string) bool {
	const kw = "import("
	for i := 0; ; {
		j := strings.Index(source[i:], kw)
		if j < 0 {
			return false
		}
		i += j + len(kw)
		rest := strings.TrimLeft(source[i:], " \t\r\n")
		if rest == "" {
			return true
		}
		quote := rest[0]
		if quote != '"' && quote != '\'' && quote != '`' {
			return true
		}
		end := closingQuote(rest[1:], quote)
		if end < 0 {
			return true
		}
		if tail := strings.TrimLeft(rest[1+end+1:], " \t\r\n"); tail == "" || tail[0] != ')' {
			return true
		}
	}
}

// closingQuote returns the index in s of the next unescaped quote, or -1. A
// template literal containing a substitution never reaches its close as far as
// this is concerned, which is the conservative answer: it is computed.
func closingQuote(s string, quote byte) int {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '$':
			if quote == '`' && i+1 < len(s) && s[i+1] == '{' {
				return -1
			}
		case quote:
			return i
		}
	}
	return -1
}
