package plugin

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/BurntSushi/toml"
)

// Manifest is a user plugin's sidecar TOML (`<name>.toml`). It is the gate that
// proves an executable is meant to be run as a Nine plugin: a binary without a
// manifest beside it is never executed. The manifest declares intent only — the
// authoritative tool list still comes from plugin.describe at load time — so it
// needs just a name and an entrypoint.
type Manifest struct {
	// Name is the declared plugin identity, surfaced in `nine plugins`.
	Name string `toml:"name"`
	// Entrypoint is the path to the plugin binary, resolved relative to the
	// manifest file's directory (e.g. "./weather").
	Entrypoint string `toml:"entrypoint"`
}

// LoadManifest reads and validates one manifest file. Both fields are required;
// a missing or malformed field is an error, so an incomplete manifest is caught
// before its binary is ever executed.
func LoadManifest(path string) (Manifest, error) {
	var m Manifest
	md, err := toml.DecodeFile(path, &m)
	if err != nil {
		return Manifest{}, fmt.Errorf("parse manifest: %w", err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		return Manifest{}, fmt.Errorf("unknown manifest key %q", undecoded[0].String())
	}
	if m.Name == "" {
		return Manifest{}, fmt.Errorf("manifest missing required field: name")
	}
	if m.Entrypoint == "" {
		return Manifest{}, fmt.Errorf("manifest missing required field: entrypoint")
	}
	return m, nil
}

// discovered is one candidate user plugin found in the scan directory. Err is
// set when the manifest is malformed or its binary is missing — the candidate is
// then reported as skipped rather than executed.
type discovered struct {
	Name         string
	ManifestPath string
	BinaryPath   string
	Err          error
}

// discoverPlugins scans dir for `*.toml` manifests (top level only, matching the
// flat sidecar layout) and pairs each with its entrypoint binary. It resolves and
// stat-checks the binary but does not execute anything. Results are sorted by
// name for deterministic load order (which settles user-vs-user tool collisions).
// A missing or empty dir yields no candidates.
func discoverPlugins(dir string) []discovered {
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil // absent dir: user plugins disabled
	}

	var out []discovered
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".toml" {
			continue
		}
		manifestPath := filepath.Join(dir, e.Name())
		name := e.Name()[:len(e.Name())-len(".toml")]
		d := discovered{Name: name, ManifestPath: manifestPath}

		m, err := LoadManifest(manifestPath)
		if err != nil {
			d.Err = err
			out = append(out, d)
			continue
		}
		if m.Name != "" {
			d.Name = m.Name
		}
		bin := m.Entrypoint
		if !filepath.IsAbs(bin) {
			bin = filepath.Join(dir, bin)
		}
		if info, err := os.Stat(bin); err != nil {
			d.Err = fmt.Errorf("entrypoint %q not found: %w", m.Entrypoint, err)
		} else if info.IsDir() {
			d.Err = fmt.Errorf("entrypoint %q is a directory, not an executable", m.Entrypoint)
		}
		d.BinaryPath = bin
		out = append(out, d)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
