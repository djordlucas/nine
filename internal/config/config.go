package config

import (
	"fmt"
	"log/slog"
	"nine/internal/cron"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"nine/internal/logsafe"
)

type Config struct {
	// SchemaVersion is the shape of this file, not the version of Nine that
	// wrote it (docs/versioning.md §Config schema). It exists so an
	// incompatible change to the config layout can be detected rather than
	// silently half-applied, and it is deliberately in place *before* the
	// first such change: a version added afterwards cannot tell an old file
	// from a new one.
	//
	// Absent means 1. Every file written before this field existed is a
	// schema-1 file, so omitting it stays correct rather than becoming a
	// missing-value error on every existing deployment.
	SchemaVersion int `toml:"schema_version"`

	LLM        LLMConfig        `toml:"llm"`
	Daemon     DaemonConfig     `toml:"daemon"`
	Plugins    PluginsConfig    `toml:"plugins"`
	Memory     MemoryConfig     `toml:"memory"`
	Embeddings EmbeddingsConfig `toml:"embeddings"`
	Workspace  WorkspaceConfig  `toml:"workspace"`
	Skills     SkillsConfig     `toml:"skills"`
	HITL       HITLConfig       `toml:"hitl"`
	Planning   PlanningConfig   `toml:"planning"`
	Roles      RolesConfig      `toml:"roles"`
	// Process are the `[[process]]` blocks: the processes this file declares
	// (adr/process-sessions.md), and Processes their shared limits.
	Process   []ProcessConfig `toml:"process"`
	Processes ProcessesConfig `toml:"processes"`
	// Board is the `[board]` table: limits for the orchestrator board
	// (adr/orchestrator-board.md §2.4).
	Board BoardConfig  `toml:"board"`
	Tools ToolsConfig `toml:"tools"`
	MCP       MCPConfig       `toml:"mcp"`
	// Context is the `[context]` table: how tools reach the model's context
	// (adr/deferred-tool-schemas.md).
	Context ContextConfig `toml:"context"`
	// API is the `[api]` table: HTTP API server configuration for remote access
	// to Nine's functionality (spec/contracts/api.md). The API runs as a separate
	// process that communicates with the daemon via Unix socket.
	API APIConfig `toml:"api"`

	// Bootstrap is the `[bootstrap]` table: a declarative self-model seeded on
	// first boot (adr/personality-pattern.md §4). It exists so a packaged
	// instance starts knowing who it is, rather than starting as generic Nine and
	// being told in its first conversation.
	Bootstrap BootstrapConfig `toml:"bootstrap"`

	// Plugin holds per-plugin `[plugin.<name>]` tables (singular), sibling to the
	// plural `[plugins]` subsystem table above — the same split `[agent]` and
	// `[[agent]]` already use. It carries operator settings passed through to a
	// plugin as environment variables, so an operator can configure a plugin Nine
	// has never heard of without a rebuild (docs/plugin-capabilities.md §3).
	Plugin map[string]PluginEntry `toml:"plugin"`

	// Tool holds per-tool `[tool.<name>]` tables (singular), sibling to the plural
}

// PluginEntry is one `[plugin.<name>]` table. Its Settings are schema-less on
// Nine's side: keys are copied through verbatim as environment-variable names so
// they match what a plugin's own README documents, and values are TOML scalars,
// stringified. Both are validated at load (Config.Validate), so a bad key or a
// non-scalar value is a config error rather than a surprise at spawn.
type PluginEntry struct {
	// PersistCache keeps the plugin's cache directory across restarts
	// (docs/plugin-capabilities.md §4). Default false. Declared here so the table
	// shape is stable; it is wired in the cache-dir phase.
	PersistCache bool `toml:"persist_cache"`

	// Settings are operator-supplied environment variables handed to the plugin
	// process at spawn, layered on top of Nine's built-in defaults (operator
	// values win on a duplicate key). Reserved keys and Nine's own spawn vars
	// cannot be set here.
	Settings map[string]any `toml:"settings"`
}

// Tool exposition modes for [context] tool_exposition.
const (
	ToolExpositionRanked   = "ranked"
	ToolExpositionDeferred = "deferred"
	ToolExpositionHybrid   = "hybrid"
)

// ContextConfig is `[context]`.
type ContextConfig struct {
	// ToolExposition is "ranked" (the default): every pinned tool and the
	// best-ranked rest are advertised with their schemas. "deferred" advertises
	// a small resident set and lists the other tools by name, for the model to
	// load with tool_search or call directly.
	ToolExposition string `toml:"tool_exposition"`
	// ResidentTools are tools a deferred loop keeps resident besides the
	// built-in set, by name; plugin and MCP tool names are allowed.
	ResidentTools []string `toml:"resident_tools"`
	// PromotedBudget bounds, in estimated tokens, the schemas a deferred loop
	// has loaded. 0 is 1500.
	PromotedBudget int `toml:"promoted_budget"`
	// HybridPicks is how many of the tools a hybrid loop would only index it
	// also advertises with their schemas, best-ranked against the turn's
	// request. 0 is 3. Needs an embedder; ignored unless hybrid.
	HybridPicks int `toml:"hybrid_picks"`
	// ToolTopN is how many tools a ranked loop advertises with their schemas
	// besides the pinned ones, best-ranked against the turn's request. 0 is 20.
	// Ignored unless ranked.
	ToolTopN int `toml:"tool_top_n"`
}

// ToolsConfig tunes the tool-dispatch boundary. It is `[tools]` rather than
// `[agent]` because `[[agent]]` is already the standing-agent table array.
// ToolsConfig is `[tools]`: the per-result output cap. The sandbox host and
// generated tier that used to live under this table are gone
// (adr/modular_nine.md §9.10).
type ToolsConfig struct {
	// MaxOutputTokens is the per-result output cap (adr/tool-output-spill.md).
	// A result over it is spilled to the memory file store and replaced by a
	// short preview naming the path. 0 (unset) keeps agent.DefaultMaxOutputTokens.
	MaxOutputTokens int `toml:"max_output_tokens"`
}

// ProcessConfig is one `[[process]]` block: a process this file declares
// (adr/process-sessions.md). This file owns its definition; the runtime owns
// whether it is running, so an edit adjusts what a process does without
// restarting one that was stopped.
//
// Whether it is live or slice comes from its tool: a live tool (pursue,
// reflect, or one whose manifest says live = true) runs until stopped and drives
// a session; any other is called on each trigger, as standing tools were.
type ProcessConfig struct {
	// Name is operator-chosen and stable; reconciliation keys on it.
	Name string `toml:"name"`
	// Tool is the tool that drives the process: a built-in, or an MCP plugin tool (`<server>__<tool>`).
	Tool string `toml:"tool"`
	// Every and Schedule are its clock, mutually exclusive: a duration ("10s")
	// or a 5-field cron expression. A process with neither wakes only on what
	// is sent or piped to it.
	Every    string `toml:"every"`
	Schedule string `toml:"schedule"`
	// Args is the tool's input, as a TOML table.
	Args map[string]any `toml:"args"`
	// Goal binds the process to a goal this file owns, created with this
	// description and named after the process: the goal's status decides
	// whether the process runs, and a goal the agent finished is never
	// resurrected. This is how a standing agent is declared.
	Goal string `toml:"goal"`
	// Role is the role its session runs under; for a goal process it defaults
	// to "monitor", read-only.
	Role string `toml:"role"`
	// Delegates lets its session spawn sub-agents.
	Delegates bool `toml:"delegates"`
	// Session attaches the process to another process's session instead of
	// owning one: its turns run there, with that session's history and role.
	Session string `toml:"session"`
	// ReportTo pipes what the process reports to that process's session.
	ReportTo string `toml:"report_to"`
	// Budget lowers [processes] budget for this process; it can never raise it.
	Budget BudgetConfig `toml:"budget"`
	// On lists the journal event types that trigger it (EventTypes); a live
	// process only (adr/process-sessions.md §7).
	On []string `toml:"on"`
	// OnFilter narrows which of those events reach it.
	OnFilter EventFilter `toml:"on_filter"`
	// EventContent delivers the events' content — tool input and output, a
	// turn's input and result — as well as their metadata.
	EventContent bool `toml:"event_content"`
	// Enabled defaults to true; false declares a process without starting it.
	Enabled *bool `toml:"enabled"`
}

// EventFilter narrows the journal events that trigger a process. Every field
// set must match.
type EventFilter struct {
	// Tool is a tool name, for tool_end events.
	Tool string `toml:"tool" json:"tool,omitempty"`
	// Sessions is "all" (the default), "conversations" or "processes".
	Sessions string `toml:"sessions" json:"sessions,omitempty"`
	// Session is one session id.
	Session string `toml:"session" json:"session,omitempty"`
}

// EventTypes are the journal event types a process may be triggered by. LLM
// requests and replies are left out: they carry every prompt, and a process
// reacts to what happened, not to how the model was asked.
var EventTypes = []string{
	"turn_start", "turn_end", "tool_end", "sub_agent_start", "sub_agent_end",
	"standing_started", "standing_stopped", "standing_failing", "standing_recovered",
	"standing_reported", "process_paused", "process_resumed", "process_deleted",
}

// ValidateEvents checks a process's event trigger: known types, a known
// sessions scope, and a live tool to run it.
func ValidateEvents(on []string, f EventFilter) error {
	for _, t := range on {
		if !slices.Contains(EventTypes, t) {
			return fmt.Errorf("on: %q is not an event a process can be triggered by; one of: %s",
				t, strings.Join(EventTypes, ", "))
		}
	}
	switch f.Sessions {
	case "", "all", "conversations", "processes":
	default:
		return fmt.Errorf("on_filter.sessions %q is not all, conversations or processes", f.Sessions)
	}
	return nil
}

// IsEnabled reports whether the block asks to run. Unset means yes.
func (c ProcessConfig) IsEnabled() bool { return c.Enabled == nil || *c.Enabled }

// ProcessesConfig is the `[processes]` table: limits shared by every process.
type ProcessesConfig struct {
	// MaxRunning is the one cap on processes running at once, live and slice
	// alike: it sizes the live pool, and a process Nine creates at the cap is
	// recorded and not started. 0 is the default, 14 (the ten goal sessions and
	// four standing tools the two caps it replaced allowed).
	MaxRunning int `toml:"max_running"`
	// Authoritative treats the [[process]] list as the full desired state: a
	// goal process no longer listed is removed and its goal archived at boot.
	// Never touches a goal a conversation created.
	Authoritative bool `toml:"authoritative"`
	// Budget is every process's default budget; a block may lower it.
	Budget BudgetConfig `toml:"budget"`
	// MaxDepth bounds lineage (adr/process-sessions.md §7): no process is
	// triggered by an event, or sent a pipe's message, at this depth or more.
	// 0 is the default, 2.
	MaxDepth int `toml:"max_depth"`
	// Database is the database's file (adr/process-sessions.md §8): processes
	// write it through nine:sql, conversations read it through sql_query.
	// Empty puts database.sqlite beside the store.
	Database string `toml:"database"`
	// DatabaseMaxMB bounds its size; 0 is 100.
	DatabaseMaxMB int `toml:"database_max_mb"`
	// StatementTimeout bounds one statement ("5s"); empty is 5s.
	StatementTimeout string `toml:"statement_timeout"`
	// ProcessRoles lists the roles a genesis-written process may run under.
	// 0 uses the default ["process"].
	ProcessRoles []string `toml:"process_roles"`
}

// BoardConfig is the `[board]` table: limits shared by the board's tasks
// (adr/orchestrator-board.md §2.4).
type BoardConfig struct {
	// MaxQueued caps one poster's queued+running tasks; a post beyond the cap
	// is refused. 0 is the default, 100.
	MaxQueued int `toml:"max_queued"`
}

// DefaultBoardMaxQueued is [board] max_queued when unset.
const DefaultBoardMaxQueued = 100

// MaxQueuedOrDefault returns [board] max_queued, defaulted.
func (c BoardConfig) MaxQueuedOrDefault() int {
	if c.MaxQueued <= 0 {
		return DefaultBoardMaxQueued
	}
	return c.MaxQueued
}

// DefaultMaxDepth is [processes] max_depth when unset: a process may react to
// another's work, not to a reaction to it.
const DefaultMaxDepth = 2

// StatementTimeoutOrDefault parses [processes] statement_timeout; empty is 5s.
func (c ProcessesConfig) StatementTimeoutOrDefault() (time.Duration, error) {
	if c.StatementTimeout == "" {
		return 5 * time.Second, nil
	}
	d, err := time.ParseDuration(c.StatementTimeout)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("[processes] statement_timeout %q: want a positive duration such as \"5s\"", c.StatementTimeout)
	}
	return d, nil
}

// MaxDepthOrDefault returns [processes] max_depth, defaulted.
func (c ProcessesConfig) MaxDepthOrDefault() int {
	if c.MaxDepth > 0 {
		return c.MaxDepth
	}
	return DefaultMaxDepth
}

// DefaultMaxRunning is [processes] max_running when unset.
const DefaultMaxRunning = 14

// BudgetConfig bounds the model turns of one process over a rolling day
// (adr/process-sessions.md §5): the turns its live program runs through
// turn(), and the tokens those turns spend. Zero means "not set here".
type BudgetConfig struct {
	TurnsPerDay  int `toml:"turns_per_day"`
	TokensPerDay int `toml:"tokens_per_day"`
}

// The default budget: generous enough that pursuing a goal never meets it, so
// what it stops is a loop that runs away.
const (
	DefaultBudgetTurnsPerDay  = 200
	DefaultBudgetTokensPerDay = 2_000_000
)

// BudgetOrDefault returns [processes] budget with each unset field defaulted.
func (c ProcessesConfig) BudgetOrDefault() BudgetConfig {
	b := c.Budget
	if b.TurnsPerDay <= 0 {
		b.TurnsPerDay = DefaultBudgetTurnsPerDay
	}
	if b.TokensPerDay <= 0 {
		b.TokensPerDay = DefaultBudgetTokensPerDay
	}
	return b
}

// Within returns the budget a block's own budget gives under ceiling: each
// field the block sets, else the ceiling's.
func (b BudgetConfig) Within(ceiling BudgetConfig) BudgetConfig {
	out := ceiling
	if b.TurnsPerDay > 0 {
		out.TurnsPerDay = min(b.TurnsPerDay, ceiling.TurnsPerDay)
	}
	if b.TokensPerDay > 0 {
		out.TokensPerDay = min(b.TokensPerDay, ceiling.TokensPerDay)
	}
	return out
}

// MaxRunningOrDefault returns [processes] max_running, defaulted.
func (c ProcessesConfig) MaxRunningOrDefault() int {
	if c.MaxRunning > 0 {
		return c.MaxRunning
	}
	return DefaultMaxRunning
}

// PlanningConfig controls the plan-before-execute policy
// (adr/thinking-and-planning.md). Config sets the session default; PlanMode
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

// RolesConfig controls worker-role resolution for delegation (adr/roles-design.md §11).
type RolesConfig struct {
	DefaultLeaf        string `toml:"default_leaf"`         // role used when a delegation names none; default "executor"
	MaxDelegationDepth int    `toml:"max_delegation_depth"` // depthGuard seed; default 2
}

// HITLConfig controls human-in-the-loop behavior for interactive sessions.
type HITLConfig struct {
	TimeoutSeconds  int      `toml:"timeout_seconds"`  // how long ask_human waits; default 300 (5 min)
	RequireApproval []string `toml:"require_approval"` // tool names gated behind human approval

	// GateSubAgents extends the RequireApproval gates to sub-agents spawned by
	// an interactive session, so delegation cannot be used to run a gated tool
	// unprompted. The prompt is routed to the owning session's stream and
	// attributed to the sub-agent. A *bool so an operator can switch it off
	// explicitly; nil (unset) means enabled. Sub-agents never get ask_human
	// either way — only the automatic gates (R-HITL.1).
	GateSubAgents *bool `toml:"gate_sub_agents"`
}

// GateSubAgentsEnabled reports whether approval gates extend into sub-agents,
// defaulting to true when unset.
func (h HITLConfig) GateSubAgentsEnabled() bool {
	return h.GateSubAgents == nil || *h.GateSubAgents
}

type WorkspaceConfig struct {
	Root string `toml:"root"`

	// TrashRetention is how long a deleted or overwritten file is kept under
	// .nine/trash/ before the sweep removes it, in days. Zero uses
	// DefaultTrashRetentionDays; a negative value disables the age sweep, which
	// leaves TrashMaxBytes as the only bound.
	TrashRetention int `toml:"trash_retention_days"`

	// TrashMaxBytes bounds the trash's total size, oldest entry removed first.
	// The trash lives inside the workspace — the operator's own disk — so an age
	// bound alone is not enough: a week of large deletions can outgrow a volume
	// long before anything expires. Zero uses DefaultTrashMaxBytes.
	TrashMaxBytes int64 `toml:"trash_max_bytes"`

	// ScanIntervalSeconds is how often the workspace is rescanned for files
	// changed outside Nine. Zero uses 60s.
	ScanIntervalSeconds int `toml:"scan_interval_seconds"`

	// IndexMaxFileBytes is the largest file whose text is indexed. The bound is
	// about churn rather than storage: FTS5 rewrites a document's whole posting
	// list when it changes, so a large file that grows costs its full size in
	// tokenization on every scan that sees it. Zero uses 8 MiB.
	IndexMaxFileBytes int64 `toml:"index_max_file_bytes"`

	// IndexMaxFiles bounds one scan. Zero uses 50,000.
	IndexMaxFiles int `toml:"index_max_files"`
}

// Trash defaults. Retention matches the spill window: both are debris from work
// that has moved on, and one number is easier to reason about than two.
const (
	DefaultTrashRetentionDays = 7
	DefaultTrashMaxBytes      = 1 << 30 // 1 GiB
)

// TrashRetentionDuration resolves the configured retention, distinguishing
// unset (the default applies) from a deliberate negative (no age sweep).
func (w WorkspaceConfig) TrashRetentionDuration() time.Duration {
	switch {
	case w.TrashRetention == 0:
		return DefaultTrashRetentionDays * 24 * time.Hour
	case w.TrashRetention < 0:
		return 0
	default:
		return time.Duration(w.TrashRetention) * 24 * time.Hour
	}
}

// ScanIntervalOrDefault resolves the rescan period.
func (w WorkspaceConfig) ScanIntervalOrDefault() time.Duration {
	if w.ScanIntervalSeconds <= 0 {
		return 60 * time.Second
	}
	return time.Duration(w.ScanIntervalSeconds) * time.Second
}

// IndexMaxFileBytesOrDefault resolves the per-file index ceiling (8 MiB).
func (w WorkspaceConfig) IndexMaxFileBytesOrDefault() int64 {
	if w.IndexMaxFileBytes <= 0 {
		return 8 << 20
	}
	return w.IndexMaxFileBytes
}

// IndexMaxFilesOrDefault resolves the per-scan file bound.
func (w WorkspaceConfig) IndexMaxFilesOrDefault() int {
	if w.IndexMaxFiles <= 0 {
		return 50_000
	}
	return w.IndexMaxFiles
}

// TrashSizeBound resolves the configured size ceiling; zero means the default.
func (w WorkspaceConfig) TrashSizeBound() int64 {
	if w.TrashMaxBytes <= 0 {
		return DefaultTrashMaxBytes
	}
	return w.TrashMaxBytes
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

type LLMConfig struct {
	Provider       string `toml:"provider"`
	Model          string `toml:"model"`
	Endpoint       string `toml:"endpoint"`
	APIKey         string `toml:"api_key"` // For Mistral, OpenAI-compatible APIs
	ContextBudget  int    `toml:"context_budget"`
	MaxConcurrent  int    `toml:"max_concurrent"`
	NumCtx         int    `toml:"num_ctx"`         // model context window size
	TimeoutSeconds int    `toml:"timeout_seconds"` // HTTP timeout for LLM calls; 0 = the adapter default
	// MaxTokens caps the tokens the model may generate in one reply of an agent
	// loop's call. 0 keeps DefaultMaxReplyTokens. A reply that reaches the cap
	// ends mid-text, so raise it for a provider and model that write long
	// outputs (code, documents); it is not the context window (num_ctx,
	// context_budget).
	MaxTokens int `toml:"max_tokens"`

	// Thinking surfaces the model's extended-thinking reasoning as a live trace in
	// clients (sends think:true to Ollama and drops /no_think). A *bool
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

	// SelfReflection controls the dedicated self-reflection session: a Go
	// duration for its cadence, or "off" to remove it. Empty uses the default.
	//
	// Removal is subtractive, not merely "do not create": an existing session is
	// deactivated so the resume pass stops reviving it. Otherwise turning
	// reflection off would only ever take effect on a machine that had never
	// run it.
	SelfReflection string `toml:"self_reflection"`

	// Runtime overrides where the daemon reports itself as running, which reaches
	// the model in the self-model's Environment block. Empty auto-detects
	// (DetectRuntime). Set it when detection cannot see the sandbox, or to say
	// something more useful than "container".
	Runtime string `toml:"runtime"`

	// InstanceName is the display name for this Nine instance, sent to clients
	// top bar. When set here it is authoritative and fixed. When empty, the
	// daemon reuses a previously generated name persisted in the store, or — on a
	// first boot with neither — shows a placeholder and asks the LLM to coin a
	// random name asynchronously, then persists it (docs/configuration.md).
	InstanceName string `toml:"instance_name"`

	// SessionRetentionDays deletes an abandoned session — and everything keyed to
	// it — after this many days without activity. 0 disables it entirely;
	// unset uses runtime.DefaultSessionRetentionDays (10).
	//
	// Age is measured from last activity, not creation, so a long-running
	// conversation used this morning is never stale. A session with an active
	// goal or an active session plan is never taken, whatever its age: a standing
	// agent that wakes weekly looks abandoned after ten days precisely because it
	// is working correctly.
	//
	// This is the one setting in Nine that destroys history rather than bounding
	// it. Every deletion is logged with what it removed.
	SessionRetentionDays *int `toml:"session_retention_days"`

	// EventRetentionTurns bounds the session_events journal per agent: the number
	// of most-recent turns kept at boot (adr/event-log.md v4). 0 → default
	// (DefaultEventRetentionTurns); negative → keep all turns.
	EventRetentionTurns int `toml:"event_retention_turns"`
	// EventRetentionDays caps the age of any journaled event in days. 0 → no age
	// limit (turn count is the only bound).
	EventRetentionDays int `toml:"event_retention_days"`

	// RelatedSessionsIndex enables the out-of-band subscriber that links
	// topically-similar sessions and the context-builder surfacing that pulls
	// from it (adr/reactive-events.md). On by default; set
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
	// Bin is the directory holding plugins that ship as their own executable.
	// Since the Go built-ins moved into the nine binary (plugin.md R-PLUG.13)
	// that is `browser` alone; user plugins come from UserDir instead. The
	// container overrides this with NINE_PLUGINS_BIN.
	Bin string `toml:"bin"`

	// CacheDir is the root under which each plugin gets its own scratch directory
	// (docs/plugin-capabilities.md §4). Empty falls back to the OS user cache dir
	// (os.UserCacheDir()/nine/plugins). The container overrides it with
	// NINE_PLUGINS_CACHE_DIR. It must be durable, not /tmp, because persistent
	// caches live under the same root.
	CacheDir string `toml:"cache_dir"`

	// JobPollSeconds is how often the daemon polls running plugin jobs
	// (docs/plugin-capabilities.md §5). 0 uses runtime.DefaultJobPollSeconds.
	JobPollSeconds int `toml:"job_poll_seconds"`

	// JobMinDelayMS is the floor on the delay between a standing tool's calls.
	// 0 uses runtime.DefaultJobMinDelayMS.
	JobMinDelayMS int `toml:"job_min_delay_ms"`

	// JobWorkers bounds how many standing-tool calls run at once.
	// 0 uses runtime.DefaultJobWorkers.
	JobWorkers int `toml:"job_workers"`

	// JobMaxSeconds bounds a single job's lifetime: the sweeper marks an over-age
	// job failed and attempts a cancel. 0 uses runtime.DefaultJobMaxSeconds (1h).
	JobMaxSeconds int `toml:"job_max_seconds"`

	// MaxJobsTotal caps the daemon's outstanding jobs across every conversation
	// and both backends. 0 uses runtime.DefaultMaxJobsTotal (32).
	//
	// The per-conversation cap below bounds one agent; this bounds the machine.
	// It mattered less when every job was a goroutine in a plugin's process — now
	// a tool job is work this daemon performs, so the total is a real resource.
	MaxJobsTotal int `toml:"max_jobs_total"`

	// MaxJobsPerConversation caps a conversation's outstanding jobs, so a looping
	// model cannot start an unbounded number. 0 uses runtime's default (8).
	MaxJobsPerConversation int `toml:"max_jobs_per_conversation"`

	// Disabled names plugins that must never start, by their wire name
	// (`shell`, `browser`, a user plugin's manifest name…). It is the operator's
	// lever for withholding a capability — most obviously `shell` — and applies
	// uniformly to built-ins, plugins with their own binary, and user plugins
	// (spec/contracts/plugin.md R-PLUG.14).
	//
	// It exists because the built-ins moved into the nine binary (R-PLUG.13):
	// before that, a plugin could be withheld by simply not shipping its binary,
	// and a built-in has no binary to omit. A disabled plugin is reported by
	// `nine plugins` rather than silently absent. The container overrides this
	// with NINE_PLUGINS_DISABLED (comma-separated).
	Disabled []string `toml:"disabled"`

	// UserDir holds operator-supplied plugins, discovered at boot from a
	// sidecar-manifest layout: an executable `<name>` beside a `<name>.toml`
	// manifest (name + entrypoint). It is scanned separately from the built-in
	// Bin dir and is never required — empty or absent disables user plugins.
	// A binary with no manifest is never executed; a manifest whose binary fails
	// the plugin handshake, or whose tools collide with an already-loaded plugin,
	// is skipped and surfaced rather than aborting the boot
	// (spec/contracts/plugins.md). The container overrides this with
	// NINE_PLUGINS_USER_DIR.
	UserDir string `toml:"user_dir"`
}

// BootstrapConfig is the `[bootstrap]` table: where to find the declarative
// self-model an instance starts with (adr/personality-pattern.md §4).
type BootstrapConfig struct {
	// SelfModelPath names a TOML file whose sections become `self/*` KV entries
	// on first boot. Empty disables it, which is the shipped posture: an
	// unconfigured Nine seeds the generic defaults it always has.
	//
	// The file is read once per database. It is the operator's, never the
	// agent's: nothing at runtime writes it, and re-running the daemon does not
	// re-apply it, so an instance that has since revised its own self-model is
	// not reset to the packaged text on every restart.
	//
	// NINE_BOOTSTRAP_SELF_MODEL overrides it, which is how a personality image
	// points at a file it mounts without rewriting the config it inherited.
	SelfModelPath string `toml:"self_model_path"`

	// Genesis names a document turned into the instance's first processes on
	// the first boot of its store (adr/process-sessions.md §10).
	// NINE_BOOTSTRAP_GENESIS overrides it.
	Genesis string `toml:"genesis"`
	// GenesisTurns bounds the genesis session; 0 is 8.
	GenesisTurns int `toml:"genesis_turns"`
}

// ProcessRolesOrDefault returns [processes] process_roles, defaulted.
func (c ProcessesConfig) ProcessRolesOrDefault() []string {
	if len(c.ProcessRoles) > 0 {
		return c.ProcessRoles
	}
	return []string{"process"}
}

// GenesisReady reports why genesis cannot run with this config, or nil. Genesis
// writes processes through the model-authored tool tier, which the toolvm removal
// took away (adr/modular_nine.md §9.10); until that refactor lands, a configured
// genesis is reported and skipped rather than run against tools that no longer
// exist.
func (c *Config) GenesisReady() error {
	if strings.TrimSpace(c.Bootstrap.Genesis) == "" {
		return nil
	}
	if c.Bootstrap.GenesisTurns < 0 {
		return fmt.Errorf("[bootstrap] genesis_turns must not be negative")
	}
	return fmt.Errorf("genesis is suspended with the model-authored tool tier it wrote through (adr/modular_nine.md §9.10)")
}

// APIConfig holds the HTTP API server configuration for remote access to
// Nine's functionality (spec/contracts/api.md). The API runs as a separate
// process that communicates with the daemon via Unix socket, providing feature
// parity with the CLI over HTTP/REST.
type APIConfig struct {
	// Enabled starts the API server. Off by default; when off, `nine api serve`
	// still works but the daemon does not start it automatically.
	Enabled bool `toml:"enabled"`

	// Port is the HTTP server port. Defaults to 8080.
	Port int `toml:"port"`

	// Host is the bind address. Defaults to "localhost" (IPv4 loopback only).
	// Set to "0.0.0.0" to listen on all interfaces.
	Host string `toml:"host"`

	// AuthToken is an optional bearer token for API authentication. When set,
	// all requests must include `Authorization: Bearer <token>`. Empty disables
	// authentication.
	AuthToken string `toml:"auth_token"`

	// TimeoutSeconds is the request timeout in seconds. Defaults to 30.
	// A request that takes longer returns 504 Gateway Timeout.
	TimeoutSeconds int `toml:"timeout_seconds"`

	// MaxConnections limits concurrent connections. Defaults to 100.
	MaxConnections int `toml:"max_connections"`

	// CORSOrigins is the list of allowed CORS origins. Defaults to ["*"]
	// (all origins). Set to specific origins for production.
	CORSOrigins []string `toml:"cors_origins"`

	// TrustedProxies lists the reverse proxies whose X-Forwarded-For and
	// X-Real-IP headers the API believes, as bare IPs or CIDR blocks
	// ("10.0.0.0/8", "192.168.1.7"). Empty — the default — means the headers
	// are ignored entirely and rate limiting keys off the transport peer.
	//
	// Both headers are attacker-controlled on any request that did not pass
	// through a proxy the operator runs, so trusting them unconditionally lets
	// a client mint a fresh rate-limit bucket per request. Set this only for
	// proxies actually in front of the API.
	TrustedProxies []string `toml:"trusted_proxies"`

	// RateLimit configures rate limiting for the API.
	RateLimit APIRateLimitConfig `toml:"rate_limit"`

	// TLS configures HTTPS/TLS support.
	TLS APITLSConfig `toml:"tls"`

	// UI serves the web UI at / (docs/web-ui.md). On when unset; false serves
	// the API alone. A *bool so an unset value can default to on.
	UI *bool `toml:"ui"`
}

// UIEnabled reports whether the API server serves the web UI, defaulting to
// true when the key is unset.
func (a APIConfig) UIEnabled() bool {
	return a.UI == nil || *a.UI
}

// APIRateLimitConfig configures rate limiting.
type APIRateLimitConfig struct {
	// Enabled turns rate limiting on. Defaults to true.
	Enabled bool `toml:"enabled"`

	// RequestsPerMinute is the limit. Defaults to 60.
	RequestsPerMinute int `toml:"requests_per_minute"`

	// BurstSize allows short bursts above the limit. Defaults to 10.
	BurstSize int `toml:"burst_size"`

	// ExcludedPaths are paths not subject to rate limiting.
	// Defaults to ["/api/v1/health", "/api/v1/status"].
	ExcludedPaths []string `toml:"excluded_paths"`
}

// APITLSConfig configures TLS for the API server.
type APITLSConfig struct {
	// Enabled turns on HTTPS. Defaults to false.
	Enabled bool `toml:"enabled"`

	// CertPath is the path to the TLS certificate file.
	CertPath string `toml:"cert_path"`

	// KeyPath is the path to the TLS private key file.
	KeyPath string `toml:"key_path"`
}

// MCPConfig holds the MCP servers Nine should connect to. Each becomes one
// plugin row: the manager connects per server — spawning the command as a
// supervised child or dialing the URL — so an MCP server is a plugin in every
// respect that matters: its own failure domain, its own row in `nine plugins`,
// and the same disable switch (spec/contracts/plugin.md R-PLUG.15).
type MCPConfig struct {
	// Servers are declared as [[mcp.server]] table arrays, following the same
	// singular-table-in-a-plural-section shape [[agent]] already uses.
	Servers []MCPServer `toml:"server"`
}

// MCPServer is one MCP server: a command to spawn and talk to over stdio.
type MCPServer struct {
	// Name identifies the server and prefixes its tools (`github` →
	// `github__create_issue`), so two servers that both advertise `search`
	// cannot collide. It is also the plugin's wire name, as `mcp:<name>`.
	Name string `toml:"name"`

	// Command is the executable to spawn, spoken to over stdio; Args are its
	// arguments. Most MCP servers ship as an npx/uvx invocation, e.g.
	// command = "npx", args = ["-y", "@modelcontextprotocol/server-github"].
	Command string   `toml:"command"`
	Args    []string `toml:"args"`

	// Env are extra environment variables for this server — typically its API
	// token. They are passed to the server process only, not to Nine's other
	// plugins, and follow the same withholding rule as everything else a plugin
	// receives (the daemon's own secrets are never inherited).
	Env map[string]string `toml:"env"`

	// URL reaches a server over streamable HTTP instead of spawning one, for
	// hosted MCP services that never run locally. Exactly one of Command or URL
	// is set. Headers are sent on every request — typically
	// Authorization = "Bearer …".
	URL     string            `toml:"url"`
	Headers map[string]string `toml:"headers"`
}

type MemoryConfig struct {
	Path string `toml:"path"` // SQLite database file path (see Config.DatabasePath)

	// SurfaceMemories controls the context-builder pull-surfacing of stored
	// key-value memories: each `memory_set` is mirrored into a shared vector pool
	// and, on later turns, the memories most similar to the current query are
	// injected as advisory enrichment. On by default; set
	// `surface_memories = false` to disable. Requires an embedder — it is a no-op
	// when embeddings are disabled. A *bool so an unset value can default to on
	// while an explicit false still disables it.
	SurfaceMemories *bool `toml:"surface_memories"`
}

// SurfaceMemoriesEnabled reports whether stored memories are indexed and
// pull-surfaced into the context, defaulting to true when the key is unset.
func (m MemoryConfig) SurfaceMemoriesEnabled() bool {
	return m.SurfaceMemories == nil || *m.SurfaceMemories
}

// API defaults.
const (
	// DefaultAPIPort is the default HTTP API server port.
	DefaultAPIPort = 8080
	// DefaultAPIHost is the default bind address (localhost only).
	DefaultAPIHost = "localhost"
	// DefaultAPITimeoutSeconds is the default request timeout.
	DefaultAPITimeoutSeconds = 30
	// DefaultAPIMaxConnections is the default max concurrent connections.
	DefaultAPIMaxConnections = 100
	// DefaultAPIRequestsPerMinute is the default rate limit.
	DefaultAPIRequestsPerMinute = 60
	// DefaultAPIBurstSize is the default rate limit burst size.
	DefaultAPIBurstSize = 10
)

// GetPort returns the effective API port, defaulting to DefaultAPIPort.
func (a APIConfig) GetPort() int {
	if a.Port == 0 {
		return DefaultAPIPort
	}
	return a.Port
}

// GetHost returns the effective API host, defaulting to DefaultAPIHost.
func (a APIConfig) GetHost() string {
	if a.Host == "" {
		return DefaultAPIHost
	}
	return a.Host
}

// GetTimeoutSeconds returns the effective timeout, defaulting to DefaultAPITimeoutSeconds.
func (a APIConfig) GetTimeoutSeconds() int {
	if a.TimeoutSeconds == 0 {
		return DefaultAPITimeoutSeconds
	}
	return a.TimeoutSeconds
}

// GetMaxConnections returns the effective max connections, defaulting to DefaultAPIMaxConnections.
func (a APIConfig) GetMaxConnections() int {
	if a.MaxConnections == 0 {
		return DefaultAPIMaxConnections
	}
	return a.MaxConnections
}

// GetCORSOrigins returns the effective CORS origins, defaulting to ["*"] if unset.
func (a APIConfig) GetCORSOrigins() []string {
	if len(a.CORSOrigins) == 0 {
		return []string{"*"}
	}
	return a.CORSOrigins
}

// GetTrustedProxies returns the configured trusted proxy IPs and CIDR blocks.
// Empty means no proxy is trusted, so forwarding headers are ignored.
func (a APIConfig) GetTrustedProxies() []string {
	return a.TrustedProxies
}

// RateLimitEnabled returns whether rate limiting is enabled, defaulting to true.
func (a APIConfig) RateLimitEnabled() bool {
	return a.RateLimit.Enabled
}

// RequestsPerMinute returns the effective rate limit, defaulting to DefaultAPIRequestsPerMinute.
func (a APIConfig) RequestsPerMinute() int {
	if a.RateLimit.RequestsPerMinute == 0 {
		return DefaultAPIRequestsPerMinute
	}
	return a.RateLimit.RequestsPerMinute
}

// BurstSize returns the effective burst size, defaulting to DefaultAPIBurstSize.
func (a APIConfig) BurstSize() int {
	if a.RateLimit.BurstSize == 0 {
		return DefaultAPIBurstSize
	}
	return a.RateLimit.BurstSize
}

// ExcludedPaths returns the effective excluded paths for rate limiting.
func (a APIConfig) ExcludedPaths() []string {
	if len(a.RateLimit.ExcludedPaths) == 0 {
		return []string{"/api/v1/health", "/api/v1/status"}
	}
	return a.RateLimit.ExcludedPaths
}

// TLSEnabled returns whether TLS is enabled.
func (a APIConfig) TLSEnabled() bool {
	return a.TLS.Enabled
}

type EmbeddingsConfig struct {
	Provider string `toml:"provider"`
	Model    string `toml:"model"`
	Endpoint string `toml:"endpoint"`
	APIKey   string `toml:"api_key"`
}

// CurrentConfigSchema is the config shape this binary understands. Bump it in
// the same change that makes an incompatible alteration to the layout, and add
// the matching step to configMigrations.
const CurrentConfigSchema = 4

// SchemaTooNewError is returned when a config declares a shape this binary does
// not know. It is a distinct type because the caller must not treat it the way
// it treats a missing file: falling through to the next path would boot the
// daemon on defaults, silently dropping every setting the operator wrote —
// including an API auth token, which turns a safety check into an exposure.
type SchemaTooNewError struct {
	Path  string
	Found int
	Known int
}

func (e *SchemaTooNewError) Error() string {
	return fmt.Sprintf("%s declares config schema version %d; this binary understands %d — "+
		"run the version of nine that wrote it, or update the file to this schema",
		e.Path, e.Found, e.Known)
}

// configMigrations[i] brings a config from schema i+1 to i+2, mirroring the
// store's migration list (internal/memory/migrate.go).
var configMigrations = []func(*Config) error{
	// 1 → 2: [[process]] replaces [[agent]], [[agent.routine]], when = { … },
	// [[standing_tool]] and [daemon] standing_agents_authoritative
	// (adr/process-sessions.md §11). Nothing is translated: Load refuses a file
	// that still has one of them, from the raw TOML, before any step runs, so
	// a schema-1 file that reaches this step has nothing to change. Bumping the
	// schema is what makes an older binary refuse a [[process]] file instead of
	// silently ignoring every block in it.
	func(*Config) error { return nil },
	// 2 → 3: the toolvm is gone (adr/modular_nine.md §9.10). [tools] keeps only
	// max_output_tokens; the sandbox host, its limits and the generated tier
	// under [tools]/[tools.agent]/[[tool.<name>]] are refused from the raw TOML
	// (retiredToolKeys below), and [tools] job_min_delay_ms/job_workers moved to
	// [plugins]. Nothing is translated: a schema-2 file that reaches this step
	// has none of the retired keys left to change.
	func(*Config) error { return nil },
	// 3 → 4: the `[board]` table is added (adr/orchestrator-board.md §2.4).
	// It is optional and defaults when absent, so there is nothing to
	// translate; bumping the schema is what makes an older binary refuse a
	// file that carries it instead of silently ignoring the limits.
	func(*Config) error { return nil },
}

func Load(path string) (*Config, error) {
	var cfg Config
	md, err := toml.DecodeFile(path, &cfg)
	if err != nil {
		return nil, err
	}

	// Absent is 1, not 0: a file written before the field existed is a
	// schema-1 file.
	if cfg.SchemaVersion == 0 {
		cfg.SchemaVersion = 1
	}
	if cfg.SchemaVersion > CurrentConfigSchema {
		return nil, &SchemaTooNewError{Path: path, Found: cfg.SchemaVersion, Known: CurrentConfigSchema}
	}
	for v := cfg.SchemaVersion; v < CurrentConfigSchema; v++ {
		if err := configMigrations[v-1](&cfg); err != nil {
			return nil, fmt.Errorf("migrate config %d→%d: %w", v, v+1, err)
		}
	}
	cfg.SchemaVersion = CurrentConfigSchema

	for key, hint := range retiredBlocks {
		if md.IsDefined(key) {
			return nil, fmt.Errorf("%s: %s (docs/configuration.md)", path, hint)
		}
	}
	if md.IsDefined("daemon", "standing_agents_authoritative") {
		return nil, fmt.Errorf("%s: [daemon] standing_agents_authoritative is replaced by [processes] authoritative", path)
	}
	for _, k := range retiredLimits {
		if md.IsDefined(k.key...) {
			return nil, fmt.Errorf("%s: %s is replaced by [processes] max_running, the one cap on processes running at once (docs/configuration.md)", path, k.name)
		}
	}
	for _, k := range retiredToolKeys {
		if md.IsDefined(k.key...) {
			return nil, fmt.Errorf("%s: %s (adr/modular_nine.md §9.10; docs/configuration.md)", path, k.hint)
		}
	}
	warnUndecoded(path, md)

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// warnUndecoded reports keys the file carries that nothing read.
//
// A misspelled key was previously neither applied nor reported, so a setting
// could silently fail to take effect — the decoder knew all along and Load
// threw the answer away. This warns rather than failing: a key from a newer
// Nine is a reason to tell the operator, not to refuse a config that is
// otherwise fine.
func warnUndecoded(path string, md toml.MetaData) {
	undecoded := md.Undecoded()
	if len(undecoded) == 0 {
		return
	}
	keys := make([]string, 0, len(undecoded))
	for _, k := range undecoded {
		keys = append(keys, k.String())
	}
	//nolint:gosec // G706: both values go through logsafe.Value, which strips the
	// control characters a forged log line needs. gosec traces the taint from the
	// file into the sink but does not recognise a sanitiser.
	slog.Warn("config keys were not recognised and had no effect; check for a typo",
		"path", logsafe.Value(path), "keys", logsafe.Value(strings.Join(keys, ", ")))
}

// Validate checks per-plugin invariants that TOML decoding cannot express, so a
// malformed config fails at load rather than at spawn. Today it validates
// [plugin.<name>.settings] key names and value types (docs/plugin-capabilities.md
// §3); it is the natural home for future cross-field checks.
func (cfg *Config) Validate() error {
	for name, entry := range cfg.Plugin {
		if _, err := pluginSettingsEnv(name, entry.Settings); err != nil {
			return err
		}
	}
	if err := validateMCPServers(cfg.MCP.Servers); err != nil {
		return err
	}
	if err := validateBudget("[processes] budget", cfg.Processes.Budget); err != nil {
		return err
	}
	if err := validateProcesses(cfg.Process, cfg.Processes.BudgetOrDefault()); err != nil {
		return err
	}
	if err := cfg.GenesisReady(); err != nil {
		return err
	}
	switch cfg.Context.ToolExposition {
	case "", ToolExpositionRanked, ToolExpositionDeferred, ToolExpositionHybrid:
	default:
		return fmt.Errorf("[context] tool_exposition %q must be %q, %q or %q",
			cfg.Context.ToolExposition, ToolExpositionRanked, ToolExpositionDeferred, ToolExpositionHybrid)
	}
	if cfg.Context.PromotedBudget < 0 {
		return fmt.Errorf("[context] promoted_budget must not be negative")
	}
	if cfg.Context.HybridPicks < 0 {
		return fmt.Errorf("[context] hybrid_picks must not be negative")
	}
	if cfg.Context.ToolTopN < 0 {
		return fmt.Errorf("[context] tool_top_n must not be negative")
	}
	if cfg.Processes.DatabaseMaxMB < 0 {
		return fmt.Errorf("[processes] database_max_mb must not be negative")
	}
	if _, err := cfg.Processes.StatementTimeoutOrDefault(); err != nil {
		return err
	}
	return nil
}

// mcpServerNameRe constrains an MCP server name to what can serve as both a
// tool-name prefix the model sees (`github__create_issue`) and a path-safe
// socket filename. Underscore is excluded because `__` is the prefix separator:
// allowing it would make `a__b__c` ambiguous about where the server name ends.
var mcpServerNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9-]*$`)

// validateMCPServers rejects a malformed [[mcp.server]] at load rather than at
// spawn. A server with no name or command cannot start, and two servers sharing
// a name would produce colliding tool prefixes and colliding plugin names — all
// three are config mistakes worth naming precisely instead of surfacing later as
// a plugin that mysteriously did not appear.
func validateMCPServers(servers []MCPServer) error {
	seen := make(map[string]bool, len(servers))
	for i, s := range servers {
		switch {
		case s.Name == "":
			return fmt.Errorf("[[mcp.server]] #%d: name is required", i+1)
		case !mcpServerNameRe.MatchString(s.Name):
			return fmt.Errorf("[[mcp.server]] %q: name must be alphanumeric with dashes (it prefixes the server's tool names)", s.Name)
		case s.Command == "" && s.URL == "":
			return fmt.Errorf("[[mcp.server]] %q: needs command (a server to spawn) or url (a hosted one)", s.Name)
		case s.Command != "" && s.URL != "":
			return fmt.Errorf("[[mcp.server]] %q: set command or url, not both", s.Name)
		case s.URL != "" && len(s.Env) > 0:
			// env configures a process; a hosted server has none. Silently ignoring
			// it would leave an operator believing they had passed a token.
			return fmt.Errorf("[[mcp.server]] %q: env applies to a spawned server; use headers with url", s.Name)
		case s.Command != "" && len(s.Headers) > 0:
			return fmt.Errorf("[[mcp.server]] %q: headers apply to url; use env with command", s.Name)
		case seen[s.Name]:
			return fmt.Errorf("[[mcp.server]] %q: duplicate name", s.Name)
		}
		seen[s.Name] = true
	}
	return nil
}

// validateProcesses checks the `[[process]]` blocks.
//
// Everything here is a config error rather than a skipped block: a process
// runs unattended and indefinitely, and one an operator wrote and Nine silently
// ignored is the worst outcome available, because nothing ever reports its
// absence.
func validateProcesses(blocks []ProcessConfig, ceiling BudgetConfig) error {
	names := make(map[string]bool, len(blocks))
	for _, b := range blocks {
		if b.Name != "" {
			names[b.Name] = true
		}
	}
	seen := make(map[string]bool, len(blocks))
	for i, b := range blocks {
		where := fmt.Sprintf("[[process]] #%d", i+1)
		if b.Name != "" {
			where = fmt.Sprintf("[[process]] %q", b.Name)
		}
		switch {
		case b.Name == "":
			return fmt.Errorf("%s: name is required — it is what reconciliation keys on", where)
		case strings.Contains(b.Name, ":"):
			return fmt.Errorf("%s: a name cannot contain \":\", which Nine uses for the processes it names itself", where)
		case seen[b.Name]:
			return fmt.Errorf("%s: duplicate name", where)
		case b.Tool == "":
			return fmt.Errorf("%s: tool is required", where)
		case b.Every != "" && b.Schedule != "":
			return fmt.Errorf("%s: set every or schedule, not both", where)
		case b.Goal != "" && b.Tool != "pursue":
			return fmt.Errorf("%s: a goal process runs tool = \"pursue\"", where)
		case b.Goal != "" && b.Session != "":
			return fmt.Errorf("%s: a goal process owns its session; it cannot also attach to %q", where, b.Session)
		case b.Session != "" && !names[b.Session]:
			return fmt.Errorf("%s: session %q names no [[process]]", where, b.Session)
		case b.Session == b.Name && b.Session != "":
			return fmt.Errorf("%s: a process cannot attach to its own session", where)
		case b.ReportTo != "" && !names[b.ReportTo]:
			return fmt.Errorf("%s: report_to %q names no [[process]]", where, b.ReportTo)
		case b.Session != "" && (b.Role != "" || b.Delegates):
			return fmt.Errorf("%s: an attached process runs under its session's role; role and delegates belong to %q", where, b.Session)
		case b.Budget.TurnsPerDay > ceiling.TurnsPerDay:
			return fmt.Errorf("%s: budget turns_per_day %d is above [processes] budget's %d; a process may lower its budget, never raise it", where, b.Budget.TurnsPerDay, ceiling.TurnsPerDay)
		case b.Budget.TokensPerDay > ceiling.TokensPerDay:
			return fmt.Errorf("%s: budget tokens_per_day %d is above [processes] budget's %d; a process may lower its budget, never raise it", where, b.Budget.TokensPerDay, ceiling.TokensPerDay)
		}
		if err := validateBudget(where+" budget", b.Budget); err != nil {
			return err
		}
		if err := ValidateEvents(b.On, b.OnFilter); err != nil {
			return fmt.Errorf("%s: %w", where, err)
		}
		if (b.EventContent || b.OnFilter != (EventFilter{})) && len(b.On) == 0 {
			return fmt.Errorf("%s: event_content and on_filter need on", where)
		}
		if len(b.On) > 0 && b.Goal != "" {
			return fmt.Errorf("%s: a goal process runs pursue, which acts on its clock and on messages, not on events", where)
		}
		seen[b.Name] = true
		if b.Every != "" {
			d, err := time.ParseDuration(b.Every)
			if err != nil {
				return fmt.Errorf("%s: every %q: %w", where, b.Every, err)
			}
			if d <= 0 {
				return fmt.Errorf("%s: every %q must be positive", where, b.Every)
			}
		}
		if b.Schedule != "" {
			if _, err := cron.Parse(b.Schedule); err != nil {
				return fmt.Errorf("%s: schedule %q: %w", where, b.Schedule, err)
			}
		}
	}
	return nil
}

// validateBudget refuses a negative budget; zero means "not set here".
func validateBudget(where string, b BudgetConfig) error {
	if b.TurnsPerDay < 0 || b.TokensPerDay < 0 {
		return fmt.Errorf("%s: turns_per_day and tokens_per_day cannot be negative", where)
	}
	return nil
}

// retiredLimits are the caps [processes] max_running replaced
// (adr/process-sessions.md §5).
var retiredLimits = []struct {
	name string
	key  []string
}{
	{"[daemon] max_goal_sessions", []string{"daemon", "max_goal_sessions"}},
}

// retiredBlocks are the tables [[process]] replaced (adr/process-sessions.md
// §11). Loading a file that still has one is an error naming its replacement:
// silently ignoring it would make every agent and standing tool it declares
// vanish without a word.
var retiredBlocks = map[string]string{
	"agent":         `[[agent]] is replaced by [[process]]: name = <id>, tool = "pursue", goal = <description>, with role, delegates, every or schedule as before`,
	"standing_tool": `[[standing_tool]] is replaced by [[process]]: name = <id>, tool, args, every or schedule as before`,
}

// retiredToolKeys are the toolvm-era keys under [tools] and the [[tool.<name>]]
// tables, removed with the toolvm (adr/modular_nine.md §9.10). Loading a file
// that still has one is an error naming its replacement: a warning would let an
// operator believe a sandbox cap still applies when nothing enforces it.
var retiredToolKeys = []struct {
	hint string
	key  []string
}{
	{`[tools] enabled is retired with the toolvm; the shipped tools are built in and the shell is a plugin`, []string{"tools", "enabled"}},
	{`[tools] user_dir is retired with the toolvm; user tools are MCP servers now ([plugins].user_dir)`, []string{"tools", "user_dir"}},
	{`[tools] timeout is retired with the toolvm`, []string{"tools", "timeout"}},
	{`[tools] max_ops is retired with the toolvm`, []string{"tools", "max_ops"}},
	{`[tools] memory_mb is retired with the toolvm`, []string{"tools", "memory_mb"}},
	{`[tools] max_concurrent is retired with the toolvm`, []string{"tools", "max_concurrent"}},
	{`[tools] job_min_delay_ms moved to [plugins] job_min_delay_ms`, []string{"tools", "job_min_delay_ms"}},
	{`[tools] job_workers moved to [plugins] job_workers`, []string{"tools", "job_workers"}},
	{`[tools.agent] is retired with the toolvm; Nine does not write tools any more — write an MCP user plugin instead ([plugins].user_dir)`, []string{"tools", "agent"}},
	{`[[tool.<name>]] is retired with the toolvm`, []string{"tool"}},
}
