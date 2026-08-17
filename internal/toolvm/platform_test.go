package toolvm

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// The papercut that motivated the dynamic import: harness.js used to reach the
// tool through a *static* import, which ES semantics evaluate before the
// importing module's body — so a tool's module-level console.log died with
// "console is not defined", which is the first thing anyone writes when
// debugging, and it implied the sandbox forbids logging.
func TestModuleScopeSeesThePlatform(t *testing.T) {
	for _, global := range []string{
		"console", "fetch", "TextEncoder", "TextDecoder", "URL",
		"URLSearchParams", "structuredClone", "setTimeout", "clearTimeout",
	} {
		t.Run(global, func(t *testing.T) {
			out, err := probe(t, `(() => { const atModuleScope = seen; return atModuleScope; })()`)
			_ = out
			_ = err
		})
	}
	// Checked as one tool so module scope is genuinely module scope.
	out, err := probeModule(t, `const seen = [
  typeof console, typeof fetch, typeof TextEncoder, typeof TextDecoder,
  typeof URL, typeof URLSearchParams, typeof structuredClone, typeof setTimeout
].join(",");
console.log("module scope works");
export default () => seen;`)
	if err != nil {
		t.Fatalf("module-level code failed: %v", err)
	}
	if strings.Contains(out, "undefined") {
		t.Errorf("something is missing at module scope: %s", out)
	}
}

// probeModule runs a whole tool source (rather than one expression), so module
// scope can be exercised.
func probeModule(t *testing.T, src string) (string, error) {
	t.Helper()
	dir := t.TempDir()
	writeTool(t, dir, "m", `
name = "m"
kind = "js"
entrypoint = "./m.js"
description = "m"
`, src)
	h := openHost(t, dir, nil)
	return h.Call(context.Background(), "m", json.RawMessage(`{}`))
}

func TestTextEncoderDecoder(t *testing.T) {
	for name, tc := range map[string]struct{ expr, want string }{
		"ascii":             {`Array.from(new TextEncoder().encode("hi")).join(",")`, "104,105"},
		"two byte":          {`Array.from(new TextEncoder().encode("é")).join(",")`, "195,169"},
		"three byte":        {`Array.from(new TextEncoder().encode("中")).join(",")`, "228,184,173"},
		"four byte emoji":   {`Array.from(new TextEncoder().encode("🎉")).join(",")`, "240,159,142,137"},
		"round trip":        {`new TextDecoder().decode(new TextEncoder().encode("héllo 中文 🎉"))`, "héllo 中文 🎉"},
		"decode from bytes": {`new TextDecoder().decode(new Uint8Array([104,105]))`, "hi"},
		"encoding name":     {`new TextEncoder().encoding`, "utf-8"},
		"invalid is replaced": {
			`new TextDecoder().decode(new Uint8Array([0xff,0x68,0x69]))`, "�hi"},
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

// A decoder that silently produced UTF-8 for a label it does not implement would
// be worse than one that refuses.
func TestTextDecoderRefusesOtherEncodings(t *testing.T) {
	out, err := probe(t, `(() => { try { new TextDecoder("shift_jis"); return "accepted"; }
                                   catch (e) { return "refused"; } })()`)
	if err != nil {
		t.Fatal(err)
	}
	if out != "refused" {
		t.Errorf("TextDecoder accepted a non-UTF-8 label")
	}
}

func TestTextDecoderFatal(t *testing.T) {
	out, err := probe(t, `(() => { try { new TextDecoder("utf-8", {fatal:true}).decode(new Uint8Array([0xff])); return "no throw"; }
                                   catch (e) { return "threw"; } })()`)
	if err != nil {
		t.Fatal(err)
	}
	if out != "threw" {
		t.Errorf("fatal decoder did not throw on invalid input")
	}
}

func TestURL(t *testing.T) {
	for name, tc := range map[string]struct{ expr, want string }{
		"pathname":     {`new URL("https://a.example/b/c?d=1#e").pathname`, "/b/c"},
		"protocol":     {`new URL("https://a.example/b").protocol`, "https:"},
		"hostname":     {`new URL("https://A.Example:8443/b").hostname`, "a.example"},
		"port":         {`new URL("https://a.example:8443/b").port`, "8443"},
		"host":         {`new URL("https://a.example:8443/b").host`, "a.example:8443"},
		"origin":       {`new URL("https://a.example:8443/b").origin`, "https://a.example:8443"},
		"hash":         {`new URL("https://a.example/b#frag").hash`, "#frag"},
		"search":       {`new URL("https://a.example/b?x=1&y=2").search`, "?x=1&y=2"},
		"param get":    {`new URL("https://a.example/b?x=1&y=2").searchParams.get("y")`, "2"},
		"param decode": {`new URL("https://a.example/b?q=a%20b%2Bc").searchParams.get("q")`, "a b+c"},
		"userinfo":     {`new URL("https://u:p@a.example/b").username`, "u"},
		"href":         {`new URL("https://a.example/b?x=1#f").href`, "https://a.example/b?x=1#f"},
		"toString":     {`String(new URL("https://a.example/b"))`, "https://a.example/b"},
		"default path": {`new URL("https://a.example").pathname`, "/"},
		"base absolute path": {
			`new URL("/x/y", "https://a.example/b/c").href`, "https://a.example/x/y"},
		"base relative": {
			`new URL("d", "https://a.example/b/c").href`, "https://a.example/b/d"},
		"base dotdot": {
			`new URL("../z", "https://a.example/b/c/d").href`, "https://a.example/b/z"},
		"base query only": {
			`new URL("?q=1", "https://a.example/b").href`, "https://a.example/b?q=1"},
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

func TestURLRejectsGarbage(t *testing.T) {
	out, err := probe(t, `(() => { try { new URL("not a url"); return "accepted"; }
                                   catch (e) { return "rejected"; } })()`)
	if err != nil {
		t.Fatal(err)
	}
	if out != "rejected" {
		t.Error("URL accepted a non-URL")
	}
}

func TestURLSearchParams(t *testing.T) {
	for name, tc := range map[string]struct{ expr, want string }{
		"build":         {`new URLSearchParams({a:"1",b:"x y"}).toString()`, "a=1&b=x%20y"},
		"append":        {`(() => { const p = new URLSearchParams(); p.append("a","1"); p.append("a","2"); return p.getAll("a").join("|"); })()`, "1|2"},
		"set":           {`(() => { const p = new URLSearchParams("a=1&a=2"); p.set("a","3"); return p.toString(); })()`, "a=3"},
		"delete":        {`(() => { const p = new URLSearchParams("a=1&b=2"); p.delete("a"); return p.toString(); })()`, "b=2"},
		"has":           {`String(new URLSearchParams("a=1").has("a"))`, "true"},
		"plus is space": {`new URLSearchParams("q=a+b").get("q")`, "a b"},
		"iterate":       {`Array.from(new URLSearchParams("a=1&b=2")).map(([k,v]) => k+v).join(",")`, "a1,b2"},
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

func TestStructuredClone(t *testing.T) {
	for name, tc := range map[string]struct{ expr, want string }{
		"deep object":        {`String(structuredClone({a:{b:{c:1}}}).a.b.c)`, "1"},
		"is a copy":          {`(() => { const o = {a:{b:1}}; const c = structuredClone(o); c.a.b = 2; return String(o.a.b); })()`, "1"},
		"date":               {`structuredClone({d:new Date(0)}).d.toISOString()`, "1970-01-01T00:00:00.000Z"},
		"map":                {`String(structuredClone(new Map([["k","v"]])).get("k"))`, "v"},
		"set":                {`String(structuredClone(new Set([1,2])).has(2))`, "true"},
		"regexp":             {`structuredClone(/ab+c/gi).source`, "ab+c"},
		"typed array":        {`Array.from(structuredClone(new Uint8Array([1,2,3]))).join(",")`, "1,2,3"},
		"undefined survives": {`(() => { const c = structuredClone({a:undefined}); return "a" in c ? "kept" : "dropped"; })()`, "kept"},
		// The reason it exists: JSON.parse(JSON.stringify(x)) throws here.
		"cycle": {`(() => { const o = {n:1}; o.self = o; const c = structuredClone(o); return String(c.self.self.n); })()`, "1"},
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

func TestStructuredCloneRefusesFunctions(t *testing.T) {
	out, err := probe(t, `(() => { try { structuredClone({f: () => 1}); return "cloned"; }
                                   catch (e) { return "refused"; } })()`)
	if err != nil {
		t.Fatal(err)
	}
	if out != "refused" {
		t.Error("structuredClone cloned a function")
	}
}

// Timers run in virtual time: ordering holds, no wall clock is spent.
func TestTimers(t *testing.T) {
	t.Run("await a timeout resolves", func(t *testing.T) {
		out, err := probeModule(t, `export default async () => {
  await new Promise((r) => setTimeout(r, 1000));
  return "resolved";
};`)
		if err != nil {
			t.Fatal(err)
		}
		if out != "resolved" {
			t.Errorf("got %q", out)
		}
	})

	t.Run("callbacks run in deadline order, not registration order", func(t *testing.T) {
		out, err := probeModule(t, `export default async () => {
  const seen = [];
  setTimeout(() => seen.push("c"), 300);
  setTimeout(() => seen.push("a"), 100);
  setTimeout(() => seen.push("b"), 200);
  await new Promise((r) => setTimeout(r, 400));
  return seen.join("");
};`)
		if err != nil {
			t.Fatal(err)
		}
		if out != "abc" {
			t.Errorf("order = %q, want abc", out)
		}
	})

	t.Run("equal deadlines break by registration order", func(t *testing.T) {
		out, err := probeModule(t, `export default async () => {
  const seen = [];
  setTimeout(() => seen.push("1"), 10);
  setTimeout(() => seen.push("2"), 10);
  await new Promise((r) => setTimeout(r, 20));
  return seen.join("");
};`)
		if err != nil {
			t.Fatal(err)
		}
		if out != "12" {
			t.Errorf("order = %q, want 12", out)
		}
	})

	t.Run("clearTimeout cancels", func(t *testing.T) {
		out, err := probeModule(t, `export default async () => {
  let fired = false;
  const id = setTimeout(() => { fired = true; }, 10);
  clearTimeout(id);
  await new Promise((r) => setTimeout(r, 50));
  return String(fired);
};`)
		if err != nil {
			t.Fatal(err)
		}
		if out != "false" {
			t.Error("a cleared timer fired")
		}
	})

	t.Run("setInterval repeats and can be cleared", func(t *testing.T) {
		out, err := probeModule(t, `export default async () => {
  let n = 0;
  const id = setInterval(() => { if (++n >= 3) clearInterval(id); }, 10);
  await new Promise((r) => setTimeout(r, 100));
  return String(n);
};`)
		if err != nil {
			t.Fatal(err)
		}
		if out != "3" {
			t.Errorf("interval fired %s times, want 3", out)
		}
	})

	// No wall clock is spent: this is the whole point, and the thing that would
	// otherwise make a sandboxed tool able to sleep away its deadline.
	t.Run("virtual time costs no real time", func(t *testing.T) {
		out, err := probeModule(t, `export default async () => {
  const t0 = Date.now();
  await new Promise((r) => setTimeout(r, 3000));
  return String(Date.now() - t0 < 1000);
};`)
		if err != nil {
			t.Fatal(err)
		}
		if out != "true" {
			t.Error("a 3s timeout consumed real time")
		}
	})

	// An unbounded interval must fail legibly rather than hang until the deadline.
	t.Run("a never-ending interval is stopped with a clear message", func(t *testing.T) {
		_, err := probeModule(t, `export default async () => {
  setInterval(() => {}, 1);
  await new Promise(() => {});
  return "unreachable";
};`)
		if err == nil {
			t.Fatal("an endless interval did not fail")
		}
		if !strings.Contains(err.Error(), "timer budget") {
			t.Errorf("unhelpful error: %v", err)
		}
	})
}

// Decision §8.2: a format request that cannot be honored fails loudly instead of
// returning a confidently wrong answer.
func TestLocaleFormattingThrowsRatherThanLying(t *testing.T) {
	for name, expr := range map[string]string{
		"date with timeZone": `new Date(0).toLocaleString("en-US", { timeZone: "Europe/Paris" })`,
		"date with locale":   `new Date(0).toLocaleDateString("de-DE")`,
		"number with locale": `(1234.5).toLocaleString("de-DE")`,
	} {
		t.Run(name, func(t *testing.T) {
			out, err := probe(t, `(() => { try { return `+expr+`; } catch (e) { return "THREW: " + e.message; } })()`)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(out, "THREW:") {
				t.Errorf("silently returned %q instead of throwing", out)
			}
			if !strings.Contains(out, "Intl") {
				t.Errorf("message does not explain why: %s", out)
			}
		})
	}
}

// …but the no-argument form is not a lie, and must keep working.
func TestLocaleFormattingWithoutArgumentsStillWorks(t *testing.T) {
	for _, expr := range []string{
		`typeof new Date(0).toLocaleString()`,
		`typeof (1234.5).toLocaleString()`,
	} {
		out, err := probe(t, expr)
		if err != nil {
			t.Fatalf("%s: %v", expr, err)
		}
		if out != "string" {
			t.Errorf("%s = %q, want a string", expr, out)
		}
	}
}

// The internals are ours. Leaving them on globalThis collided with author code
// and invited tools to bind to things we want to keep changing.
//
// Enumerated rather than listed by name on purpose: the first version of this
// checked three specific globals, so when M5 added seven more host bindings it
// stayed green while every one of them leaked. Asking "what is left?" cannot go
// stale as bindings are added.
func TestHarnessInternalsAreNotExposed(t *testing.T) {
	out, err := probe(t, `Object.getOwnPropertyNames(globalThis)
		.filter((n) => n.startsWith("__nine"))
		.sort()
		.join(",")`)
	if err != nil {
		t.Fatal(err)
	}
	// __nine_result is the one exception and has to stay: qjs_host.c reads the
	// result back off the global object after evaluation.
	if out != "__nine_result" {
		t.Errorf("harness internals are reachable from tool code: %s\n"+
			"(only __nine_result may survive; capture the rest into closures and delete them)", out)
	}
}

// The nine:* stdlib, now that a developer tool may import it.
func TestDeveloperToolCanImportStdlib(t *testing.T) {
	// Two rows without a header, one with — checked both ways so this is testing
	// the import rather than a guess about the parser.
	out, err := probeModule(t, `import { parse } from "nine:csv";
export default () => parse("a,b\n1,2").length + "|" + parse("a,b\n1,2", { header: true }).length;`)
	if err != nil {
		t.Fatal(err)
	}
	if out != "2|1" {
		t.Errorf("nine:csv parse returned %q, want 2|1", out)
	}
}
