package toolvm

import (
	"context"
	"encoding/json"
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
