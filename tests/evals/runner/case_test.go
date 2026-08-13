package runner

import (
	"os"
	"path/filepath"
	"testing"
)

// writeCase writes body to a temp .yaml file and returns its path.
func writeCase(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "case.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadCase_MinimalDefaults(t *testing.T) {
	p := writeCase(t, `
id: memory-roundtrip
prompts:
  - "Remember my prod DB host is db.prod.example.com"
expect:
  answer:
    contains: ["db.prod.example.com"]
`)
	c, err := LoadCase(p)
	if err != nil {
		t.Fatalf("LoadCase: %v", err)
	}
	if c.Track != TrackLive {
		t.Errorf("default track = %q, want live", c.Track)
	}
	if c.Tier != TierBasic {
		t.Errorf("default tier = %q, want basic", c.Tier)
	}
	if c.Session.Role != "executor" {
		t.Errorf("default role = %q, want executor", c.Session.Role)
	}
	if c.Runs != 3 || c.TimeoutSecs != 300 {
		t.Errorf("defaults runs=%d timeout=%d", c.Runs, c.TimeoutSecs)
	}
	if c.Models.ExpectedPassMinClass != "medium" {
		t.Errorf("default class = %q", c.Models.ExpectedPassMinClass)
	}
}

func TestLoadCase_FullRoundtrip(t *testing.T) {
	p := writeCase(t, `
id: memory-roundtrip
description: "Store a fact, then recall it."
track: both
tier: basic
tags: [memory, kv]
setup:
  files:
    /work/data.txt: "alpha\nbeta\n"
  kv:
    self/region: "us-west-2"
prompts:
  - "Remember my prod DB host is db.prod.example.com"
  - "What is my prod DB host?"
session:
  role: executor
  config:
    related_sessions_index: false
expect:
  side_effects:
    kv:
      self/prod_db: { equals: "db.prod.example.com" }
  trajectory:
    tools_all_of: [memory_set]
    tools_none_of: [shell]
    max_turns: 2
    no_stall: true
  answer:
    contains: ["db.prod.example.com"]
runs: 3
pass_threshold: "2/3"
models:
  include: [qwen3.5:4b, qwen3.5:9b]
  expected_pass_min_class: small
`)
	c, err := LoadCase(p)
	if err != nil {
		t.Fatalf("LoadCase: %v", err)
	}
	if c.Track != TrackBoth {
		t.Errorf("track = %q", c.Track)
	}
	if got := c.Setup.KV["self/region"]; got != "us-west-2" {
		t.Errorf("setup kv = %q", got)
	}
	if c.Expect.SideEffects.KV["self/prod_db"].Equals == nil ||
		*c.Expect.SideEffects.KV["self/prod_db"].Equals != "db.prod.example.com" {
		t.Errorf("kv equals not decoded")
	}
	if c.Expect.Trajectory.MaxTurns == nil || *c.Expect.Trajectory.MaxTurns != 2 {
		t.Errorf("max_turns not decoded")
	}
	if len(c.Prompts) != 2 {
		t.Errorf("prompts = %d", len(c.Prompts))
	}
}

func TestLoadCase_Invalid(t *testing.T) {
	cases := map[string]string{
		"no id": `
prompts: ["hi"]
expect: { answer: { contains: ["x"] } }
`,
		"no prompts": `
id: x
expect: { answer: { contains: ["x"] } }
`,
		"no assertion": `
id: x
prompts: ["hi"]
`,
		"bad track": `
id: x
track: nope
prompts: ["hi"]
expect: { answer: { contains: ["x"] } }
`,
		"non-kebab id": `
id: Bad_ID
prompts: ["hi"]
expect: { answer: { contains: ["x"] } }
`,
		"double string-match": `
id: x
prompts: ["hi"]
expect:
  side_effects:
    kv:
      k: { equals: "a", contains: "b" }
`,
		"unknown field": `
id: x
prompts: ["hi"]
bogus_field: true
expect: { answer: { contains: ["x"] } }
`,
		"interactive without answers": `
id: x
prompts: ["hi"]
session: { interactive: true }
expect: { answer: { contains: ["x"] } }
`,
		"bad threshold": `
id: x
prompts: ["hi"]
pass_threshold: "5/3"
expect: { answer: { contains: ["x"] } }
`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadCase(writeCase(t, body)); err == nil {
				t.Errorf("expected error for %q", name)
			}
		})
	}
}

func TestLoadCases_DuplicateID(t *testing.T) {
	dir := t.TempDir()
	body := "id: dup\nprompts: [\"hi\"]\nexpect: { answer: { contains: [\"x\"] } }\n"
	os.WriteFile(filepath.Join(dir, "a.yaml"), []byte(body), 0o644)
	os.WriteFile(filepath.Join(dir, "b.yaml"), []byte(body), 0o644)
	if _, err := LoadCases(dir); err == nil {
		t.Error("expected duplicate-id error")
	}
}

func TestParseThreshold(t *testing.T) {
	if _, _, err := parseThreshold("2/3"); err != nil {
		t.Errorf("2/3: %v", err)
	}
	if _, _, err := parseThreshold("4/3"); err == nil {
		t.Error("4/3 should fail")
	}
	if _, _, err := parseThreshold("all"); err != nil {
		t.Errorf("all: %v", err)
	}
}
