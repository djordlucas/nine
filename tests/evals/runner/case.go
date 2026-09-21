// Package runner loads, isolates, drives, and grades Nine eval cases as
// specified in docs/evals.md. A case is a YAML document (tests/evals/cases/*.yaml)
// describing fixtures, prompts, and tolerant assertions over a session's durable
// event journal and real side-effects. Cases run on one of two tracks: Track R
// (deterministic replay of a recorded journal, no live model) and Track L (a live
// model, graded over N runs). This file defines the case schema (§2) plus loading,
// defaulting, and validation.
package runner

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Track selects a case's execution mode (docs/evals.md §0).
type Track string

const (
	TrackReplay Track = "replay" // deterministic replay of a recorded journal
	TrackLive   Track = "live"   // a live model, graded over N runs
	TrackBoth   Track = "both"   // Track-L recording becomes the Track-R fixture
)

// Tier is a coarse difficulty/kind label used for filtering and CI gating.
type Tier string

const (
	TierSmoke      Tier = "smoke"
	TierBasic      Tier = "basic"
	TierMultiStep  Tier = "multi_step"
	TierDelegation Tier = "delegation"
	TierSafety     Tier = "safety"
	TierHITL       Tier = "hitl"
)

var validTiers = map[Tier]bool{
	TierSmoke: true, TierBasic: true, TierMultiStep: true,
	TierDelegation: true, TierSafety: true, TierHITL: true,
}

// Case is one eval case: a self-contained description of fixtures, a request, and
// the assertions that must all hold for it to pass. It is decoded straight from a
// single YAML document; every field except ID and Prompts has a sane zero-value
// default (see defaults()).
type Case struct {
	ID          string   `yaml:"id"`
	Description string   `yaml:"description"`
	Track       Track    `yaml:"track"`
	Tier        Tier     `yaml:"tier"`
	Tags        []string `yaml:"tags"`

	Setup   Setup    `yaml:"setup"`
	Prompts []string `yaml:"prompts"`
	Session Session  `yaml:"session"`

	// HumanAnswers are canned human replies matched in order to ask_human calls
	// when the session is interactive (docs/evals.md §2 human_answers).
	HumanAnswers []string `yaml:"human_answers"`

	Expect Expect `yaml:"expect"`

	// Live-run controls (Track L).
	// RequiresEnv names environment variables that must be set for this case to
	// mean anything — infrastructure the suite cannot provide itself, like a
	// browser for an MCP server to drive. A case missing one is reported as
	// skipped rather than run and failed, so `make eval-live` does not silently
	// start requiring a browser on every machine.
	RequiresEnv []string `yaml:"requires_env"`

	Runs          int    `yaml:"runs"`
	PassThreshold string `yaml:"pass_threshold"` // e.g. "2/3"
	TimeoutSecs   int    `yaml:"timeout_seconds"`

	Models Models `yaml:"models"`

	// path is the source file, retained for diagnostics; not a YAML field.
	path string `yaml:"-"`
}

// Setup are fixtures written into the isolated store and workspace before the run.
type Setup struct {
	Files map[string]string `yaml:"files"` // workspace path -> content
	// StoredFiles seeds the memory file store (the `files` table), keyed by
	// stored path. Needed because no agent tool writes that table any more —
	// file_store is retired (adr/file-namespaces.md) and the daemon is the only
	// writer — so a case exercising spill reading has to be handed one.
	StoredFiles map[string]string `yaml:"stored_files"`
	KV     map[string]string     `yaml:"kv"`     // pre-seeded key/value memory
	Skills map[string]SkillSetup `yaml:"skills"` // skill name -> skill
	Goals  []string              `yaml:"goals"`  // pre-seeded goal descriptions

	// MCPServers are MCP servers to bring up for this case, mirroring
	// [[mcp.server]] in nine.toml. They belong to setup rather than to session
	// config because they are part of the world the case needs to exist —
	// tools, not a knob.
	MCPServers []MCPServerSetup `yaml:"mcp_servers"`
}

// SkillSetup is one skill pre-seeded into a case's store.
//
// Description is not decoration: skill_search ranks the `skills` vector
// namespace by embedded *description*, so a seeded skill without one is
// invisible to semantic discovery. It used to be impossible to express — the
// field was a bare name->body map — which is why the only way to get a
// searchable catalog was to have the model write it, and that is what made
// skill-search unpassable (populating the index taught the model the answer's
// name, so it read by name instead of searching).
type SkillSetup struct {
	Description string   `yaml:"description"`
	Tags        []string `yaml:"tags"`
	Content     string   `yaml:"content"`
}

// MCPServerSetup declares one MCP server a case needs. It is the eval-side
// mirror of config.MCPServer; the harness starts one bridge per entry exactly as
// the daemon's startMCPServers does.
//
// Command and Args go through ExpandEnv, so a case can name a fixture the suite
// builds at run time (`${NINE_EVAL_MCP_FIXTURE}`) without hardcoding a temp
// path. An unset variable expands to empty and fails validation at start rather
// than spawning something surprising.
type MCPServerSetup struct {
	Name    string            `yaml:"name"`
	Command string            `yaml:"command"`
	Args    []string          `yaml:"args"`
	Env     map[string]string `yaml:"env"`
}

// Session configures the driven session (all optional).
type Session struct {
	Role        string         `yaml:"role"`
	Interactive bool           `yaml:"interactive"`
	Config      map[string]any `yaml:"config"` // per-case nine.toml overrides (dotted keys)
}

// Expect groups the three assertion families. A case passes when ALL present
// assertions hold (docs/evals.md §2).
type Expect struct {
	SideEffects SideEffects `yaml:"side_effects"`
	Trajectory  Trajectory  `yaml:"trajectory"`
	Answer      Answer      `yaml:"answer"`
}

// SideEffects assert on the isolated store/workspace after the run (strongest,
// model-independent — docs/evals.md §1).
type SideEffects struct {
	Files map[string]StringMatch `yaml:"files"`
	// StoredFiles asserts over the memory file store (the `files` table) rather
	// than the workspace, keyed by a path **prefix**: the assertion holds when
	// some stored file under that prefix matches. A prefix rather than an exact
	// path because spilled tool outputs get a random suffix
	// (spill/<agent>/<tool>-<rand>.txt) that a case cannot predict
	// (adr/tool-output-spill.md §6).
	StoredFiles   map[string]StringMatch `yaml:"stored_files"`
	KV            map[string]StringMatch `yaml:"kv"`
	Workflows     *WorkflowExpect        `yaml:"workflows"`
	Goals         *GoalExpect            `yaml:"goals"`
	Notifications *CountExpect           `yaml:"notifications"`
	Vectors       *VectorExpect          `yaml:"vectors"`
}

// StringMatch is a tolerant assertion over a file: at most one of Equals/Contains/
// Matches is set (validated). Empty matches nothing meaningful and is rejected.
type StringMatch struct {
	Equals   *string `yaml:"equals"`
	Contains string  `yaml:"contains"`
	Matches  string  `yaml:"matches"` // regexp
	// Absent asserts the file is not there at all. A delete case has nothing
	// else to assert on: the strong evidence that a file was removed is its
	// absence, and every content matcher needs a file to read first.
	Absent bool `yaml:"absent"`
}

// WorkflowExpect asserts over the workflows table.
type WorkflowExpect struct {
	Status   string `yaml:"status"`    // e.g. "done"
	MinSteps int    `yaml:"min_steps"` // minimum step count
}

// GoalExpect asserts a goal was created and (optionally) a pursue shell spawned.
type GoalExpect struct {
	Created       int  `yaml:"created"`
	PursueSpawned bool `yaml:"pursue_spawned"`
}

// CountExpect asserts a minimum count (notifications).
type CountExpect struct {
	Min int `yaml:"min"`
}

// VectorExpect asserts a minimum vector count in a namespace.
type VectorExpect struct {
	Namespace string `yaml:"namespace"`
	Min       int    `yaml:"min"`
}

// Trajectory asserts on the tool/turn shape of the run (docs/evals.md §1(2)).
type Trajectory struct {
	ToolsAllOf  []string `yaml:"tools_all_of"`
	ToolsAnyOf  []string `yaml:"tools_any_of"`
	ToolsNoneOf []string `yaml:"tools_none_of"`

	MaxTurns  *int  `yaml:"max_turns"`
	MinTurns  *int  `yaml:"min_turns"`
	NoStall   bool  `yaml:"no_stall"`
	GapReport *bool `yaml:"gap_report"`

	SubAgents  *SubAgentExpect   `yaml:"sub_agents"`
	LLMRequest *LLMRequestExpect `yaml:"llm_request"`

	// Spills asserts how many tool results exceeded the output cap and were
	// spilled to the file store — a `tool_end` event carrying a spill_path
	// (adr/tool-output-spill.md §4). Use it to prove a case really exercised
	// the large-output path instead of getting a conveniently small result.
	Spills *CountExpect `yaml:"spills"`
}

// SubAgentExpect asserts spawned sub-agent count, either exact (Count) or a range.
type SubAgentExpect struct {
	Count *int `yaml:"count"`
	Min   *int `yaml:"min"`
	Max   *int `yaml:"max"`
}

// LLMRequestExpect asserts over the assembled prompts sent to the model.
type LLMRequestExpect struct {
	SystemContains       []string `yaml:"system_contains"`
	SystemNotContains    []string `yaml:"system_not_contains"`
	ToolAdvertisedNoneOf []string `yaml:"tool_advertised_none_of"`
}

// Answer asserts on the final free text (tolerant only — docs/evals.md §1(3)).
type Answer struct {
	Contains    []string `yaml:"contains"`     // all present (case-insensitive)
	Matches     string   `yaml:"matches"`      // regexp
	NotContains []string `yaml:"not_contains"` // none present
	Judge       *Judge   `yaml:"judge"`
}

// Judge configures an optional LLM-as-judge over the answer (docs/evals.md §7).
type Judge struct {
	Rubric    string  `yaml:"rubric"`
	Model     string  `yaml:"model"`
	PassScore float64 `yaml:"pass_score"`
}

// Models constrains applicability across the model matrix (docs/evals.md §6).
type Models struct {
	Include              []string `yaml:"include"`
	ExpectedPassMinClass string   `yaml:"expected_pass_min_class"` // nano|small|medium|large
}

// Path returns the source file the case was loaded from.
func (c *Case) Path() string { return c.path }

// LoadCase reads and validates a single case YAML document.
func LoadCase(path string) (*Case, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Case
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true) // reject typos/unknown fields — cases are generated, catch drift early
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	c.path = path
	c.defaults()
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

// LoadCases loads every *.yaml case under dir, sorted by id for deterministic
// ordering, and fails on the first invalid document or duplicate id.
func LoadCases(dir string) ([]*Case, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var cases []*Case
	seen := map[string]string{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml") {
			continue
		}
		c, err := LoadCase(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		if prev, dup := seen[c.ID]; dup {
			return nil, fmt.Errorf("duplicate case id %q in %s and %s", c.ID, prev, name)
		}
		seen[c.ID] = name
		cases = append(cases, c)
	}
	sort.Slice(cases, func(i, j int) bool { return cases[i].ID < cases[j].ID })
	return cases, nil
}

// defaults fills unset optional fields with their documented defaults
// (docs/evals.md §2: "Every field except id and prompts has a sane default").
func (c *Case) defaults() {
	if c.Track == "" {
		c.Track = TrackLive
	}
	if c.Tier == "" {
		c.Tier = TierBasic
	}
	if c.Session.Role == "" {
		c.Session.Role = "executor"
	}
	if c.Runs == 0 {
		c.Runs = 3
	}
	if c.PassThreshold == "" {
		c.PassThreshold = "1/1"
	}
	if c.TimeoutSecs == 0 {
		// Generous by default: a multi-turn, tool-using run on a local model
		// (slower per token than a hosted API) needs headroom, or a run trips
		// the per-run deadline mid-turn and reads as a failure rather than a
		// genuine pass/fail. Cases that are known-fast can override downward.
		c.TimeoutSecs = 300
	}
	if c.Models.ExpectedPassMinClass == "" {
		c.Models.ExpectedPassMinClass = "medium"
	}
}

// validate enforces the required fields and internal consistency the schema
// promises, so a malformed case fails fast at load rather than mid-run.
func (c *Case) validate() error {
	if c.ID == "" {
		return fmt.Errorf("id is required")
	}
	if !isKebab(c.ID) {
		return fmt.Errorf("id %q must be kebab-case", c.ID)
	}
	if len(c.Prompts) == 0 {
		return fmt.Errorf("at least one prompt is required")
	}
	switch c.Track {
	case TrackReplay, TrackLive, TrackBoth:
	default:
		return fmt.Errorf("invalid track %q (want replay|live|both)", c.Track)
	}
	if !validTiers[c.Tier] {
		return fmt.Errorf("invalid tier %q", c.Tier)
	}
	switch c.Models.ExpectedPassMinClass {
	case "nano", "small", "medium", "large":
	default:
		return fmt.Errorf("invalid expected_pass_min_class %q", c.Models.ExpectedPassMinClass)
	}
	if _, _, err := parseThreshold(c.PassThreshold); err != nil {
		return err
	}
	if c.Session.Interactive && len(c.HumanAnswers) == 0 {
		return fmt.Errorf("interactive session requires human_answers")
	}
	// Caught at load, not at spawn: a nameless or command-less MCP server would
	// otherwise surface as a missing tool mid-run, which reads like a model
	// failure rather than a broken case.
	seenMCP := map[string]bool{}
	for i, srv := range c.Setup.MCPServers {
		switch {
		case srv.Name == "":
			return fmt.Errorf("setup.mcp_servers[%d]: name is required (it prefixes the server's tools)", i)
		case srv.Command == "":
			return fmt.Errorf("setup.mcp_servers[%q]: command is required", srv.Name)
		case seenMCP[srv.Name]:
			return fmt.Errorf("setup.mcp_servers: duplicate name %q", srv.Name)
		}
		seenMCP[srv.Name] = true
	}
	if !c.Expect.hasAny() {
		return fmt.Errorf("at least one assertion is required")
	}
	for path, m := range c.Expect.SideEffects.Files {
		if err := m.validate(); err != nil {
			return fmt.Errorf("side_effects.files[%s]: %w", path, err)
		}
	}
	for key, m := range c.Expect.SideEffects.KV {
		if err := m.validate(); err != nil {
			return fmt.Errorf("side_effects.kv[%s]: %w", key, err)
		}
	}
	for prefix, m := range c.Expect.SideEffects.StoredFiles {
		if err := m.validate(); err != nil {
			return fmt.Errorf("side_effects.stored_files[%s]: %w", prefix, err)
		}
	}
	if j := c.Expect.Answer.Judge; j != nil {
		if j.Rubric == "" || j.Model == "" {
			return fmt.Errorf("answer.judge requires rubric and model")
		}
	}
	return nil
}

// validate rejects a StringMatch that sets more than one predicate or none.
func (m StringMatch) validate() error {
	n := 0
	if m.Equals != nil {
		n++
	}
	if m.Contains != "" {
		n++
	}
	if m.Matches != "" {
		n++
	}
	if m.Absent {
		n++
	}
	if n == 0 {
		return fmt.Errorf("must set one of equals|contains|matches|absent")
	}
	if n > 1 {
		return fmt.Errorf("set only one of equals|contains|matches|absent")
	}
	return nil
}

// hasAny reports whether the case declares at least one assertion.
func (e Expect) hasAny() bool {
	se := e.SideEffects
	if len(se.Files) > 0 || len(se.StoredFiles) > 0 || len(se.KV) > 0 || se.Workflows != nil ||
		se.Goals != nil || se.Notifications != nil || se.Vectors != nil {
		return true
	}
	t := e.Trajectory
	if len(t.ToolsAllOf) > 0 || len(t.ToolsAnyOf) > 0 || len(t.ToolsNoneOf) > 0 ||
		t.MaxTurns != nil || t.MinTurns != nil || t.NoStall || t.GapReport != nil ||
		t.SubAgents != nil || t.LLMRequest != nil || t.Spills != nil {
		return true
	}
	a := e.Answer
	return len(a.Contains) > 0 || a.Matches != "" || len(a.NotContains) > 0 || a.Judge != nil
}

// parseThreshold parses "k/n" into (k, n). "all"/"any" are accepted shorthands.
func parseThreshold(s string) (k, n int, err error) {
	switch s {
	case "all":
		return -1, -1, nil // resolved against runs at grade time
	case "any":
		return 1, 0, nil
	}
	parts := strings.SplitN(s, "/", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("invalid pass_threshold %q (want k/n)", s)
	}
	if _, e := fmt.Sscanf(parts[0], "%d", &k); e != nil {
		return 0, 0, fmt.Errorf("invalid pass_threshold %q: %v", s, e)
	}
	if _, e := fmt.Sscanf(parts[1], "%d", &n); e != nil {
		return 0, 0, fmt.Errorf("invalid pass_threshold %q: %v", s, e)
	}
	if k < 0 || n <= 0 || k > n {
		return 0, 0, fmt.Errorf("invalid pass_threshold %q", s)
	}
	return k, n, nil
}

// isKebab reports whether s is lowercase kebab-case (a-z, 0-9, '-').
func isKebab(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return s[0] != '-' && s[len(s)-1] != '-'
}

// MissingEnv returns the RequiresEnv entries that are unset or empty. A case
// with any missing is skipped: it needs infrastructure this machine does not
// have, which is not the same as the model failing it.
func (c *Case) MissingEnv() []string {
	var missing []string
	for _, key := range c.RequiresEnv {
		if os.Getenv(key) == "" {
			missing = append(missing, key)
		}
	}
	return missing
}
