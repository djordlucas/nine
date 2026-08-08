package deps

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/evanw/esbuild/pkg/api"
)

// Lockfile is the exact third-party code a bundled tool carries: name, resolved
// version, integrity, and who pulled it in. Stored beside the tool and printed by
// `nine tools show`, it makes "what npm code is in this daemon, and who asked for
// it" answerable after the fact (§4.4).
type Lockfile struct {
	Packages []LockEntry `json:"packages,omitempty"`
}

// LockEntry is one resolved package.
type LockEntry struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Integrity   string `json:"integrity"`
	RequestedBy string `json:"requested_by"` // "tool" for a top-level import, else a dependent
}

// Empty reports whether the tool pulled in no external packages.
func (l Lockfile) Empty() bool { return len(l.Packages) == 0 }

// Bundler resolves and inlines a generated tool's external dependencies. One is
// built per daemon from [tools.agent.deps]; a nil Bundler (deps off) means the
// caller treats any non-nine:* import as an error.
type Bundler struct {
	policy Policy
	client *client
	cache  *cache
}

// New builds a Bundler over cacheDir, using httpClient for registry access — the
// daemon's client, so registry traffic is the daemon's, never a guest's. Returns
// nil when the policy disables external deps, so a nil Bundler is the off state.
func New(policy Policy, cacheDir string, httpClient *http.Client) *Bundler {
	if !policy.Enabled() {
		return nil
	}
	return &Bundler{
		policy: policy,
		client: &client{http: httpClient, registry: policy.registry()},
		cache:  newCache(cacheDir),
	}
}

// Bundle resolves every non-nine:* import in source, inlines it, and returns a
// self-contained ESM module plus the lockfile of what it pulled in. A tool with
// no external imports returns (source, empty, nil) untouched — the common case,
// and the one that must stay cheap.
//
// Everything network- or filesystem-touching happens here, at write time, once.
// By the time the returned source is stored and later run, it has no imports left
// but nine:* and no way to reach the network (§4.3, §4.4).
func (b *Bundler) Bundle(ctx context.Context, source string) (string, Lockfile, error) {
	top := ExternalImports(source)
	if len(top) == 0 {
		return source, Lockfile{}, nil // pure or nine:*-only: nothing to do
	}

	work, err := os.MkdirTemp("", "nine-deps-")
	if err != nil {
		return "", Lockfile{}, err
	}
	defer os.RemoveAll(work) //nolint:errcheck

	lock, err := b.resolveTree(ctx, top, work)
	if err != nil {
		return "", Lockfile{}, err
	}

	bundled, err := b.esbuild(source, work)
	if err != nil {
		return "", Lockfile{}, err
	}
	if len(bundled) > b.policy.maxBundleBytes() {
		return "", Lockfile{}, fmt.Errorf("bundle is %d bytes, over the %d-byte cap ([tools.agent.deps].max_bundle_kb)",
			len(bundled), b.policy.maxBundleBytes())
	}
	return bundled, lock, nil
}

// resolveTree walks the dependency graph breadth-first, fetching and caching each
// package and linking it into work/node_modules for esbuild to resolve against.
// Policy (allowlist/open), integrity, and the budgets are all enforced here.
func (b *Bundler) resolveTree(ctx context.Context, top []string, work string) (Lockfile, error) {
	type want struct{ name, rng, by string }
	seen := map[string]bool{}
	var lock Lockfile

	queue := make([]want, 0, len(top))
	for _, n := range top {
		queue = append(queue, want{name: n, rng: "", by: "tool"})
	}

	for depth := 0; len(queue) > 0; depth++ {
		if depth > b.policy.maxDepth() {
			return Lockfile{}, fmt.Errorf("dependency tree exceeds max_depth=%d", b.policy.maxDepth())
		}
		var next []want
		for _, w := range queue {
			if seen[w.name] {
				continue // one version per name (flat node_modules; a stated simplification)
			}
			rng, err := b.policy.admit(w.name, w.rng)
			if err != nil {
				return Lockfile{}, err
			}
			res, err := b.acquire(ctx, w.name, rng)
			if err != nil {
				return Lockfile{}, err
			}
			if err := b.cache.link(work, w.name, res.Integrity); err != nil {
				return Lockfile{}, err
			}
			seen[w.name] = true
			lock.Packages = append(lock.Packages, LockEntry{
				Name: w.name, Version: res.Version, Integrity: res.Integrity, RequestedBy: w.by,
			})
			if len(seen) > b.policy.maxPackages() {
				return Lockfile{}, fmt.Errorf("dependency tree exceeds max_packages=%d (incl. transitive)", b.policy.maxPackages())
			}

			pj, err := b.cache.packageJSON(res.Integrity)
			if err != nil {
				return Lockfile{}, fmt.Errorf("%s: %w", w.name, err)
			}
			for dep, drng := range pj.Dependencies {
				if !seen[dep] {
					next = append(next, want{name: dep, rng: drng, by: w.name})
				}
			}
		}
		queue = next
	}

	sort.Slice(lock.Packages, func(i, j int) bool { return lock.Packages[i].Name < lock.Packages[j].Name })
	return lock, nil
}

// acquire resolves name@range to a concrete package and ensures its extracted
// tree is in the cache, honoring `frozen`: a frozen build resolves only from the
// recorded index and the local package store, never the network.
func (b *Bundler) acquire(ctx context.Context, name, rng string) (resolution, error) {
	res, ok := b.cache.lookupResolution(name, rng)
	if !ok {
		if b.policy.Frozen {
			return resolution{}, fmt.Errorf("%s@%s is not in the resolution cache and deps are frozen", name, rng)
		}
		var err error
		if res, err = b.client.resolve(ctx, name, rng); err != nil {
			return resolution{}, err
		}
		if err := b.cache.recordResolution(name, rng, res); err != nil {
			return resolution{}, err
		}
	}
	if !b.cache.hasPackage(res.Integrity) {
		if b.policy.Frozen {
			return resolution{}, fmt.Errorf("%s@%s tarball is not cached and deps are frozen", name, res.Version)
		}
		files, err := b.client.fetchTarball(ctx, res.Tarball, res.Integrity)
		if err != nil {
			return resolution{}, err
		}
		if err := b.cache.storePackage(res.Integrity, files); err != nil {
			return resolution{}, err
		}
	}
	return res, nil
}

// esbuild bundles source against work/node_modules into one self-contained ESM
// module. nine:* stays external (the host serves it at call time); a Node builtin
// import fails here under PlatformNeutral, which is the intended refusal — the
// custom resolver serves only from the verified cache and nothing else, so §4.3's
// rule is implemented by this pass, not weakened by it.
func (b *Bundler) esbuild(source, work string) (string, error) {
	result := api.Build(api.BuildOptions{
		Stdin: &api.StdinOptions{
			Contents:   source,
			ResolveDir: work,
			Sourcefile: "tool.js",
			Loader:     api.LoaderJS,
		},
		Bundle:        true,
		Format:        api.FormatESModule,
		Platform:      api.PlatformNeutral,
		AbsWorkingDir: work,
		Write:         false,
		LogLevel:      api.LogLevelSilent,
		// node_modules is a flat tree of symlinks into the content-addressed cache
		// (cache.link). Resolve through the symlink *path*, not its real target, so a
		// transitive import from node_modules/top walks up to node_modules/hidden
		// rather than into the cache dir, which has no node_modules of its own.
		PreserveSymlinks: true,
		Plugins: []api.Plugin{{
			Name: "nine-stdlib-external",
			Setup: func(pb api.PluginBuild) {
				pb.OnResolve(api.OnResolveOptions{Filter: `^nine:`}, func(a api.OnResolveArgs) (api.OnResolveResult, error) {
					return api.OnResolveResult{Path: a.Path, External: true}, nil
				})
			},
		}},
	})
	if len(result.Errors) > 0 {
		msgs := make([]string, 0, len(result.Errors))
		for _, e := range result.Errors {
			msgs = append(msgs, e.Text)
		}
		return "", fmt.Errorf("bundle failed: %s", strings.Join(msgs, "; "))
	}
	if len(result.OutputFiles) != 1 {
		return "", fmt.Errorf("bundle produced %d files, want 1", len(result.OutputFiles))
	}
	return string(result.OutputFiles[0].Contents), nil
}

// importSpec matches the module specifier of a static or dynamic import/export.
// It is used only to discover a tool's TOP-LEVEL external imports for prefetch;
// transitive deps come from each package's package.json, and a specifier this
// misses simply fails to resolve in esbuild (fail-closed), never resolves wrongly.
var importSpec = regexp.MustCompile(`(?:import|export)[^'"]*?from\s*["']([^"']+)["']|import\s*["']([^"']+)["']|import\s*\(\s*["']([^"']+)["']`)

// ExternalImports returns the distinct npm package names a source imports — bare
// specifiers only, with nine:* and relative/absolute paths excluded. Exported so
// the write path can refuse (deps off) or gate (§9.4) on an external import
// without running a full resolution.
func ExternalImports(source string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range importSpec.FindAllStringSubmatch(source, -1) {
		spec := m[1] + m[2] + m[3] // exactly one group matches per row
		if spec == "" || strings.HasPrefix(spec, "nine:") ||
			strings.HasPrefix(spec, ".") || strings.HasPrefix(spec, "/") {
			continue
		}
		name := packageName(spec)
		if name != "" && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// packageName reduces an import specifier to its package name: "lodash-es/chunk"
// -> "lodash-es", "@scope/pkg/sub" -> "@scope/pkg".
func packageName(spec string) string {
	parts := strings.Split(spec, "/")
	if strings.HasPrefix(spec, "@") {
		if len(parts) >= 2 {
			return parts[0] + "/" + parts[1]
		}
		return ""
	}
	return parts[0]
}
