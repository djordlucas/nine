package config

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

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
	// StandingTools are the `[[standing_tool]]` blocks: resumable tools the daemon
	// runs indefinitely on their own cadence (adr/standing-tools.md).
	StandingTools []StandingToolConfig `toml:"standing_tool"`
	Tools         ToolsConfig          `toml:"tools"`
	MCP           MCPConfig            `toml:"mcp"`

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
	// MaxOutputTokens is the per-result output cap (adr/tool-output-spill.md).
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

	// CacheDir is the root for the generated tier's dependency cache: extracted,
	// integrity-verified npm packages, content-addressed and shared across tools
	// (docs/sandboxed-tools.md §4.4). Empty uses os.UserCacheDir()/nine/tools.
	CacheDir string `toml:"cache_dir"`

	// JobMaxCalls bounds how many times one long-running tool job may be called
	// before the daemon fails it. 0 uses runtime.DefaultJobMaxCalls (720).
	//
	// This bound is essential rather than defensive. Each call is deadline-bounded
	// as ever, but a tool returning `continue` with after_ms = 0 forever converts
	// a bounded CPU story into an unbounded one, one legal call at a time.
	JobMaxCalls int `toml:"job_max_calls"`

	// JobWorkers bounds how many long-running tool calls the sweeper makes at
	// once. 0 uses runtime.DefaultJobWorkers (4).
	//
	// Each concurrent call is a wasm instantiation holding up to [tools] memory_mb,
	// so this is the knob that decides how much memory a busy job queue can take.
	JobWorkers int `toml:"job_workers"`

	// JobMinDelayMS is the floor on the delay a resumable tool may ask for
	// between calls. 0 uses runtime.DefaultJobMinDelayMS (250).
	//
	// The tool's request is a request: the effective delay is at least this, and
	// in practice is rounded up to the sweeper's next tick.
	JobMinDelayMS int `toml:"job_min_delay_ms"`

	// Agent is the `[tools.agent]` table: the generated tier, where Nine writes
	// its own tools (docs/sandboxed-tools.md §5.2). Off by default and
	// independent of `enabled` — an operator may want developer tools without
	// letting the agent author any.
	Agent ToolsAgentConfig `toml:"agent"`
}

// ToolsAgentConfig governs generated tools: the ones Nine writes itself.
//
// The distinction this table exists to enforce is the whole design
// (docs/sandboxed-tools.md §2): **the agent writes the code; the operator writes
// the grants; these are never the same actor.** Nine gains one column and never
// the other. A generated tool that could grant itself filesystem access would be
// a shell with extra steps.
type ToolsAgentConfig struct {
	// Enabled turns on `tool_write`. Off by default.
	Enabled bool `toml:"enabled"`

	// Eval additionally allows `js_eval` — one execution, nothing persisted
	// (§5.3). It is not a third trust tier: it runs under exactly the
	// generated-tool rules and is strictly *less* persistent. It earns a separate
	// switch because it is the surface that keeps iteration out of the catalog.
	Eval bool `toml:"eval"`

	// MaxTools caps the catalog. 0 uses toolvm.DefaultMaxGeneratedTools (64).
	//
	// This is the sleeper problem, not a resource limit: every generated tool
	// competes for the context budget in tool selection, so a catalog of 200
	// half-redundant tools degrades ranking for the *built-in* tools too. The
	// agent poisons its own tool selection and gets worse at everything (§9.2).
	MaxTools int `toml:"max_tools"`

	// RequireApproval selects when tool_write and js_eval route through the HITL
	// gate: "on_capability" (default), "always", or "never".
	//
	// The default gates on substance rather than frequency, and that is the whole
	// intent. Approval fatigue is what defeats approval gates: prompting a human
	// on a pure date-formatting tool trains them to approve without reading, and
	// then the one prompt that matters meets the same reflex. A gate that fires
	// rarely is a gate that gets read.
	RequireApproval string `toml:"require_approval"`

	// Capabilities is the MAXIMUM a generated tool may be granted — a ceiling,
	// not a default. A tool that declares nothing still gets nothing, however
	// permissive this is. The ceiling bounds what is *grantable*; the tool's own
	// declaration decides what is *granted*. The two are deliberately separate so
	// that widening the ceiling does not retroactively widen every existing tool.
	Capabilities ToolCapabilities `toml:"capabilities"`

	// Deps is the `[tools.agent.deps]` table: external npm dependencies for
	// generated tools (docs/sandboxed-tools.md §4.4). Off by default — the single
	// riskiest switch in the design.
	Deps ToolsDepsConfig `toml:"deps"`

	// AllowLongRunning lets a generated tool declare itself resumable and be run
	// as a long-running job. Off by default, mirroring [tools.agent.deps].mode.
	//
	// Nine writing itself a date formatter and Nine writing itself something that
	// runs for an hour across restarts are different propositions, and the
	// capability ceiling cannot express the difference: it bounds *reach*, not
	// *duration*. A capability-free tool that never stops is inert per call and
	// unbounded in aggregate.
	AllowLongRunning bool `toml:"allow_long_running"`

	// AllowStanding lets a generated tool ask to be run standing — indefinitely,
	// on its own cadence. Off by default, and separate from AllowLongRunning: a
	// job the model started still ends, where a standing run does not until
	// somebody stops it.
	//
	// Even with this on, every promotion is approved by a human (see
	// RequireApproval, which this deliberately overrides).
	AllowStanding bool `toml:"allow_standing"`

	// MaxStanding caps how many generated standing tools may exist at once.
	// 0 uses runtime.DefaultMaxGeneratedStanding (4). Small on purpose: unlike a
	// catalogued tool, each of these consumes cadence forever.
	MaxStanding int `toml:"max_standing"`

	// AllowNetworkDeps lifts the deps+net.http interlock. A package that can reach
	// the network can exfiltrate whatever the tool sees, so a tool that both
	// declares net.http AND resolves an external dependency is refused unless this
	// is set. The two features are individually reasonable and jointly a
	// data-exfiltration primitive (§4.4).
	AllowNetworkDeps bool `toml:"allow_network_deps"`
}

// ToolsDepsConfig is `[tools.agent.deps]`: when and how a generated tool may pull
// an external npm package. Resolution happens once, in the daemon, at tool_write
// time; by call time the tool is one self-contained module with no imports and no
// network (docs/sandboxed-tools.md §4.4).
type ToolsDepsConfig struct {
	// Mode is "off" (default), "allowlist" (only named packages, transitive
	// included), or "open" (anything within the budgets — a development posture).
	Mode string `toml:"mode"`
	// Registry is the npm-compatible registry base URL. Empty uses the public
	// registry; an internal mirror is the hardened choice.
	Registry string `toml:"registry"`
	// Allow is the operator's package list for allowlist mode: a name and a semver
	// range they are willing to stand behind. Ignored in open mode.
	Allow []DepAllow `toml:"allow"`
	// MaxPackages caps the resolved tree (transitive included), MaxBundleKB the
	// final bundle, MaxDepth the transitive depth. 0 uses the deps-package
	// defaults (24 / 2048 / 4). Budgets are how a small allowlist is kept small.
	MaxPackages int `toml:"max_packages"`
	MaxBundleKB int `toml:"max_bundle_kb"`
	MaxDepth    int `toml:"max_depth"`
	// Frozen resolves only from the cache/lockfile and never touches the network —
	// the air-gapped / reproducible posture. Iterate open, then freeze.
	Frozen bool `toml:"frozen"`
}

// DepAllow is one `[tools.agent.deps].allow` entry.
type DepAllow struct {
	Name    string `toml:"name"`
	Version string `toml:"version"` // a semver range, e.g. "^4.17.21"
}

// Deps modes for [tools.agent.deps].mode.
const (
	DepsModeOff       = "off"
	DepsModeAllowlist = "allowlist"
	DepsModeOpen      = "open"
)

// DepsMode returns the effective deps mode, defaulting to off.
func (a ToolsAgentConfig) DepsMode() string {
	switch a.Deps.Mode {
	case DepsModeAllowlist, DepsModeOpen, DepsModeOff:
		return a.Deps.Mode
	default:
		return DepsModeOff
	}
}

// Approval modes for [tools.agent].require_approval.
const (
	ToolApprovalOnCapability = "on_capability"
	ToolApprovalAlways       = "always"
	ToolApprovalNever        = "never"
)

// ApprovalMode returns the effective require_approval mode, defaulting to
// on_capability.
func (a ToolsAgentConfig) ApprovalMode() string {
	switch a.RequireApproval {
	case ToolApprovalAlways, ToolApprovalNever, ToolApprovalOnCapability:
		return a.RequireApproval
	case "":
		return ToolApprovalOnCapability
	default:
		return ToolApprovalOnCapability
	}
}

// StandingToolConfig is one `[[standing_tool]]` block.
//
// It follows `[[agent]]` deliberately — an operator who has declared a standing
// agent should recognise this on sight, and the ownership split is the same:
// this file owns the definition, the runtime owns whether it is running.
type StandingToolConfig struct {
	// ID is operator-chosen and stable; reconciliation keys on it, so renaming
	// creates a second standing tool rather than renaming the first.
	ID string `toml:"id"`
	// Tool is the resumable tool to run.
	Tool string `toml:"tool"`
	// Args is the tool's input at the start of each cycle, as a TOML table.
	Args map[string]any `toml:"args"`
	// Interval and Schedule are the cadence *between cycles*, and are mutually
	// exclusive — a duration ("10s") or a 5-field cron expression. Setting both
	// is a config error.
	Interval string `toml:"interval"`
	Schedule string `toml:"schedule"`
	// Enabled defaults to true. Setting it false declares a standing tool without
	// starting it, which is how you stage one before turning it on.
	Enabled *bool `toml:"enabled"`
}

// IsEnabled reports whether the block asks to run. Unset means yes: declaring a
// standing tool and having to also enable it would be a papercut.
func (c StandingToolConfig) IsEnabled() bool { return c.Enabled == nil || *c.Enabled }

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

	// Timeout overrides `[tools] timeout` for this one tool. Empty inherits it.
	//
	// It exists because one slow tool otherwise sets the deadline for every tool:
	// raising the global bound to accommodate a tool that legitimately takes
	// twenty seconds also hands twenty seconds to a tool with an infinite loop,
	// and the deadline is the only CPU bound the host has. Naming the tool keeps
	// the exception where it belongs.
	//
	// It is a resource bound rather than a capability, which is why it sits here
	// and not under [capabilities] (spec/contracts/toolvm.md R-TVM.4).
	Timeout string `toml:"timeout"`
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

	// State grants a durable, host-owned key/value store scoped to this tool.
	// Absent (nil) is the default and means the tool remembers nothing between
	// calls, which is how every tool behaved before this existed.
	State *ToolStateGrant `toml:"state"`
}

// ToolStateGrant is the `state` grant: what a tool may remember between calls.
//
// Scope is required and deliberately has no default. It decides whether the
// namespace is shared across every caller or keyed by conversation, and that is
// the whole security argument for the capability — a tool-scoped store is a
// cross-session information channel that needs no other capability, since a
// tool's arguments come from the model and can carry anything in that session's
// context. Defaulting it would be choosing that on the operator's behalf.
type ToolStateGrant struct {
	// Scope is "tool" or "conversation".
	Scope string `toml:"scope"`
	// MaxKeys, MaxValueKB and MaxTotalKB bound one namespace. 0 uses the
	// toolvm defaults (128 keys, 64 KB per value, 1024 KB in total).
	MaxKeys    int `toml:"max_keys"`
	MaxValueKB int `toml:"max_value_kb"`
	MaxTotalKB int `toml:"max_total_kb"`
	// TTL expires a written value after a duration ("24h"). Empty never expires.
	TTL string `toml:"ttl"`
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

	// Routines are additional stages the session carries alongside its pursue
	// shell, each waking on its own cadence. The scheduler has always supported
	// several stages per session; until this existed nothing could ask for more
	// than one (docs/session-plans.md).
	//
	// The pursue shell stays the session's role-bearing routine, so a routine
	// never sets a role — a session has exactly one, and two claimants would
	// make it depend on ordering.
	Routines []AgentRoutine `toml:"routine"`
}

// AgentRoutine is one additional stage on a standing agent's session, declared
// as a [[agent.routine]] table.
type AgentRoutine struct {
	Kind     string `toml:"kind"`     // registered routine kind (e.g. "idle-reflection")
	Interval string `toml:"interval"` // wake cadence (Go duration) — XOR Schedule
	Schedule string `toml:"schedule"` // cron expression — XOR Interval
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
	Endpoint       string `toml:"endpoint"`
	ContextBudget  int    `toml:"context_budget"`
	MaxConcurrent  int    `toml:"max_concurrent"`
	NumCtx         int    `toml:"num_ctx"`         // model context window size
	TimeoutSeconds int    `toml:"timeout_seconds"` // HTTP timeout for LLM calls; 0 = the adapter default

	// Thinking surfaces the model's extended-thinking reasoning as a live trace in
	// the TUI (sends think:true to Ollama and drops /no_think). A *bool
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

	// InstanceName is the display name for this Nine instance, shown in the TUI
	// top bar. When set here it is authoritative and fixed. When empty, the
	// daemon reuses a previously generated name persisted in the store, or — on a
	// first boot with neither — shows a placeholder and asks the LLM to coin a
	// random name asynchronously, then persists it (docs/configuration.md).
	InstanceName string `toml:"instance_name"`

	// StandingAgentsAuthoritative treats the [[agent]] list as the full desired
	// state (adr/predefined-agents-design.md §7 v3). When true, a config-origin goal no
	// longer listed in nine.toml is archived and its session stopped on boot.
	// Default false: removing an entry just stops reconciling it, leaving the
	// goal for the operator to archive manually. Never touches
	// conversation-created goals.
	StandingAgentsAuthoritative bool `toml:"standing_agents_authoritative"`

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

// MCPConfig holds the MCP servers Nine should connect to. Each becomes one
// plugin: the daemon starts an `mcp` bridge instance per server, so an MCP
// server is a plugin in every respect that matters — its own process, its own
// crash isolation, its own row in `nine plugins`, and the same disable switch
// (spec/contracts/plugin.md R-PLUG.15).
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
		if err := validateToolEntry("tool."+name, entry); err != nil {
			return err
		}
	}
	if err := validateMCPServers(cfg.MCP.Servers); err != nil {
		return err
	}
	if err := validateStandingTools(cfg.StandingTools); err != nil {
		return err
	}
	if err := validateToolEntry("tools.agent", ToolEntry{Capabilities: cfg.Tools.Agent.Capabilities}); err != nil {
		return err
	}
	switch cfg.Tools.Agent.RequireApproval {
	case "", ToolApprovalOnCapability, ToolApprovalAlways, ToolApprovalNever:
	default:
		return fmt.Errorf("[tools.agent]: require_approval %q must be %q, %q, or %q",
			cfg.Tools.Agent.RequireApproval,
			ToolApprovalOnCapability, ToolApprovalAlways, ToolApprovalNever)
	}
	switch cfg.Tools.Agent.Deps.Mode {
	case "", DepsModeOff, DepsModeAllowlist, DepsModeOpen:
	default:
		return fmt.Errorf("[tools.agent.deps]: mode %q must be %q, %q, or %q",
			cfg.Tools.Agent.Deps.Mode, DepsModeOff, DepsModeAllowlist, DepsModeOpen)
	}
	for _, a := range cfg.Tools.Agent.Deps.Allow {
		if a.Name == "" {
			return fmt.Errorf("[tools.agent.deps]: an allow entry has an empty name")
		}
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

// validateStandingTools checks the `[[standing_tool]]` blocks.
//
// Everything here is a config error rather than a skipped block. A standing tool
// runs unattended and indefinitely; one an operator wrote and Nine silently
// ignored is the worst outcome available, because nothing ever reports its
// absence.
func validateStandingTools(blocks []StandingToolConfig) error {
	seen := make(map[string]bool, len(blocks))
	for i, b := range blocks {
		where := fmt.Sprintf("[[standing_tool]] #%d", i+1)
		if b.ID != "" {
			where = fmt.Sprintf("[[standing_tool]] %q", b.ID)
		}
		switch {
		case b.ID == "":
			return fmt.Errorf("%s: id is required — it is what reconciliation keys on", where)
		case seen[b.ID]:
			return fmt.Errorf("%s: duplicate id", where)
		case b.Tool == "":
			return fmt.Errorf("%s: tool is required", where)
		}
		seen[b.ID] = true

		// One trigger, matching the standing-agent rule (docs/scheduling.md): both
		// set is a config error rather than a silent precedence nobody remembers.
		if b.Interval != "" && b.Schedule != "" {
			return fmt.Errorf("%s: set interval or schedule, not both", where)
		}
		if b.Interval == "" && b.Schedule == "" {
			return fmt.Errorf("%s: needs interval or schedule — a standing tool with no cadence would never run", where)
		}
		if b.Interval != "" {
			d, err := time.ParseDuration(b.Interval)
			if err != nil {
				return fmt.Errorf("%s: interval %q: %w", where, b.Interval, err)
			}
			if d <= 0 {
				return fmt.Errorf("%s: interval %q must be positive", where, b.Interval)
			}
		}
	}
	return nil
}

// validateToolEntry checks one `[tool.<name>]` grant. Everything here is a
// config error rather than a runtime surprise, because a grant that silently
// does not mean what the operator thought is the failure mode the capability
// model exists to prevent.
func validateToolEntry(table string, entry ToolEntry) error {
	caps := entry.Capabilities

	for _, m := range append(append([]ToolMount{}, caps.FS.Read...), caps.FS.Write...) {
		if m.Host == "" || m.Guest == "" {
			return fmt.Errorf("[%s]: fs mount needs both host and guest paths", table)
		}
		if !filepath.IsAbs(m.Host) {
			return fmt.Errorf("[%s]: fs mount host path %q must be absolute", table, m.Host)
		}
	}

	for _, k := range caps.Env {
		if k == "" {
			return fmt.Errorf("[%s]: env grant has an empty key", table)
		}
		// The daemon's environment holds LLM provider credentials, and a
		// sandboxed tool is precisely the thing that should never see them. This
		// is a refusal rather than a filter so that an operator who meant to grant
		// one finds out at load, not by wondering why the key is empty.
		if strings.HasPrefix(k, "NINE_") || strings.HasSuffix(k, "_API_KEY") {
			return fmt.Errorf("[%s]: env key %q is reserved and cannot be granted to a sandboxed tool", table, k)
		}
	}

	if err := validateStateGrant(table, caps.State); err != nil {
		return err
	}

	return validateHTTPGrant(table, caps.Net.HTTP)
}

// validateStateGrant checks a `state` grant.
//
// The scope check is the one that matters. Refusing an unset scope rather than
// picking the safer one is deliberate: the two scopes differ in whether a tool
// can carry data from one conversation into another, and an operator who did
// not decide that should be told, not defaulted.
func validateStateGrant(table string, g *ToolStateGrant) error {
	if g == nil {
		return nil
	}
	// The literals are toolvm.StateScopeTool / StateScopeConversation. They are
	// spelled out rather than imported for the same reason the net.http method
	// names are: this package validates the operator's file and stays free of the
	// subsystems the file configures, and runtime does the translation.
	switch g.Scope {
	case "tool", "conversation":
	case "":
		return fmt.Errorf(
			"[%s]: state needs scope = \"tool\" or \"conversation\"; there is no implicit "+
				"default, because the two differ in whether this tool can carry data "+
				"between conversations", table)
	default:
		return fmt.Errorf("[%s]: state scope %q is not \"tool\" or \"conversation\"", table, g.Scope)
	}
	for _, b := range []struct {
		name string
		v    int
	}{{"max_keys", g.MaxKeys}, {"max_value_kb", g.MaxValueKB}, {"max_total_kb", g.MaxTotalKB}} {
		if b.v < 0 {
			return fmt.Errorf("[%s]: state %s cannot be negative", table, b.name)
		}
	}
	if g.TTL != "" {
		d, err := time.ParseDuration(g.TTL)
		if err != nil {
			return fmt.Errorf("[%s]: state ttl %q: %w", table, g.TTL, err)
		}
		if d <= 0 {
			return fmt.Errorf("[%s]: state ttl %q must be positive; omit it for no expiry", table, g.TTL)
		}
	}
	return nil
}

// validateHTTPGrant checks a `net.http` grant. Every rejection here is a config
// error rather than a runtime surprise, because the whole point of an egress
// allowlist is that the operator knows exactly what they permitted.
func validateHTTPGrant(table string, g *ToolHTTPGrant) error {
	if g == nil {
		return nil
	}

	if len(g.AllowHosts) == 0 {
		return fmt.Errorf("[%s]: net.http needs allow_hosts; there is no implicit default", table)
	}
	for _, h := range g.AllowHosts {
		h = strings.ToLower(strings.TrimSpace(h))
		switch {
		case h == "":
			return fmt.Errorf("[%s]: net.http allow_hosts has an empty entry", table)
		case h == "*":
			// Permitted, and deliberately the only pattern that has to be written
			// out in full. It grants any *host*; it does not grant any *address* —
			// loopback, link-local, private ranges and multicast stay blocked at
			// dial time whatever the allowlist says (internal/toolvm/ssrf.go).
			// Refusing it used to point operators at a native plugin instead,
			// which has the daemon's uid and none of those checks.
		case strings.Contains(h, "://"), strings.Contains(h, "/"):
			return fmt.Errorf("[%s]: net.http allow_hosts entry %q must be a hostname, not a URL", table, h)
		case strings.Count(h, "*") > 1, strings.Contains(h, "*") && !strings.HasPrefix(h, "*."):
			return fmt.Errorf("[%s]: net.http allow_hosts entry %q must be an exact host or a leading %q pattern", table, h, "*.")
		}
	}

	if len(g.Methods) == 0 {
		return fmt.Errorf("[%s]: net.http needs methods; there is no implicit default", table)
	}
	for _, m := range g.Methods {
		switch strings.ToUpper(strings.TrimSpace(m)) {
		case "GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS":
		default:
			return fmt.Errorf("[%s]: net.http method %q is not a permitted method", table, m)
		}
	}

	if g.MaxBytes < 0 {
		return fmt.Errorf("[%s]: net.http max_bytes must not be negative", table)
	}
	return nil
}
