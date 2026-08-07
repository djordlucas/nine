package config

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

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
	Tools      ToolsConfig      `toml:"tools"`

	// Plugin holds per-plugin `[plugin.<name>]` tables (singular), sibling to the
	// plural `[plugins]` subsystem table above — the same split `[agent]` and
	// `[[agent]]` already use. It carries operator settings passed through to a
	// plugin as environment variables, so an operator can configure a plugin Nine
	// has never heard of without a rebuild (docs/plugin-capabilities.md §3).
	Plugin map[string]PluginEntry `toml:"plugin"`

	// Tool holds per-tool `[tool.<name>]` tables (singular), sibling to the plural
	// `[tools]` table above and following the same split. It is the grant half of
	// the sandboxed-tool capability model (spec/contracts/toolvm.md).
	Tool map[string]ToolEntry `toml:"tool"`
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

// ToolsConfig tunes the tool-dispatch boundary. It is `[tools]` rather than
// `[agent]` because `[[agent]]` is already the standing-agent table array.
type ToolsConfig struct {
	// MaxOutputTokens is the per-result output cap (docs/tool-output-spill.md).
	// A result over it is spilled to the memory file store and replaced by a
	// short preview naming the path, so raising this is rarely needed — the data
	// is not lost either way. Raise it when a model should routinely see more of
	// a large result inline; lower it to keep observations tight. 0 (unset)
	// keeps agent.DefaultMaxOutputTokens (2048).
	MaxOutputTokens int `toml:"max_output_tokens"`

	// Enabled turns the sandboxed-tool host on (spec/contracts/toolvm.md). Off by
	// default: a deployment that never sets it behaves exactly as it did before
	// the host existed, which is the additive property the whole design rests on
	// (docs/sandboxed-tools.md §1).
	Enabled bool `toml:"enabled"`

	// UserDir holds developer sandboxed tools, discovered at boot from the same
	// sidecar-manifest layout user plugins use: a `<name>.js` or `<name>.wasm`
	// beside a `<name>.toml` manifest. Empty or absent loads nothing. A file with
	// no manifest is never loaded, and a tool whose name collides with a built-in,
	// a plugin tool, or an earlier-loaded sandboxed tool is skipped and surfaced
	// rather than aborting the boot (R-TVM.10).
	UserDir string `toml:"user_dir"`

	// Timeout is the per-call wall-clock deadline. It is the only CPU bound the
	// host has — wazero offers no fuel metering — so an operator running many
	// concurrent sessions is trusting this, not a work budget
	// (docs/sandboxed-tools.md §3). Empty uses toolvm.DefaultTimeout (5s).
	Timeout string `toml:"timeout"`

	// MemoryMB caps a single call's linear memory. 0 uses toolvm.DefaultMemoryMB
	// (16 MiB).
	MemoryMB int `toml:"memory_mb"`
}

// ToolEntry is one `[tool.<name>]` table (singular), sibling to the plural
// `[tools]` subsystem table above — the same split `[plugin.<name>]` and
// `[plugins]` already use (R-PLUG.10).
//
// It is where an operator confers capabilities on one named sandboxed tool. The
// asymmetry with the tool's own manifest is the point: a manifest *declares a
// need* and only this *grants* (docs/sandboxed-tools.md §6.3). There is
// deliberately no wildcard `[tool."*"]` — an operator granting filesystem access
// to a tool does so to a tool they have read.
type ToolEntry struct {
	Capabilities ToolCapabilities `toml:"capabilities"`
}

// ToolCapabilities is the grant side of the capability model. Every field
// defaults to the empty set: no filesystem, no network, no environment. Clock,
// randomness, and logging are granted unconditionally because they leak nothing
// (docs/sandboxed-tools.md §6.2) and so have no knob here.
type ToolCapabilities struct {
	FS ToolFSGrant `toml:"fs"`

	// Env is an explicit key allowlist, never an all-or-nothing flag: the
	// daemon's environment holds LLM provider API keys, so a tool granted "env"
	// wholesale would be a credential exfiltration primitive. The NINE_* and
	// *_API_KEY patterns are refused outright at load (Config.Validate).
	Env []string `toml:"env"`

	// Net grants outbound HTTP. It is the one capability with no wazero primitive
	// behind it — wazero has no network at all — so every check that makes it
	// safe is Nine's own (docs/sandboxed-tools.md §8).
	Net ToolNetGrant `toml:"net"`
}

// ToolFSGrant maps host paths into a tool's guest filesystem. wazero enforces
// the scope itself via pre-opens, so this is the one capability Nine does not
// have to police at call time.
type ToolFSGrant struct {
	Read  []ToolMount `toml:"read"`
	Write []ToolMount `toml:"write"`
}

// ToolMount is one host→guest path mapping. The guest path is what the tool's
// own code sees, which is what makes narrowing a mount to a subdirectory a
// one-line change on the operator's side (docs/sandboxed-tools.md §7.1).
type ToolMount struct {
	Host  string `toml:"host"`
	Guest string `toml:"guest"`
}

// ToolNetGrant carries the `net.http` grant. Absent (nil HTTP) is the default
// and means no network at all.
type ToolNetGrant struct {
	HTTP *ToolHTTPGrant `toml:"http"`
}

// ToolHTTPGrant is the shape §8 specifies.
type ToolHTTPGrant struct {
	// AllowHosts is required and may not be a bare "*". Entries are exact names
	// ("api.example.com") or single-wildcard subdomain patterns
	// ("*.example.com", which does not match the apex).
	//
	// This is only half the control: the allowlist is a *name* check, and a name
	// resolves wherever its owner points it. The other half — rejecting
	// link-local, loopback, and private addresses on the IP actually dialed — is
	// unconditional and not configurable, because an operator cannot be asked to
	// remember that 169.254.169.254 is where their cloud keeps its credentials.
	AllowHosts []string `toml:"allow_hosts"`
	// Methods is required: the HTTP methods this tool may use.
	Methods []string `toml:"methods"`
	// MaxBytes caps the response body. 0 uses toolvm.DefaultHTTPMaxBytes (1 MiB).
	MaxBytes int `toml:"max_bytes"`
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

	// InstanceName is the display name for this Nine instance, shown in the TUI
	// top bar. When set here it is authoritative and fixed. When empty, the
	// daemon reuses a previously generated name persisted in the store, or — on a
	// first boot with neither — shows a placeholder and asks the LLM to coin a
	// random name asynchronously, then persists it (docs/configuration.md).
	InstanceName string `toml:"instance_name"`

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

	// CacheDir is the root under which each plugin gets its own scratch directory
	// (docs/plugin-capabilities.md §4). Empty falls back to the OS user cache dir
	// (os.UserCacheDir()/nine/plugins). The container overrides it with
	// NINE_PLUGINS_CACHE_DIR. It must be durable, not /tmp, because persistent
	// caches live under the same root.
	CacheDir string `toml:"cache_dir"`

	// JobPollSeconds is how often the daemon polls running plugin jobs
	// (docs/plugin-capabilities.md §5). 0 uses runtime.DefaultJobPollSeconds.
	JobPollSeconds int `toml:"job_poll_seconds"`

	// JobMaxSeconds bounds a single job's lifetime: the sweeper marks an over-age
	// job failed and attempts a cancel. 0 uses runtime.DefaultJobMaxSeconds (1h).
	JobMaxSeconds int `toml:"job_max_seconds"`

	// MaxJobsPerConversation caps a conversation's outstanding jobs, so a looping
	// model cannot start an unbounded number. 0 uses runtime's default (8).
	MaxJobsPerConversation int `toml:"max_jobs_per_conversation"`

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
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
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
	for name, entry := range cfg.Tool {
		if err := validateToolEntry(name, entry); err != nil {
			return err
		}
	}
	return nil
}

// validateToolEntry checks one `[tool.<name>]` grant. Everything here is a
// config error rather than a runtime surprise, because a grant that silently
// does not mean what the operator thought is the failure mode the capability
// model exists to prevent.
func validateToolEntry(name string, entry ToolEntry) error {
	caps := entry.Capabilities

	for _, m := range append(append([]ToolMount{}, caps.FS.Read...), caps.FS.Write...) {
		if m.Host == "" || m.Guest == "" {
			return fmt.Errorf("[tool.%s]: fs mount needs both host and guest paths", name)
		}
		if !filepath.IsAbs(m.Host) {
			return fmt.Errorf("[tool.%s]: fs mount host path %q must be absolute", name, m.Host)
		}
	}

	for _, k := range caps.Env {
		if k == "" {
			return fmt.Errorf("[tool.%s]: env grant has an empty key", name)
		}
		// The daemon's environment holds LLM provider credentials, and a
		// sandboxed tool is precisely the thing that should never see them. This
		// is a refusal rather than a filter so that an operator who meant to grant
		// one finds out at load, not by wondering why the key is empty.
		if strings.HasPrefix(k, "NINE_") || strings.HasSuffix(k, "_API_KEY") {
			return fmt.Errorf("[tool.%s]: env key %q is reserved and cannot be granted to a sandboxed tool", name, k)
		}
	}

	return validateHTTPGrant(name, caps.Net.HTTP)
}

// validateHTTPGrant checks a `net.http` grant. Every rejection here is a config
// error rather than a runtime surprise, because the whole point of an egress
// allowlist is that the operator knows exactly what they permitted.
func validateHTTPGrant(name string, g *ToolHTTPGrant) error {
	if g == nil {
		return nil
	}

	if len(g.AllowHosts) == 0 {
		return fmt.Errorf("[tool.%s]: net.http needs allow_hosts; there is no implicit default", name)
	}
	for _, h := range g.AllowHosts {
		h = strings.ToLower(strings.TrimSpace(h))
		switch {
		case h == "":
			return fmt.Errorf("[tool.%s]: net.http allow_hosts has an empty entry", name)
		case h == "*":
			// An operator who wants an unrestricted egress tool should write a
			// native plugin, where that intent is explicit and reviewed
			// (docs/sandboxed-tools.md §8).
			return fmt.Errorf("[tool.%s]: net.http allow_hosts may not be a bare %q; name the hosts, or write a native plugin if you need unrestricted egress", name, "*")
		case strings.Contains(h, "://"), strings.Contains(h, "/"):
			return fmt.Errorf("[tool.%s]: net.http allow_hosts entry %q must be a hostname, not a URL", name, h)
		case strings.Count(h, "*") > 1, strings.Contains(h, "*") && !strings.HasPrefix(h, "*."):
			return fmt.Errorf("[tool.%s]: net.http allow_hosts entry %q must be an exact host or a leading %q pattern", name, h, "*.")
		}
	}

	if len(g.Methods) == 0 {
		return fmt.Errorf("[tool.%s]: net.http needs methods; there is no implicit default", name)
	}
	for _, m := range g.Methods {
		switch strings.ToUpper(strings.TrimSpace(m)) {
		case "GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS":
		default:
			return fmt.Errorf("[tool.%s]: net.http method %q is not a permitted method", name, m)
		}
	}

	if g.MaxBytes < 0 {
		return fmt.Errorf("[tool.%s]: net.http max_bytes must not be negative", name)
	}
	return nil
}
