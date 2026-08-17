package toolvm

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// jsTool builds a js tool with a manifest body and grants, and returns a caller.
func jsTool(t *testing.T, manifestCaps, src string, grants map[string]Grant) func(string) (string, error) {
	t.Helper()
	dir := t.TempDir()
	writeTool(t, dir, "t", `
name = "t"
kind = "js"
entrypoint = "./t.js"
description = "t"
`+manifestCaps, src)
	h := openHost(t, dir, grants)
	for _, s := range h.Status() {
		if !s.Loaded {
			t.Fatalf("tool did not load: %s", s.Err)
		}
	}
	return func(args string) (string, error) {
		return h.Call(context.Background(), "t", json.RawMessage(args))
	}
}

// THE headline finding of docs/rich-js-tools.md: fs.read was declarable,
// grantable, validated, reported by `nine tools` — and unreachable from the kind
// nearly every tool is written in.
func TestJSCanReadAGrantedMount(t *testing.T) {
	host := t.TempDir()
	if err := os.WriteFile(filepath.Join(host, "hello.txt"), []byte("from the host"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(host, "bytes.bin"), []byte{0x89, 0xff, 0x00}, 0o600); err != nil {
		t.Fatal(err)
	}

	call := jsTool(t, `
[capabilities]
fs = ["read"]
`, `import { readFileText, readFile, readDir, stat, exists, mounts } from "nine:fs";
export default () => ({
  text: readFileText("/data/hello.txt"),
  bytes: Array.from(readFile("/data/bytes.bin")),
  dir: readDir("/data").sort().join(","),
  size: stat("/data/hello.txt").size,
  isFile: stat("/data/hello.txt").isFile,
  missing: stat("/data/nope.txt"),
  exists: exists("/data/hello.txt"),
  mounts: mounts().read.join(","),
});`, map[string]Grant{"t": {FSRead: []Mount{{Host: host, Guest: "/data"}}}})

	out, err := call(`{}`)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"text":"from the host"`,
		`"bytes":[137,255,0]`, // bytes, not U+FFFD
		`"dir":"bytes.bin,hello.txt"`,
		`"size":13`,
		`"isFile":true`,
		`"missing":null`,
		`"exists":true`,
		`"mounts":"/data"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in\n  %s", want, out)
		}
	}
}

func TestJSCanWriteAGrantedMount(t *testing.T) {
	out := t.TempDir()
	call := jsTool(t, `
[capabilities]
fs = ["write"]
`, `import { writeFile } from "nine:fs";
export default () => {
  writeFile("/out/text.txt", "written by js");
  writeFile("/out/bytes.bin", new Uint8Array([1,2,255]));
  return "ok";
};`, map[string]Grant{"t": {FSWrite: []Mount{{Host: out, Guest: "/out"}}}})

	if _, err := call(`{}`); err != nil {
		t.Fatal(err)
	}
	txt, err := os.ReadFile(filepath.Join(out, "text.txt"))
	if err != nil || string(txt) != "written by js" {
		t.Errorf("text.txt = %q, %v", txt, err)
	}
	raw, err := os.ReadFile(filepath.Join(out, "bytes.bin"))
	if err != nil || len(raw) != 3 || raw[0] != 1 || raw[2] != 255 {
		t.Errorf("bytes.bin = %v, %v", raw, err)
	}
}

// writeFile takes three data shapes and only one of them is obvious. The two
// below were correct but unpinned, and both fail in the silent-wrong-bytes way
// rather than the loud way if they ever regress.
func TestJSWriteAcceptsEveryByteShape(t *testing.T) {
	for name, tc := range map[string]struct {
		expr string
		want []byte
	}{
		"plain ArrayBuffer": {`new Uint8Array([7,8,9]).buffer`, []byte{7, 8, 9}},
		// A view with a byteOffset exercises the offset arithmetic in
		// js_nine_fs_write; getting it wrong writes the wrong window of memory.
		"view with byteOffset": {`new Uint8Array(new Uint8Array([9,9,1,2,3]).buffer, 2, 3)`, []byte{1, 2, 3}},
		"Uint8Array":           {`new Uint8Array([1,2,255])`, []byte{1, 2, 255}},
	} {
		t.Run(name, func(t *testing.T) {
			out := t.TempDir()
			call := jsTool(t, `
[capabilities]
fs = ["write"]
`, `import { writeFile } from "nine:fs";
export default () => { writeFile("/out/f.bin", `+tc.expr+`); return "ok"; };`,
				map[string]Grant{"t": {FSWrite: []Mount{{Host: out, Guest: "/out"}}}})

			if _, err := call(`{}`); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(filepath.Join(out, "f.bin"))
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("wrote %v, want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("wrote %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// fs.js admits a read against a write-only grant, since a write mount is
// readable. Asserted because it is a deliberate asymmetry, not an accident.
func TestJSCanReadThroughAWriteMount(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "seed.txt"), []byte("readable"), 0o600); err != nil {
		t.Fatal(err)
	}
	call := jsTool(t, `
[capabilities]
fs = ["write"]
`, `import { readFileText } from "nine:fs";
export default () => readFileText("/out/seed.txt");`,
		map[string]Grant{"t": {FSWrite: []Mount{{Host: dir, Guest: "/out"}}}})

	out, err := call(`{}`)
	if err != nil {
		t.Fatal(err)
	}
	if out != "readable" {
		t.Errorf("got %q, want %q", out, "readable")
	}
}

// The capability model is unchanged: the module exists, and without a grant it
// says so rather than reaching anything.
func TestJSFSWithoutAGrantIsRefusedClearly(t *testing.T) {
	call := jsTool(t, ``, `import { readFileText } from "nine:fs";
export default () => readFileText("/data/x");`, nil)

	_, err := call(`{}`)
	if err == nil {
		t.Fatal("an ungranted read succeeded")
	}
	if !strings.Contains(err.Error(), "fs.read is not granted") {
		t.Errorf("unhelpful error: %v", err)
	}
}

// Read-only means read-only, and that is wazero's enforcement rather than ours.
func TestJSCannotWriteAReadOnlyMount(t *testing.T) {
	host := t.TempDir()
	call := jsTool(t, `
[capabilities]
fs = ["read"]
`, `import { writeFile } from "nine:fs";
export default () => { writeFile("/data/new.txt", "nope"); return "wrote"; };`,
		map[string]Grant{"t": {FSRead: []Mount{{Host: host, Guest: "/data"}}}})

	if _, err := call(`{}`); err == nil {
		t.Fatal("wrote to a read-only mount")
	}
	if _, err := os.Stat(filepath.Join(host, "new.txt")); err == nil {
		t.Fatal("a file appeared in a read-only mount")
	}
}

// A tool cannot climb out of its pre-open. This is wazero's guarantee, not a
// path check of ours — which is exactly why fs went through libc rather than a
// host function (docs/rich-js-tools.md §6.4).
func TestJSCannotEscapeTheMount(t *testing.T) {
	host := t.TempDir()
	secret := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(secret, []byte("do not read me"), 0o600); err != nil {
		t.Fatal(err)
	}

	call := jsTool(t, `
[capabilities]
fs = ["read"]
`, `import { readFileText } from "nine:fs";
export default ({ path }) => {
  try { return "READ: " + readFileText(path); } catch (e) { return "refused"; }
};`, map[string]Grant{"t": {FSRead: []Mount{{Host: host, Guest: "/data"}}}})

	for _, path := range []string{
		"/etc/passwd",
		"/data/../../etc/passwd",
		secret,
		"/data/../" + filepath.Base(secret),
	} {
		args, _ := json.Marshal(map[string]string{"path": path})
		out, err := call(string(args))
		if err == nil && strings.HasPrefix(out, "READ:") {
			t.Errorf("escaped the mount via %q: %s", path, out)
		}
	}
}

func TestJSCanReadGrantedEnv(t *testing.T) {
	t.Setenv("DEMO_TZ", "Europe/Paris")
	t.Setenv("DEMO_OTHER", "should be invisible")

	call := jsTool(t, `
[capabilities]
env = ["DEMO_TZ"]
`, `import { get, keys, all } from "nine:env";
export default () => ({ tz: get("DEMO_TZ"), keys: keys().join(","), all: Object.keys(all()).join(",") });`,
		map[string]Grant{"t": {Env: []string{"DEMO_TZ"}}})

	out, err := call(`{}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"tz":"Europe/Paris"`) {
		t.Errorf("granted key not readable: %s", out)
	}
	if strings.Contains(out, "should be invisible") {
		t.Error("an ungranted key leaked")
	}
}

func TestJSEnvRefusesUngrantedKeys(t *testing.T) {
	t.Setenv("DEMO_TZ", "Europe/Paris")
	t.Setenv("DEMO_SECRET", "nope")

	call := jsTool(t, `
[capabilities]
env = ["DEMO_TZ"]
`, `import { get } from "nine:env";
export default ({ key }) => { try { return "GOT: " + get(key); } catch (e) { return "refused: " + e.message; } };`,
		map[string]Grant{"t": {Env: []string{"DEMO_TZ"}}})

	out, err := call(`{"key":"DEMO_SECRET"}`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(out, "GOT:") {
		t.Errorf("read an ungranted key: %s", out)
	}
	if !strings.Contains(out, "not granted") {
		t.Errorf("unhelpful refusal: %s", out)
	}
}

func TestJSEnvWithoutAGrantIsRefusedClearly(t *testing.T) {
	call := jsTool(t, ``, `import { get } from "nine:env";
export default () => get("PATH");`, nil)

	_, err := call(`{}`)
	if err == nil {
		t.Fatal("an ungranted env read succeeded")
	}
	if !strings.Contains(err.Error(), "env is not granted") {
		t.Errorf("unhelpful error: %v", err)
	}
}

// nine.caps describes; it never confers. A tool with no grant is told so.
func TestCapsDescribesAndDoesNotConfer(t *testing.T) {
	host := t.TempDir()
	call := jsTool(t, `
[capabilities]
fs = ["read"]
`, `import { mounts } from "nine:fs";
export default () => JSON.stringify(mounts());`,
		map[string]Grant{"t": {FSRead: []Mount{{Host: host, Guest: "/data"}}}})

	out, err := call(`{}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"read":["/data"]`) {
		t.Errorf("mounts() = %s", out)
	}
	// The host path must not be disclosed: the guest mapping exists to hide it.
	if strings.Contains(out, host) {
		t.Errorf("the host path leaked into the guest: %s", out)
	}
}

func TestCrypto(t *testing.T) {
	for name, tc := range map[string]struct{ expr, want string }{
		"getRandomValues fills": {
			`(() => { const a = new Uint8Array(32); crypto.getRandomValues(a);
                      return String(a.some((x) => x !== 0)); })()`, "true"},
		"returns the same array": {
			`(() => { const a = new Uint8Array(4); return String(crypto.getRandomValues(a) === a); })()`, "true"},
		"uuid shape": {
			`String(/^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/.test(crypto.randomUUID()))`, "true"},
		"uuids differ": {
			`String(crypto.randomUUID() !== crypto.randomUUID())`, "true"},
		"floats are refused": {
			`(() => { try { crypto.getRandomValues(new Float64Array(2)); return "accepted"; }
                      catch (e) { return "refused"; } })()`, "refused"},
		"larger than one getentropy chunk": {
			`(() => { const a = new Uint8Array(1000); crypto.getRandomValues(a);
                      return String(a.filter((x) => x === 0).length < 100); })()`, "true"},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := probe(t, tc.expr)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("%s = %q, want %q", tc.expr, got, tc.want)
			}
		})
	}
}

// Randomness must not repeat across calls: a fresh instance per call means a
// fresh PRNG seed, and a deterministic one would make every "random" id the same.
func TestCryptoDiffersAcrossCalls(t *testing.T) {
	dir := t.TempDir()
	writeTool(t, dir, "r", `
name = "r"
kind = "js"
entrypoint = "./r.js"
description = "r"
`, `export default () => crypto.randomUUID();`)
	h := openHost(t, dir, nil)

	seen := map[string]bool{}
	for i := 0; i < 5; i++ {
		out, err := h.Call(context.Background(), "r", json.RawMessage(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		if seen[out] {
			t.Fatalf("uuid %s repeated across calls", out)
		}
		seen[out] = true
	}
}
