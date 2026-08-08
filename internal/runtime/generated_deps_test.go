package runtime

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

	"nine/internal/config"
	"nine/internal/memory/memtest"
	"nine/internal/toolvm/deps"
)

// mockRegistry serves one package (name→indexJS) as an npm-compatible registry,
// enough to drive the bridge's write-time bundling end to end.
func mockRegistry(t *testing.T, name, indexJS string) *httptest.Server {
	t.Helper()
	pj, _ := json.Marshal(map[string]any{"name": name, "version": "1.0.0", "module": "index.js", "main": "index.js"})
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for file, content := range map[string]string{"package.json": string(pj), "index.js": indexJS} {
		_ = tw.WriteHeader(&tar.Header{Name: "package/" + file, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg})
		_, _ = tw.Write([]byte(content))
	}
	_ = tw.Close()
	_ = gz.Close()
	tarball := buf.Bytes()
	sum := sha512.Sum512(tarball)
	integrity := "sha512-" + base64.StdEncoding.EncodeToString(sum[:])

	srv := httptest.NewUnstartedServer(nil)
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/tgz/") {
			_, _ = w.Write(tarball)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"versions": map[string]any{"1.0.0": map[string]any{
				"dist": map[string]any{"tarball": srv.URL + "/tgz/" + name, "integrity": integrity},
			}},
		})
	})
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

// A generated tool that imports an allowlisted package is bundled at write time
// and runs — the inlined dependency and all — under the real blob, with the
// lockfile recorded in the row.
func TestWriteBundlesExternalDependency(t *testing.T) {
	srv := mockRegistry(t, "leftpad", `export default (s, n) => String(s).padStart(n, " ");`)
	policy := deps.Policy{Mode: deps.ModeAllowlist, Registry: srv.URL, Allow: []deps.Allow{{Name: "leftpad", Version: "*"}}}

	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := &config.Config{}
	cfg.Tools.Enabled = true
	cfg.Tools.Agent.Enabled = true
	host := OpenSandboxedTools(context.Background(), cfg, store, nil)
	t.Cleanup(func() { _ = host.Close(context.Background()) })

	gt := NewGeneratedToolStore(store, host, nil, deps.New(policy, t.TempDir(), &http.Client{}), false)
	if gt == nil {
		t.Fatal("tier off")
	}

	src := `import leftpad from "leftpad"; export default ({s}) => ({ out: leftpad(s, 4) });`
	if _, err := gt.Write(context.Background(), genSpec("padder", src, nil)); err != nil {
		t.Fatalf("write: %v", err)
	}

	out, err := host.Call(context.Background(), "padder", []byte(`{"s":"7"}`))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if out != `{"out":"   7"}` {
		t.Errorf("output = %q", out)
	}

	got, ok, err := store.GeneratedToolGet("padder")
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if strings.Contains(got.Source, `from "leftpad"`) {
		t.Errorf("stored source still imports leftpad:\n%s", got.Source)
	}
	if !strings.Contains(string(got.Lockfile), "leftpad") {
		t.Errorf("lockfile missing the dependency: %s", got.Lockfile)
	}
}

// Deps off: importing an external package is refused at write time with a message
// the model can act on, not left to fail at call time.
func TestWriteRefusesDepWhenDepsOff(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := &config.Config{}
	cfg.Tools.Enabled = true
	cfg.Tools.Agent.Enabled = true
	host := OpenSandboxedTools(context.Background(), cfg, store, nil)
	t.Cleanup(func() { _ = host.Close(context.Background()) })

	gt := NewGeneratedToolStore(store, host, nil, nil, false) // nil bundler = deps off
	_, err = gt.Write(context.Background(), genSpec("x", `import _ from "leftpad"; export default () => 1;`, nil))
	if err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("want a deps-disabled refusal, got %v", err)
	}
	if _, ok, _ := store.GeneratedToolGet("x"); ok {
		t.Error("a refused write left a row behind")
	}
}
