package toolvm

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// Kind is how a tool's wasm module is obtained.
type Kind string

const (
	// KindWasm is a module the developer built themselves, from Rust, TinyGo,
	// Zig, or C. Full speed, any language, no interpreter in the middle.
	KindWasm Kind = "wasm"
	// KindJS is JavaScript run by the pre-supplied QuickJS blob. Nothing is
	// compiled at install time; the module is the interpreter and the tool's
	// source is its input.
	KindJS Kind = "js"
)

// Manifest is a developer tool's sidecar TOML (`<name>.toml`).
//
// It is a gate, exactly as for user plugins: a `.js` or `.wasm` file with no
// manifest beside it is never loaded. Unlike a native plugin manifest it is also
// *authoritative* for the tool's shape — there is no process to ask
// plugin.describe, so the name, description, and input schema come from here.
type Manifest struct {
	// Name is the tool name the model calls. It shares one namespace with
	// built-ins and plugin tools, and collisions skip the whole tool (R-TVM.10).
	Name string `toml:"name"`

	// Kind is "js" or "wasm".
	Kind Kind `toml:"kind"`

	// Entrypoint is the path to the `.js` or `.wasm` file, resolved relative to
	// the manifest's own directory (e.g. "./csvstats.js").
	Entrypoint string `toml:"entrypoint"`

	// Description is what the model is shown when ranking tools. It is the whole
	// basis on which the tool gets selected, so an empty one is an error rather
	// than a default.
	Description string `toml:"description"`

	// DisplayName is an optional human-friendly label for TUI display, matching
	// plugin.ToolDefinition.
	DisplayName string `toml:"display_name"`

	// InputSchema is the path to a JSON Schema file for the tool's arguments,
	// resolved like Entrypoint. Optional: a tool taking no arguments needs none,
	// and gets the empty object schema.
	InputSchema string `toml:"input_schema"`

	// ABI is the guest contract version the module was built against. 0 means
	// unstated and is taken as ABIVersion — a `js` tool never builds anything, so
	// requiring its author to think about the ABI would be noise.
	ABI int `toml:"abi"`

	// Capabilities declares what the tool needs. It grants nothing
	// (docs/sandboxed-tools.md §6.3).
	Capabilities Declaration `toml:"capabilities"`

	// Resumable says this tool may end a call with a `continue` envelope and be
	// called again — long-running work, run as a job.
	//
	// It is not a capability: it confers no reach, so it sits outside
	// [capabilities] alongside the timeout override. What it changes is the
	// lifecycle, and R-TVM.10 makes the manifest authoritative for a tool's shape.
	Resumable bool `toml:"resumable"`
}

// LoadManifest reads and validates one manifest file. Every required field is
// checked here so an incomplete manifest is caught before its module is ever
// instantiated.
func LoadManifest(path string) (Manifest, error) {
	var m Manifest
	md, err := toml.DecodeFile(path, &m)
	if err != nil {
		return Manifest{}, fmt.Errorf("parse manifest: %w", err)
	}
	// An unknown key is an error rather than a warning: in a file whose entire
	// job is declaring capabilities, a typo'd key silently meaning nothing is the
	// worst possible failure mode.
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		return Manifest{}, fmt.Errorf("unknown manifest key %q", undecoded[0].String())
	}

	if m.Name == "" {
		return Manifest{}, fmt.Errorf("manifest missing required field: name")
	}
	if !validToolName(m.Name) {
		return Manifest{}, fmt.Errorf("tool name %q must match [a-z0-9_]+", m.Name)
	}
	if m.Entrypoint == "" {
		return Manifest{}, fmt.Errorf("manifest missing required field: entrypoint")
	}
	if m.Description == "" {
		return Manifest{}, fmt.Errorf("manifest missing required field: description")
	}
	switch m.Kind {
	case KindJS, KindWasm:
	case "":
		return Manifest{}, fmt.Errorf("manifest missing required field: kind (%q or %q)", KindJS, KindWasm)
	default:
		return Manifest{}, fmt.Errorf("unknown kind %q (want %q or %q)", m.Kind, KindJS, KindWasm)
	}
	if m.ABI == 0 {
		m.ABI = ABIVersion
	}
	if m.ABI != ABIVersion {
		return Manifest{}, fmt.Errorf("unsupported abi %d (this build speaks %d)", m.ABI, ABIVersion)
	}
	if _, err := m.Capabilities.capabilities(); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

// validToolName keeps tool names to the shape the model and the CLI both handle
// without quoting. It matches what plugin tools already use in practice.
func validToolName(s string) bool {
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' {
			continue
		}
		return false
	}
	return s != ""
}

// discovered is one candidate tool found in the scan directory. Err is set when
// the manifest is malformed or its entrypoint is missing — the candidate is then
// reported as skipped, and nothing of it is loaded or run.
type discovered struct {
	Name         string
	ManifestPath string
	Manifest     Manifest
	Source       []byte
	SchemaJSON   json.RawMessage
	Err          error
}

// discoverTools scans dir for `*.toml` manifests (top level only, matching the
// flat sidecar layout) and pairs each with its entrypoint. It reads files but
// instantiates nothing. Results are sorted by name for deterministic load order,
// which is what settles tool-vs-tool collisions in favor of the first loaded. A
// missing or empty dir yields no candidates, so an unset user_dir simply
// disables developer tools.
func discoverTools(dir string) []discovered {
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil // absent dir: developer tools disabled
	}

	var out []discovered
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".toml" {
			continue
		}
		manifestPath := filepath.Join(dir, e.Name())
		d := discovered{
			Name:         strings.TrimSuffix(e.Name(), ".toml"),
			ManifestPath: manifestPath,
		}

		m, err := LoadManifest(manifestPath)
		if err != nil {
			d.Err = err
			out = append(out, d)
			continue
		}
		d.Manifest = m
		d.Name = m.Name

		if d.Source, d.Err = readRelative(dir, m.Entrypoint); d.Err != nil {
			d.Err = fmt.Errorf("entrypoint %q: %w", m.Entrypoint, d.Err)
			out = append(out, d)
			continue
		}
		if d.SchemaJSON, d.Err = readSchema(dir, m.InputSchema); d.Err != nil {
			out = append(out, d)
			continue
		}
		out = append(out, d)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// readRelative reads a manifest-relative path, refusing anything that escapes
// the tools directory. An absolute entrypoint is allowed — an operator who
// writes one has said what they mean — but "../.." reaching out of a directory
// the operator thinks of as self-contained is not.
func readRelative(dir, rel string) ([]byte, error) {
	p := rel
	if !filepath.IsAbs(p) {
		p = filepath.Join(dir, p)
		if !strings.HasPrefix(filepath.Clean(p)+string(filepath.Separator), filepath.Clean(dir)+string(filepath.Separator)) {
			return nil, fmt.Errorf("path escapes the tools directory")
		}
	}
	info, err := os.Stat(p)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return nil, fmt.Errorf("is a directory, not a file")
	}
	return os.ReadFile(p) //nolint:gosec // path is manifest-declared and containment-checked above
}

// readSchema loads a tool's declared input schema, defaulting to the empty
// object schema. The schema is validated as JSON here rather than at first call,
// so a malformed one is a skipped tool with a named error instead of a tool the
// model is shown and cannot use.
func readSchema(dir, rel string) (json.RawMessage, error) {
	if rel == "" {
		return json.RawMessage(`{"type":"object","properties":{}}`), nil
	}
	raw, err := readRelative(dir, rel)
	if err != nil {
		return nil, fmt.Errorf("input_schema %q: %w", rel, err)
	}
	if !json.Valid(raw) {
		return nil, fmt.Errorf("input_schema %q: not valid JSON", rel)
	}
	return json.RawMessage(raw), nil
}
