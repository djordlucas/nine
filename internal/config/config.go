package config

import "github.com/BurntSushi/toml"

type Config struct {
	LLM        LLMConfig        `toml:"llm"`
	Daemon     DaemonConfig     `toml:"daemon"`
	Plugins    PluginsConfig    `toml:"plugins"`
	Memory     MemoryConfig     `toml:"memory"`
	Embeddings EmbeddingsConfig `toml:"embeddings"`
	UI         UIConfig         `toml:"ui"`
	Workspace  WorkspaceConfig  `toml:"workspace"`
	Skills     SkillsConfig     `toml:"skills"`
	HITL       HITLConfig       `toml:"hitl"`
	Planning   PlanningConfig   `toml:"planning"`
	Roles      RolesConfig      `toml:"roles"`
	Agents     []AgentConfig    `toml:"agent"`
}

// PlanningConfig controls the plan-before-execute policy
// (docs/thinking-and-planning.md). Config sets the session default; PlanMode
// has a live equivalent via the set_plan_mode command.
type PlanningConfig struct {
	// PlanMode gates the reasoning policy: off | plan-only | always.
	// Empty defaults to plan-only.
	PlanMode string `toml:"plan_mode"`
	// PlanApproval gates the interactive plan-approval checkpoint:
	// off | on | on-risky. Empty defaults to on-risky (prompt only when the plan
	// intends a require_approval tool).
	PlanApproval string `toml:"plan_approval"`
}

const (
	PlanApprovalModeOnRisky = "on-risky"
	PlanApprovalModeOn      = "on"
	PlanApprovalModeOff     = "off"
	PlanModeOff             = "off"
	PlanModePlanOnly        = "plan-only"
	PlanModeAlways          = "always"
)

// PlanApprovalMode returns the plan-approval mode, defaulting to "on-risky"
func (p PlanningConfig) PlanApprovalMode() string {
	if p.PlanApproval == "" {
		return PlanApprovalModeOnRisky
	}
	return p.PlanApproval
}

// Mode returns the plan mode, defaulting to "plan-only".
func (p PlanningConfig) Mode() string {
	if p.PlanMode == "" {
		return PlanModePlanOnly
	}
	return p.PlanMode
}

// AgentConfig declares a pre-defined long-running agent — a goal seeded at boot
// and run under the pursue shell with a narrowed role (docs/predefined-agents.md).
// Interval and Schedule are mutually exclusive; when neither is set the agent
// runs on the default pursue idle interval.
type AgentConfig struct {
	ID          string `toml:"id"`          // stable goal ID; reconciliation keys on it
	Description string `toml:"description"` // the standing intention the agent pursues
	Role        string `toml:"role"`        // work-tool/persona role; default "monitor"
	Delegates   bool   `toml:"delegates"`   // may spawn sub-agents; default false
	Interval    string `toml:"interval"`    // idle cadence (Go duration) — XOR Schedule
	Schedule    string `toml:"schedule"`    // cron expression — XOR Interval
}

// RolesConfig controls worker-role resolution for delegation (docs/roles.md §11).
type RolesConfig struct {
	DefaultLeaf        string `toml:"default_leaf"`         // role used when a delegation names none; default "executor"
	MaxDelegationDepth int    `toml:"max_delegation_depth"` // depthGuard seed; default 2
}

// HITLConfig controls human-in-the-loop behavior for interactive sessions.
type HITLConfig struct {
	TimeoutSeconds  int      `toml:"timeout_seconds"`  // how long ask_human waits; default 300 (5 min)
	RequireApproval []string `toml:"require_approval"` // tool names gated behind human approval
}

type WorkspaceConfig struct {
	Root string `toml:"root"`
}

// SkillsConfig points at the operator's own skills and roles
// (spec/contracts/skills.md R-SKILL.2). Skills are read from UserDir at boot
// only; there is no watcher.
type SkillsConfig struct {
	// UserDir holds operator-authored skills as `*.md`, with role skills under
	// `roles/`, mirroring the built-in layout. Empty or absent disables user
	// skills entirely. The container overrides it with NINE_SKILLS_USER_DIR.
	UserDir string `toml:"user_dir"`
}

type UIConfig struct {
	Theme       string `toml:"theme"`        // "light" (default) or "dark"
	ShowContext *bool  `toml:"show_context"` // nil or true = show context bar; false = hide
}

type LLMConfig struct {
	Provider       string `toml:"provider"`
	Model          string `toml:"model"`
	APIKey         string `toml:"api_key"`
	Endpoint       string `toml:"endpoint"`
	ContextBudget  int    `toml:"context_budget"`
	MaxConcurrent  int    `toml:"max_concurrent"`
	NumCtx         int    `toml:"num_ctx"`         // Ollama only: model context window size
	TimeoutSeconds int    `toml:"timeout_seconds"` // HTTP timeout for LLM calls; 0 = no timeout

	// Thinking surfaces the model's extended-thinking reasoning as a live trace in
	// the TUI (Ollama only for now: sends think:true and drops /no_think). A *bool
	// so an unset value defaults to on while an explicit false disables it. The
	// model must advertise the "thinking" capability.
	Thinking *bool `toml:"thinking"`
}

// ThinkingEnabled reports whether extended-thinking surfacing is enabled,
// defaulting to true when the key is unset.
func (l LLMConfig) ThinkingEnabled() bool {
	return l.Thinking == nil || *l.Thinking
}

type DaemonConfig struct {
	SocketPath         string `toml:"socket_path"`
	TaskTimeoutSeconds int    `toml:"task_timeout_seconds"` // default 1800 (30 min)
	MaxGoalSessions    int    `toml:"max_goal_sessions"`    // default runtime.DefaultMaxGoalSessions when <= 0

	// StandingAgentsAuthoritative treats the [[agent]] list as the full desired
	// state (docs/predefined-agents.md §7 v3). When true, a config-origin goal no
	// longer listed in nine.toml is archived and its session stopped on boot.
	// Default false: removing an entry just stops reconciling it, leaving the
	// goal for the operator to archive manually. Never touches
	// conversation-created goals.
	StandingAgentsAuthoritative bool `toml:"standing_agents_authoritative"`

	// EventRetentionTurns bounds the session_events journal per agent: the number
	// of most-recent turns kept at boot (docs/event-log.md v4). 0 → default
	// (DefaultEventRetentionTurns); negative → keep all turns.
	EventRetentionTurns int `toml:"event_retention_turns"`
	// EventRetentionDays caps the age of any journaled event in days. 0 → no age
	// limit (turn count is the only bound).
	EventRetentionDays int `toml:"event_retention_days"`

	// RelatedSessionsIndex enables the out-of-band subscriber that links
	// topically-similar sessions and the context-builder surfacing that pulls
	// from it (docs/reactive-events.md). On by default; set
	// `related_sessions_index = false` to disable. Requires an embedder — it is a
	// no-op (skipped with a warning) when embeddings are disabled. A *bool so an
	// unset value can default to on while an explicit false still disables it.
	RelatedSessionsIndex *bool `toml:"related_sessions_index"`
}

// RelatedSessionsIndexEnabled reports whether the related-session indexer and
// its context surfacing are enabled, defaulting to true when the key is unset.
func (d DaemonConfig) RelatedSessionsIndexEnabled() bool {
	return d.RelatedSessionsIndex == nil || *d.RelatedSessionsIndex
}

type PluginsConfig struct {
	Dir string `toml:"dir"`
	Bin string `toml:"bin"`
}

type MemoryConfig struct {
	Path        string `toml:"path"`         // deprecated: legacy SQLite file path, no longer used
	DatabaseURL string `toml:"database_url"` // PostgreSQL DSN (see Config.DatabaseURL)
}

type EmbeddingsConfig struct {
	Provider string `toml:"provider"`
	Model    string `toml:"model"`
	Endpoint string `toml:"endpoint"`
	APIKey   string `toml:"api_key"`
}

func Load(path string) (*Config, error) {
	var cfg Config
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}
