package deps

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// --- a mock npm registry -----------------------------------------------------

type mockPkg struct {
	version   string
	files     map[string]string
	tarball   []byte
	integrity string
}

type mockRegistry struct {
	srv  *httptest.Server
	pkgs map[string]*mockPkg
}

func newRegistry(t *testing.T) *mockRegistry {
	t.Helper()
	r := &mockRegistry{pkgs: map[string]*mockPkg{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/tgz/", func(w http.ResponseWriter, req *http.Request) {
		name := strings.TrimPrefix(req.URL.Path, "/tgz/")
		p, ok := r.pkgs[name]
		if !ok {
			http.NotFound(w, req)
			return
		}
		_, _ = w.Write(p.tarball)
	})
	// Everything else is a packument request for the (URL-decoded) package name.
	mux.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
		name := strings.TrimPrefix(req.URL.Path, "/")
		p, ok := r.pkgs[name]
		if !ok {
			http.NotFound(w, req)
			return
		}
		doc := map[string]any{
			"versions": map[string]any{
				p.version: map[string]any{
					"dist": map[string]any{
						"tarball":   r.srv.URL + "/tgz/" + name,
						"integrity": p.integrity,
					},
				},
			},
		}
		_ = json.NewEncoder(w).Encode(doc)
	})
	r.srv = httptest.NewServer(mux)
	t.Cleanup(r.srv.Close)
	return r
}

// add registers a package whose entry module is indexJS, with the given runtime
// deps recorded in its package.json.
func (r *mockRegistry) add(t *testing.T, name, version, indexJS string, deps map[string]string) {
	t.Helper()
	pj := map[string]any{"name": name, "version": version, "module": "index.js", "main": "index.js"}
	if len(deps) > 0 {
		pj["dependencies"] = deps
	}
	pjBytes, _ := json.Marshal(pj)
	files := map[string]string{"package.json": string(pjBytes), "index.js": indexJS}
	tb := makeTarball(t, files)
	sum := sha512.Sum512(tb)
	r.pkgs[name] = &mockPkg{
		version:   version,
		files:     files,
		tarball:   tb,
		integrity: "sha512-" + base64.StdEncoding.EncodeToString(sum[:]),
	}
}

// corrupt swaps a package's served tarball for different bytes, so the served
// integrity no longer matches — the DNS-rebinding-of-supply-chains test.
func (r *mockRegistry) corrupt(name string) { r.pkgs[name].tarball = []byte("tampered") }

func makeTarball(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		hdr := &tar.Header{Name: "package/" + name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func bundler(t *testing.T, reg *mockRegistry, p Policy) *Bundler {
	t.Helper()
	p.Registry = reg.srv.URL
	b := New(p, t.TempDir(), reg.srv.Client())
	if b == nil {
		t.Fatal("New returned nil for an enabled policy")
	}
	return b
}

// --- tests -------------------------------------------------------------------

const leftpadJS = `export default (s, n = 4) => String(s).padStart(n, " ");`

func TestBundleAllowlistHappyPath(t *testing.T) {
	reg := newRegistry(t)
	reg.add(t, "leftpad", "1.0.0", leftpadJS, nil)

	b := bundler(t, reg, Policy{Mode: ModeAllowlist, Allow: []Allow{{Name: "leftpad", Version: "^1.0.0"}}})
	src := `import leftpad from "leftpad"; export default ({s}) => ({ out: leftpad(s, 4) });`

	out, lock, err := b.Bundle(context.Background(), src)
	if err != nil {
		t.Fatalf("Bundle: %v", err)
	}
	if strings.Contains(out, `from "leftpad"`) || strings.Contains(out, "import leftpad") {
		t.Errorf("import not inlined:\n%s", out)
	}
	if !strings.Contains(out, "padStart") {
		t.Errorf("dependency body missing from bundle")
	}
	if len(lock.Packages) != 1 || lock.Packages[0].Name != "leftpad" || lock.Packages[0].Version != "1.0.0" {
		t.Errorf("lockfile wrong: %+v", lock.Packages)
	}
	if lock.Packages[0].RequestedBy != "tool" {
		t.Errorf("top-level dep should be requested_by=tool, got %q", lock.Packages[0].RequestedBy)
	}
}

func TestBundleNineStarLeftExternal(t *testing.T) {
	reg := newRegistry(t)
	b := bundler(t, reg, Policy{Mode: ModeOpen})
	src := `import { parse } from "nine:csv"; export default ({t}) => parse(t).length;`
	out, lock, err := b.Bundle(context.Background(), src)
	if err != nil {
		t.Fatalf("Bundle: %v", err)
	}
	if !lock.Empty() {
		t.Errorf("nine:* should resolve nothing, got %+v", lock.Packages)
	}
	if !strings.Contains(out, "nine:csv") {
		t.Errorf("nine:* import should stay external in the bundle:\n%s", out)
	}
}

func TestBundlePureSourceUntouched(t *testing.T) {
	reg := newRegistry(t)
	b := bundler(t, reg, Policy{Mode: ModeAllowlist})
	src := `export default ({a,b}) => a+b;`
	out, lock, err := b.Bundle(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	if out != src || !lock.Empty() {
		t.Errorf("a pure tool must pass through untouched")
	}
}

func TestBundleAllowlistRejectsUnlisted(t *testing.T) {
	reg := newRegistry(t)
	reg.add(t, "sneaky", "1.0.0", leftpadJS, nil)
	b := bundler(t, reg, Policy{Mode: ModeAllowlist, Allow: []Allow{{Name: "leftpad", Version: "*"}}})
	_, _, err := b.Bundle(context.Background(), `import x from "sneaky"; export default () => x;`)
	if err == nil || !strings.Contains(err.Error(), "allow") {
		t.Fatalf("unlisted package should be refused with an allowlist error, got %v", err)
	}
}

func TestBundleTransitiveMustBeAllowlisted(t *testing.T) {
	reg := newRegistry(t)
	reg.add(t, "top", "1.0.0", `import dep from "hidden"; export default (x) => dep(x);`, map[string]string{"hidden": "^1.0.0"})
	reg.add(t, "hidden", "1.0.0", leftpadJS, nil)

	// "top" allowed, "hidden" not → the transitive dep is refused (§4.4).
	b := bundler(t, reg, Policy{Mode: ModeAllowlist, Allow: []Allow{{Name: "top", Version: "*"}}})
	_, _, err := b.Bundle(context.Background(), `import top from "top"; export default (x) => top(x);`)
	if err == nil || !strings.Contains(err.Error(), "hidden") {
		t.Fatalf("off-allowlist transitive dep should be refused, got %v", err)
	}

	// Both allowed → resolves, lockfile records the transitive's requester.
	b2 := bundler(t, reg, Policy{Mode: ModeAllowlist, Allow: []Allow{{Name: "top", Version: "*"}, {Name: "hidden", Version: "*"}}})
	_, lock, err := b2.Bundle(context.Background(), `import top from "top"; export default (x) => top(x);`)
	if err != nil {
		t.Fatalf("both allowed: %v", err)
	}
	if len(lock.Packages) != 2 {
		t.Fatalf("want 2 packages, got %+v", lock.Packages)
	}
	for _, p := range lock.Packages {
		if p.Name == "hidden" && p.RequestedBy != "top" {
			t.Errorf("hidden should be requested_by=top, got %q", p.RequestedBy)
		}
	}
}

func TestBundleIntegrityMismatchRefused(t *testing.T) {
	reg := newRegistry(t)
	reg.add(t, "leftpad", "1.0.0", leftpadJS, nil)
	reg.corrupt("leftpad") // served bytes no longer match the advertised integrity
	b := bundler(t, reg, Policy{Mode: ModeOpen})
	_, _, err := b.Bundle(context.Background(), `import x from "leftpad"; export default () => x;`)
	if err == nil || !strings.Contains(err.Error(), "integrity") {
		t.Fatalf("integrity mismatch should be refused, got %v", err)
	}
}

func TestBundleNodeBuiltinRefused(t *testing.T) {
	reg := newRegistry(t)
	reg.add(t, "needsfs", "1.0.0", `import fs from "fs"; export default () => fs;`, nil)
	b := bundler(t, reg, Policy{Mode: ModeOpen})
	_, _, err := b.Bundle(context.Background(), `import x from "needsfs"; export default () => x;`)
	if err == nil {
		t.Fatal("a package importing a Node builtin should fail to bundle under PlatformNeutral")
	}
}

func TestBundleOffMeansNil(t *testing.T) {
	if New(Policy{Mode: ModeOff}, t.TempDir(), http.DefaultClient) != nil {
		t.Fatal("deps off should yield a nil Bundler")
	}
}

func TestBundleMaxPackages(t *testing.T) {
	reg := newRegistry(t)
	reg.add(t, "a", "1.0.0", `import b from "b"; export default () => b;`, map[string]string{"b": "*"})
	reg.add(t, "b", "1.0.0", leftpadJS, nil)
	b := bundler(t, reg, Policy{Mode: ModeOpen, MaxPackages: 1})
	_, _, err := b.Bundle(context.Background(), `import a from "a"; export default () => a;`)
	if err == nil || !strings.Contains(err.Error(), "max_packages") {
		t.Fatalf("budget should refuse the tree, got %v", err)
	}
}

func TestBundleFrozenServesFromCache(t *testing.T) {
	reg := newRegistry(t)
	reg.add(t, "leftpad", "1.0.0", leftpadJS, nil)
	cacheDir := t.TempDir()
	src := `import x from "leftpad"; export default ({s}) => x(s);`

	// Warm the cache with a normal open build.
	warm := New(Policy{Mode: ModeOpen, Registry: reg.srv.URL}, cacheDir, reg.srv.Client())
	if _, _, err := warm.Bundle(context.Background(), src); err != nil {
		t.Fatalf("warm: %v", err)
	}

	// Now freeze and point at a dead registry — resolution must come from cache.
	dead := "http://127.0.0.1:1" // nothing listens here
	frozen := New(Policy{Mode: ModeOpen, Registry: dead, Frozen: true}, cacheDir, &http.Client{})
	if _, _, err := frozen.Bundle(context.Background(), src); err != nil {
		t.Fatalf("frozen build should resolve from cache, got %v", err)
	}
}
