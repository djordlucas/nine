package toolvm

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// stdlibHost opens a host with the generated tier on and an empty ceiling — the
// nine:* stdlib needs no capability, so a pure-transform tool importing it runs
// under the zero grant.
func stdlibHost(t *testing.T) *Host {
	t.Helper()
	h, err := Open(context.Background(), Config{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { h.Close(context.Background()) }) //nolint:errcheck
	h.SetAgentConfig(AgentConfig{Enabled: true})
	return h
}

// loadGen registers one generated tool importing the nine:* stdlib and returns
// its output for args.
func loadGen(t *testing.T, h *Host, name, source, args string) string {
	t.Helper()
	h.LoadGenerated(context.Background(), []Generated{{Name: name, Source: source}}, nil)
	if h.Get(name) == nil {
		t.Fatalf("generated tool %q did not load: %+v", name, h.Status())
	}
	out, err := h.Call(context.Background(), name, json.RawMessage(args))
	if err != nil {
		t.Fatalf("Call(%s): %v", name, err)
	}
	return out
}

// Every nine:* module must parse and run under the real committed blob (no
// std/os), so a bump that breaks one fails here rather than at an agent's call.
func TestStdlibCSVRoundTrip(t *testing.T) {
	h := stdlibHost(t)
	src := `import { parse, format } from "nine:csv";
	export default ({ text }) => {
	  const rows = parse(text, { header: true });
	  return { count: rows.length, first: rows[0], csv: format(rows.map(r => [r.a, r.b])) };
	};`
	out := loadGen(t, h, "csv_tool", src, `{"text":"a,b\n1,2\n3,\"x,y\""}`)

	var got struct {
		Count int               `json:"count"`
		First map[string]string `json:"first"`
		CSV   string            `json:"csv"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output %q: %v", out, err)
	}
	if got.Count != 2 || got.First["a"] != "1" || got.First["b"] != "2" {
		t.Errorf("parse wrong: %+v", got)
	}
	if got.CSV != "1,2\n3,\"x,y\"" {
		t.Errorf("format wrong: %q", got.CSV)
	}
}

func TestStdlibDateISOWeek(t *testing.T) {
	h := stdlibHost(t)
	src := `import { isoWeek, formatISODate, parseDate } from "nine:date";
	export default ({ date }) => {
	  const d = parseDate(date);
	  return { week: isoWeek(d), iso: formatISODate(d) };
	};`
	// 2026-01-01 is a Thursday → ISO week 1.
	out := loadGen(t, h, "date_tool", src, `{"date":"2026-01-01T00:00:00Z"}`)
	var got struct {
		Week int    `json:"week"`
		ISO  string `json:"iso"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output %q: %v", out, err)
	}
	if got.Week != 1 || got.ISO != "2026-01-01" {
		t.Errorf("date wrong: %+v", got)
	}
}

func TestStdlibDiffLines(t *testing.T) {
	h := stdlibHost(t)
	src := `import { unified } from "nine:diff";
	export default ({ a, b }) => ({ u: unified(a, b) });`
	out := loadGen(t, h, "diff_tool", src, `{"a":"one\ntwo\nthree","b":"one\n2\nthree"}`)
	var got struct {
		U string `json:"u"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output %q: %v", out, err)
	}
	want := " one\n-two\n+2\n three"
	if got.U != want {
		t.Errorf("diff wrong:\n got %q\nwant %q", got.U, want)
	}
}

// A generated tool may import nine:* and nothing else: a bare npm specifier with
// no deps pipeline (§4.4) must fail, not resolve to anything.
func TestStdlibRejectsUnknownImport(t *testing.T) {
	h := stdlibHost(t)
	h.LoadGenerated(context.Background(),
		[]Generated{{Name: "bad", Source: `import _ from "lodash-es"; export default () => 1;`}}, nil)
	if h.Get("bad") == nil {
		t.Fatal("tool did not load")
	}
	// Loads fine (a js tool is not compiled at load), but the unresolved import
	// fails at call time.
	if _, err := h.Call(context.Background(), "bad", json.RawMessage(`{}`)); err == nil {
		t.Fatal("importing an npm package resolved with no deps pipeline")
	}
}

// nine:html is a tokenizer, not a tree builder, and these are the cases that
// distinguish a working tokenizer from a regex that looks like one. Each runs
// under the real committed blob.
func TestStdlibHTMLTextExtraction(t *testing.T) {
	h := stdlibHost(t)
	src := `import { textOf } from "nine:html";
	         export default ({ html }) => ({ text: textOf(html) });`

	cases := []struct {
		name    string
		html    string
		want    []string
		notWant []string
	}{
		{
			// Inside <script>, "<" does not open a tag. Treating it as one is how
			// a naive stripper swallows the rest of a page.
			name:    "raw text is not markup",
			html:    `<p>before</p><script>if (a<b) { f("</p>") }</script><p>after</p>`,
			want:    []string{"before", "after"},
			notWant: []string{"a<b", "f("},
		},
		{
			name:    "style is dropped",
			html:    `<style>.x{content:"<p>"}</style><p>visible</p>`,
			want:    []string{"visible"},
			notWant: []string{"content", "{"},
		},
		{
			// A quoted attribute value may contain ">"; the tag has not ended.
			name:    "gt inside a quoted attribute",
			html:    `<a title="a > b">link</a> tail`,
			want:    []string{"link", "tail"},
			notWant: []string{"a > b"},
		},
		{
			name:    "comments are not text",
			html:    `<p>keep</p><!-- <p>drop</p> -->`,
			want:    []string{"keep"},
			notWant: []string{"drop"},
		},
		{
			name:    "doctype is skipped",
			html:    `<!DOCTYPE html><p>body</p>`,
			want:    []string{"body"},
			notWant: []string{"DOCTYPE"},
		},
		{
			name: "entities decode",
			html: `<p>a &amp; b &lt;c&gt; &#39;d&#39;</p>`,
			want: []string{"a & b <c> 'd'"},
		},
		{
			name:    "unclosed tags do not eat the document",
			html:    `<div><p>one<p>two<div>three`,
			want:    []string{"one", "two", "three"},
			notWant: []string{"<"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args, err := json.Marshal(map[string]string{"html": tc.html})
			if err != nil {
				t.Fatal(err)
			}
			out := loadGen(t, h, "html_"+t.Name(), src, string(args))
			// Assert against the extracted text, not the JSON envelope: the
			// envelope's own braces and quotes would match a notWant probe.
			var got struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatalf("unmarshal %q: %v", out, err)
			}
			for _, w := range tc.want {
				if !strings.Contains(got.Text, w) {
					t.Errorf("text missing %q: %q", w, got.Text)
				}
			}
			for _, w := range tc.notWant {
				if strings.Contains(got.Text, w) {
					t.Errorf("text leaked %q: %q", w, got.Text)
				}
			}
		})
	}
}

// findByClass is what a scraping tool needs: elements by class token, in
// document order, with their text and attributes.
func TestStdlibHTMLFindByClass(t *testing.T) {
	h := stdlibHost(t)
	src := `import { findByClass } from "nine:html";
	        export default ({ html, cls }) => ({ hits: findByClass(html, cls) });`

	page := `<div>
	   <a class="result__a js-x" href="/l/?uddg=one"><b>Ti</b>tle One</a>
	   <div class="result__snippet">Snippet one</div>
	   <a class="result__anchor" href="/no">not a match</a>
	   <a class="result__a" href="/two">Title Two</a>
	 </div>`

	args, _ := json.Marshal(map[string]string{"html": page, "cls": "result__a"})
	out := loadGen(t, h, "html_class", src, string(args))

	// Nested markup inside the element still yields its text.
	if !strings.Contains(out, "Title One") || !strings.Contains(out, "Title Two") {
		t.Errorf("missing a matched element:\n%s", out)
	}
	// The href comes back so a scraper can follow it.
	if !strings.Contains(out, "uddg=one") {
		t.Errorf("attributes not returned:\n%s", out)
	}
	// A class token match, not a prefix match: result__anchor is not result__a.
	if strings.Contains(out, "not a match") {
		t.Errorf("prefix class matched:\n%s", out)
	}
}

// A call ships the modules its source can name, not all eight. The saving is the
// point — the whole library is 30 KB of JSON on every call — but the property
// under test is that narrowing never costs a tool an import it uses.
func TestACallShipsOnlyTheModulesItsSourceCanName(t *testing.T) {
	h := stdlibHost(t)
	const src = `
import { parse } from "nine:csv";
export default () => String(parse("a,b\n1,2").length);
`
	if out := loadGen(t, h, "narrow", src, `{}`); out != "2" {
		t.Fatalf("output = %q, want the tool's own import to still resolve", out)
	}

	got := h.Get("narrow").modules
	if _, ok := got["nine:csv"]; !ok {
		t.Error("the module the tool imports was not shipped")
	}
	if _, ok := got["nine:html"]; ok {
		t.Error("nine:html was shipped to a tool that never names it")
	}
	// The allowlist is unchanged: what a tool *may* import is still the stdlib.
	if len(h.Get("narrow").Imports()) != len(stdlibSpecifiers) {
		t.Errorf("Imports() = %v, want the whole allowlist", h.Get("narrow").Imports())
	}
}

// A dynamic import of a literal is a legitimate way to reach a module, so the
// scan must see it — it is not an import statement and a parser-shaped check
// would have missed it.
func TestADynamicImportOfALiteralStillResolves(t *testing.T) {
	h := stdlibHost(t)
	const src = `
export default async () => {
  const { parse } = await import("nine:csv");
  return String(parse("a\n1").length);
};
`
	if out := loadGen(t, h, "dynamic", src, `{}`); out != "2" {
		t.Errorf("output = %q, want a dynamically imported module to resolve", out)
	}
}

// A specifier the source never spells cannot be found by a substring scan, so
// that shape gets the whole library rather than a resolver error its author
// would have no way to read.
func TestAComputedImportFallsBackToTheWholeLibrary(t *testing.T) {
	h := stdlibHost(t)
	const src = `
export default async (args) => {
  const { parse } = await import("nine:" + args.mod);
  return String(parse("a\n1").length);
};
`
	if out := loadGen(t, h, "computed", src, `{"mod":"csv"}`); out != "2" {
		t.Errorf("output = %q, want a computed specifier to still resolve", out)
	}
	if got := len(h.Get("computed").modules); got != len(stdlibSpecifiers) {
		t.Errorf("shipped %d modules, want the whole library", got)
	}
}
