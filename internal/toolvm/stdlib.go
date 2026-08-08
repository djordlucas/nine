package toolvm

import (
	"embed"
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
