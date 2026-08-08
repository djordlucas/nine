package deps

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// cache is the on-disk state under [tools].cache_dir, shared across tools and
// across daemon restarts. It holds two things:
//
//   - extracted package trees, keyed by their integrity hash — content-addressed,
//     so the same lodash-es@4.17.21 resolves once however many tools want it; and
//   - a resolution index mapping "name@range" to the concrete version+integrity it
//     resolved to, which is what gives `frozen` its offline behavior: a warm cache
//     resolves with no network, and a frozen build refuses anything not already in
//     the index (§4.4, "iterate open, then freeze").
type cache struct {
	root string
	mu   sync.Mutex // serializes index read/modify/write
}

func newCache(root string) *cache { return &cache{root: root} }

// resolution is one recorded name@range → concrete answer.
type resolution struct {
	Version   string `json:"version"`
	Integrity string `json:"integrity"`
	Tarball   string `json:"tarball"`
}

func (c *cache) indexPath() string { return filepath.Join(c.root, "resolutions.json") }

// pkgDir is where a package's extracted tree lives, keyed by integrity so the
// path is stable and collision-free across names and versions.
func (c *cache) pkgDir(integrity string) string {
	sum := sha256.Sum256([]byte(integrity))
	return filepath.Join(c.root, "pkg", hex.EncodeToString(sum[:]))
}

// lookupResolution returns a recorded resolution for name@range, if any.
func (c *cache) lookupResolution(name, rng string) (resolution, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	idx := c.readIndex()
	r, ok := idx[name+"@"+rng]
	return r, ok
}

// recordResolution persists name@range → r.
func (c *cache) recordResolution(name, rng string, r resolution) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	idx := c.readIndex()
	idx[name+"@"+rng] = r
	b, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(c.root, 0o755); err != nil {
		return err
	}
	return os.WriteFile(c.indexPath(), b, 0o644)
}

// readIndex loads the resolution index, treating any error (absent, corrupt) as
// an empty index — the cache is an optimization, never a source of truth.
func (c *cache) readIndex() map[string]resolution {
	idx := map[string]resolution{}
	b, err := os.ReadFile(c.indexPath())
	if err != nil {
		return idx
	}
	_ = json.Unmarshal(b, &idx)
	return idx
}

// hasPackage reports whether the extracted tree for integrity is already present.
func (c *cache) hasPackage(integrity string) bool {
	_, err := os.Stat(filepath.Join(c.pkgDir(integrity), "package.json"))
	return err == nil
}

// storePackage extracts a verified tarball into the content-addressed store,
// unless it is already there. files is the package tree with the tar's leading
// "package/" already stripped.
func (c *cache) storePackage(integrity string, files map[string][]byte) error {
	dir := c.pkgDir(integrity)
	if c.hasPackage(integrity) {
		return nil
	}
	for rel, data := range files {
		// Path traversal guard: a malicious tarball must not write outside its dir.
		clean := filepath.Clean(rel)
		if filepath.IsAbs(clean) || clean == ".." || hasDotDotPrefix(clean) {
			return fmt.Errorf("unsafe path in package %q", rel)
		}
		dst := filepath.Join(dir, clean)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func hasDotDotPrefix(p string) bool {
	return len(p) >= 3 && p[0] == '.' && p[1] == '.' && (p[2] == '/' || p[2] == filepath.Separator)
}

// packageJSON reads and parses a cached package's package.json.
func (c *cache) packageJSON(integrity string) (pkgJSON, error) {
	b, err := os.ReadFile(filepath.Join(c.pkgDir(integrity), "package.json"))
	if err != nil {
		return pkgJSON{}, err
	}
	var p pkgJSON
	if err := json.Unmarshal(b, &p); err != nil {
		return pkgJSON{}, fmt.Errorf("package.json: %w", err)
	}
	return p, nil
}

// pkgJSON is the sliver of package.json resolution needs. esbuild reads the file
// itself for entry-point fields (main/module/exports); this is only for walking
// the transitive dependency graph.
type pkgJSON struct {
	Name         string            `json:"name"`
	Version      string            `json:"version"`
	Dependencies map[string]string `json:"dependencies"`
}

// link makes node_modules/<name> in workDir point at the cached package tree, so
// esbuild resolves against a standard flat node_modules layout. A symlink avoids
// copying the tree for every tool that shares a dependency.
func (c *cache) link(workDir, name, integrity string) error {
	dst := filepath.Join(workDir, "node_modules", filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	_ = os.RemoveAll(dst)
	return os.Symlink(c.pkgDir(integrity), dst)
}
