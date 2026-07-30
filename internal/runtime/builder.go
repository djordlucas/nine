package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"nine/internal/agent"
	"nine/internal/config"
	ninectx "nine/internal/context"
	"nine/internal/embed"
	"nine/internal/llm"
	"nine/internal/memory"
	"nine/internal/plugin"
	"nine/internal/protocol"
	"nine/internal/selfmodel"
)

// minSubAgentTimeoutSeconds is the smallest run_agents timeout the caller may
// request. Values below this (including 0/unset) fall back to
// AgentBuilderConfig.TaskTimeoutSeconds, since the LLM-supplied default tends
// to be too short for multi-agent research tasks.
const minSubAgentTimeoutSeconds = 300

// LoopConfig holds the static infrastructure shared across all loops built by
// an AgentBuilder. It groups plugin and AI dependencies that do not change
// between individual loop instantiations.
type LoopConfig struct {
	// Plugin infrastructure
	Mgr *plugin.Manager

	// AI infrastructure
	Embedder      embed.Embedder
	Memory        *memory.Store
	ContextBudget int
	SystemPrompt  string
	Assembler     *selfmodel.Assembler
	// RelatedSessions mirrors [daemon] related_sessions_index: when on (and an
	// embedder is configured), each loop pull-surfaces a recorded related prior
	// session relevant to the current turn (docs/reactive-events.md §5). On by
	// default; the read costs nothing when disabled.
	RelatedSessions bool
	// SurfaceMemories mirrors [memory] surface_memories: when on (and an embedder
	// is configured), each memory_set is indexed into the shared vector pool and
	// each loop pull-surfaces the stored memories most relevant to the current
	// turn. On by default; the read costs nothing when disabled.
	SurfaceMemories bool
	// MaxToolOutputTokens mirrors [tools] max_output_tokens: the dispatcher's
	// per-result output cap. 0 keeps agent.DefaultMaxOutputTokens.
	MaxToolOutputTokens int
}

// AgentBuilderConfig holds the behavioral dependencies layered on top of the
// loop infrastructure. Keep this small: add new per-loop knobs to LoopConfig,
// new factory-level behaviors here.
type AgentBuilderConfig struct {
	Loop         LoopConfig
	InitialQueue *llm.Queue
	NotifAdd     func(agentID, text string)
	// NotifyUser posts a message to the human-facing notification feed for a
	// goal-owning background shell (notify_user tool; docs/predefined-agents.md
	// §5 piece 2). Nil disables the tool (e.g. in tests).
	NotifyUser         func(agentID, text string)
	Sup                *Supervisor
	TaskTimeoutSeconds int // default 1800 (30 min) when 0

	// HITL coordinates ask_human and approval gates for interactive sessions.
	// Nil disables human-in-the-loop (e.g. in tests).
	HITL *HITL
	// ApprovalTools are tool names that require human approval before running,
	// from [hitl].require_approval. Only enforced for loops owned by an
	// interactive session — the session's own, and (per GateSubAgents) the
	// sub-agents it spawns.
	ApprovalTools []string
	// GateSubAgents extends the ApprovalTools gates into sub-agents spawned by
	// an interactive session, so delegation is not a way around them
	// ([hitl].gate_sub_agents, default on). Sub-agents never get ask_human
	// regardless — only the automatic gates (R-HITL.1/R-HITL.5).
	GateSubAgents bool

	// DefaultLeafRole names the role used when a delegation names none
	// (roles.default_leaf; default "executor" — R-ROLE.9).
	DefaultLeafRole string
	// MaxDelegationDepth seeds the depthGuard recursion backstop
	// (roles.max_delegation_depth; default 2 — R-ROLE.6).
	MaxDelegationDepth int

	// PlanApproval is the plan-approval mode (off | on | on-risky) from
	// [planning].plan_approval, already defaulted. "off" disables the interactive
	// plan-review checkpoint, "on" always prompts, "on-risky" prompts only when
	// the plan names an ApprovalTools entry.
	PlanApproval string

	// PlanMode is the reasoning policy (off | plan-only | always) from
	// [planning].plan_mode, already defaulted. "off" disables native thinking and
	// the analysis pass; "always" thinks on every inner call; "plan-only" is the
	// default plan-then-execute.
	PlanMode string
}

// AgentBuilder builds configured agent loops. It owns sub-agent tracking and
// the role registry.
// Its BuildForRole method satisfies the LoopFactory type.
type AgentBuilder struct {
	cfg          AgentBuilderConfig
	roles        *RoleRegistry
	queuePtr     atomic.Pointer[llm.Queue]
	goalSpawnFn  atomic.Pointer[agent.GoalSessionSpawnFn]
	emitProgress atomic.Pointer[func(agentID string, msg protocol.Msg)]

	subAgentMu      sync.RWMutex
	activeSubAgents []protocol.SubAgentInfo

	// toolVecs caches per-tool description embeddings (keyed by tool name) so the
	// context builder can rank tools by relevance to the query. Tool descriptions
	// are static after boot, so each is embedded at most once across all sessions.
	toolVecMu sync.Mutex
	toolVecs  map[string][]float32
}

// toolVector returns the cached embedding of a tool's description, computing it
// once on first use. Returns nil when no embedder is configured (ranking then
// degrades to always-include tools plus insertion order under the top-N cap).
func (f *AgentBuilder) toolVector(emb embed.Embedder, td llm.ToolDef) []float32 {
	if emb == nil {
		return nil
	}
	f.toolVecMu.Lock()
	if v, ok := f.toolVecs[td.Name]; ok {
		f.toolVecMu.Unlock()
		return v
	}
	f.toolVecMu.Unlock()

	// Embed outside the lock — this may be a network call for a remote embedder.
	// A concurrent miss for the same tool just recomputes; the result is identical.
	text := td.Name + ": " + td.Description
	vec, err := emb.Embed(context.Background(), text)
	if err != nil {
		slog.Debug("embed tool failed", "tool", td.Name, "err", err)
		return nil
	}

	f.toolVecMu.Lock()
	if f.toolVecs == nil {
		f.toolVecs = make(map[string][]float32)
	}
	f.toolVecs[td.Name] = vec
	f.toolVecMu.Unlock()
	return vec
}

// NewAgentBuilder creates an AgentBuilder with the given config.
func NewAgentBuilder(cfg AgentBuilderConfig) *AgentBuilder {
	var rs roleSkillStore
	if cfg.Loop.Memory != nil {
		rs = cfg.Loop.Memory
	}
	f := &AgentBuilder{cfg: cfg, roles: NewRoleRegistry(rs, cfg.DefaultLeafRole)}
	f.queuePtr.Store(cfg.InitialQueue)
	return f
}

// Roles exposes the builder's role registry (read-side: Resolve/LeafRoles).
func (f *AgentBuilder) Roles() *RoleRegistry { return f.roles }

// Build creates a top-level orchestrator loop for agentID — the pre-roles
// entry point, kept for callers that always want the session-following root.
// interactive enables human-in-the-loop tools (ask_human, approval gates).
func (f *AgentBuilder) Build(agentID string, interactive bool) *agent.Loop {
	return f.BuildForRole(agentID, RoleParams{Role: OrchestratorRole, Interactive: interactive})
}

// BuildForRole creates a root agent loop for agentID from p. Satisfies
// LoopFactory. Unknown role names resolve to the default leaf role (R-ROLE.9).
// p.Interactive is effective only for HITL-eligible roles (AND-ed with the
// role's Interactive flag, docs/roles.md §6); p.OwnsGoal and p.Delegates layer
// the pursue-shell's goal ownership and delegation opt-in over the resolved
// role (docs/predefined-agents.md §3.1).
func (f *AgentBuilder) BuildForRole(agentID string, p RoleParams) *agent.Loop {
	role := f.roles.Resolve(p.Role)
	role.Interactive = role.Interactive && p.Interactive
	role.OwnsGoal = p.OwnsGoal
	role.Delegates = role.Delegates || p.Delegates
	// A root session owns its own gates; sub-agents inherit this owner (subGate).
	var gate gateCtx
	if role.Interactive && f.cfg.HITL != nil {
		gate.owner = agentID
	}
	return f.build(agentID, role, f.maxDelegationDepth(), gate)
}

// maxDelegationDepth returns the configured depthGuard seed (R-ROLE.6).
func (f *AgentBuilder) maxDelegationDepth() int {
	if f.cfg.MaxDelegationDepth > 0 {
		return f.cfg.MaxDelegationDepth
	}
	return DefaultMaxDelegationDepth
}

// SubAgents returns a snapshot of currently-running sub-agents.
func (f *AgentBuilder) SubAgents() []protocol.SubAgentInfo {
	f.subAgentMu.RLock()
	defer f.subAgentMu.RUnlock()
	cp := make([]protocol.SubAgentInfo, len(f.activeSubAgents))
	copy(cp, f.activeSubAgents)
	return cp
}

// UpdateQueue atomically replaces the LLM queue used for all new loops.
func (f *AgentBuilder) UpdateQueue(q *llm.Queue) {
	f.queuePtr.Store(q)
}

// QueueDepth reports the shared LLM queue's current load (pending, inflight,
// maxConcurrent), for surfacing back-pressure in daemon status. Zeroes if no
// queue is configured.
func (f *AgentBuilder) QueueDepth() (pending, inflight, maxConcurrent int) {
	if q := f.queuePtr.Load(); q != nil {
		return q.Depth()
	}
	return 0, 0, 0
}

// SetGoalSessionSpawnFn registers the function used to spawn background
// "pursue" sessions for newly-created top-level goals (see
// docs/goal-sessions.md). Only loops whose role has SpawnsGoals are wired
// with it. Must be called after the daemon is constructed (it depends on the
// daemon's session registry) and before any conversations are created.
func (f *AgentBuilder) SetGoalSessionSpawnFn(fn agent.GoalSessionSpawnFn) {
	f.goalSpawnFn.Store(&fn)
}

// SetEmitProgressFn registers the function used to deliver sub-agent
// lifecycle events (sub_agent_start/sub_agent_end) to a conversation's
// progress stream. Must be called after the daemon is constructed (it
// depends on the daemon's session registry) and before any conversations are
// created.
func (f *AgentBuilder) SetEmitProgressFn(fn func(agentID string, msg protocol.Msg)) {
	f.emitProgress.Store(&fn)
}

// emitProgressEvent forwards msg to agentID's progress stream, if a function
// is registered. Safe to call with no function registered (e.g. in tests).
func (f *AgentBuilder) emitProgressEvent(agentID string, msg protocol.Msg) {
	if p := f.emitProgress.Load(); p != nil {
		(*p)(agentID, msg)
	}
}

// protectedKeyPrefixes are KV key prefixes that Nine cannot delete.
var protectedKeyPrefixes = []string{
	"self/",
}

// coreToolNames are the memory/file/skill tools available at every nesting depth.
var coreToolNames = []string{
	"memory_get", "memory_set", "memory_delete", "memory_list",
	"file_store", "file_fetch", "file_list", "file_search_text",
	"skill_list", "skill_read", "skill_write", "skill_modify",
}

// subAgentToolNames are the run_agent/workflow/goal_create delegation tools
// available only to delegating roles with depthGuard remaining (R-ROLE.6; was:
// depth < 2). Goal *self-management* is not here — it is a shell capability
// (goalSelfMgmtToolNames), granted independently of delegation.
var subAgentToolNames = []string{
	"run_agent", "run_agents",
	"workflow_create", "workflow_get", "workflow_update", "workflow_list", "workflow_retry_step",
	"goal_create",
}

// goalSelfMgmtToolNames are the goal-ownership tools a pursue-shell session
// uses to steer its own goal. They are granted to any goal-owning shell
// (role.OwnsGoal) or delegating role, bypassing an allowlist role's own tool
// set the way gap_report does (docs/predefined-agents.md §3.1).
var goalSelfMgmtToolNames = []string{
	"goal_get", "goal_list", "goal_update_status", "goal_append_subtree",
}

// appendInterceptedTools appends the agent.InterceptedDefs entries whose
// names appear in names to tools, preserving InterceptedDefs order.
//
// roleEnum, when non-empty, replaces the delegation tools' static definitions
// with ones whose `role` field lists the live leaf roles (R-ROLE.8) — that is
// what lets an operator- or agent-authored role skill be advertised to the
// model at all, rather than only resolving if the model guesses its name.
// Only the InputSchema varies; Name and Description are untouched, so the
// builder's name-keyed tool-description embedding cache stays valid.
func appendInterceptedTools(tools []ninectx.ToolWithVector, names []string, roleEnum string) []ninectx.ToolWithVector {
	nameSet := make(map[string]bool, len(names))
	for _, n := range names {
		nameSet[n] = true
	}
	rendered := make(map[string]llm.ToolDef)
	if roleEnum != "" {
		for _, def := range agent.SubAgentDefs(roleEnum) {
			rendered[def.Name] = def
		}
	}
	for _, def := range agent.InterceptedDefs {
		if !nameSet[def.Name] {
			continue
		}
		if r, ok := rendered[def.Name]; ok {
			def = r
		}
		tools = append(tools, ninectx.ToolWithVector{Tool: def})
	}
	return tools
}

// build assembles an agent loop for agentID running role. depthGuard is the
// delegation-recursion backstop: delegation tools are registered only when
// role.Delegates and depthGuard > 0, and each spawn passes depthGuard-1
// (R-ROLE.6). role.Interactive must already be the effective value (role
// eligibility AND caller interactivity). gate routes approval-gate prompts to
// the owning interactive session, if any — a sub-agent has role.Interactive
// false but may still be gated through its parent's owner.
func (f *AgentBuilder) build(agentID string, role Role, depthGuard int, gate gateCtx) *agent.Loop {
	lc := f.cfg.Loop
	d := agent.New()

	for _, p := range lc.Mgr.Running() {
		if p != nil {
			d.RegisterPlugin(lc.Mgr, p)
		}
	}

	f.registerCoreTools(d, lc, agentID)

	// shellTools are conferred by the session shell, not the role's allowlist —
	// they are granted unfiltered (like gap_report) and pruning in RestrictTo
	// must let them through (docs/predefined-agents.md §3.1). Delegation,
	// goal-ownership, and human-output are structural capabilities, so a narrow
	// role that opts into them (e.g. a standing agent with delegates=true) still
	// gets the tools even though its allowlist never lists them.
	var shellTools []string

	delegates := role.Delegates && depthGuard > 0
	if delegates {
		f.registerSubAgentTools(d, lc, agentID, role, depthGuard, gate)
		shellTools = append(shellTools, subAgentToolNames...)
	}
	// Goal self-management is a shell capability: any goal-owning pursue shell
	// (role.OwnsGoal) or delegating role can read and steer its goal.
	goalMgmt := delegates || role.OwnsGoal
	if goalMgmt {
		agent.RegisterGoalManagement(d, lc.Memory)
		shellTools = append(shellTools, goalSelfMgmtToolNames...)
	}
	// notify_user is the human-output surface for a goal-owning background shell
	// (docs/predefined-agents.md §5 piece 2): a standing/pursue session has no
	// conversation, so this is how it reaches a human.
	if role.OwnsGoal && f.cfg.NotifyUser != nil {
		agent.RegisterNotifyUser(d, agentID, f.cfg.NotifyUser)
		shellTools = append(shellTools, "notify_user")
	}
	// ask_human stays interactive-only (R-HITL.1): a sub-agent or background
	// session never raises its own questions. Approval gates are broader — they
	// follow the *owning* session, so a gated tool a sub-agent reaches for is
	// still put to the human who started the conversation (R-HITL.5). That is
	// what stops delegation from being a way around require_approval.
	if role.Interactive && f.cfg.HITL != nil {
		f.registerAskHuman(d, agentID)
	}
	if gate.gated() && f.cfg.HITL != nil && len(f.cfg.ApprovalTools) > 0 {
		f.registerApprovalGates(d, agentID, gate)
	}

	// Catalog meta-tools let the model query the full tool/skill catalogs on
	// demand, covering the once-per-turn ranking blind spot
	// (docs/tool-exposition.md). Granted regardless of the role allowlist — like
	// gap_report — so they are advertised, always-included, and survive
	// RestrictTo. tool_list is the query-free enumeration and needs no embedder;
	// the *_search pair does, since without one there is nothing to rank
	// against. tool_list's and tool_search's handlers are registered below once
	// the tool list exists; skill_search's rides in RegisterSkillTools above.
	shellTools = append(shellTools, "tool_list")
	if lc.Embedder != nil {
		shellTools = append(shellTools, "tool_search", "skill_search")
	}

	// Boundary 2 of R-ROLE.4: for allowlist roles, prune dispatch handlers so
	// a disallowed tool cannot run even if the model hallucinates its name.
	// gap_report survives every allowlist — it is the escape hatch when no
	// allowed tool fits (R-ROLE.5). Wildcard roles keep the full handler set,
	// including the registered-but-unadvertised tools (gap_report,
	// memory_embed/memory_query/file_search_semantic), exactly as before roles.
	if !role.AllTools {
		allow := append([]string{"gap_report"}, role.Tools...)
		allow = append(allow, shellTools...)
		d.RestrictTo(allow)
	}

	// Boundary 1 of R-ROLE.4: the advertised tool list. The role enum is
	// rendered from the live registry only for roles that can delegate —
	// nothing else advertises run_agent, so nothing else needs it (R-ROLE.8).
	var roleEnum string
	if delegates {
		roleEnum = f.roles.RoleEnum()
	}
	tools := buildToolList(lc, role, shellTools, roleEnum)
	if role.Interactive && f.cfg.HITL != nil {
		tools = append(tools, ninectx.ToolWithVector{Tool: agent.AskHumanDef})
	}

	// Populate each tool's description embedding (cached) so the context builder
	// ranks non-always tools by relevance to the query rather than insertion order.
	for i := range tools {
		tools[i].Vector = f.toolVector(lc.Embedder, tools[i].Tool)
	}
	// tool_search ranks — and tool_list enumerates — the finalized advertised set
	// (vectors populated above), so register them now that `tools` is complete.
	// The closure returns this loop's slice, so results only ever include tools
	// it can actually call.
	getTools := func() []ninectx.ToolWithVector { return tools }
	agent.RegisterToolSearch(d, lc.Embedder, getTools)
	agent.RegisterToolList(d, getTools)

	alwaysTools := filterByRole(append([]string{}, coreToolNames...), role)
	alwaysTools = append(alwaysTools, shellTools...)
	if role.Interactive && f.cfg.HITL != nil {
		alwaysTools = append(alwaysTools, agent.AskHumanDef.Name)
	}
	perLoopBuilder := ninectx.New(ninectx.Config{
		Budget:      lc.ContextBudget,
		ToolTopN:    20,
		AlwaysTools: alwaysTools,
	})

	var selfModelFn func(ctx context.Context, queryVec []float32) string
	if lc.Assembler != nil {
		selfModelFn = lc.Assembler.Build
	}

	// Pull-surface a recorded related prior session (docs/reactive-events.md §5),
	// only when the indexer that populates the store is itself enabled and an
	// embedder is present to rank relevance to the current query.
	var relatedFn func(ctx context.Context, queryVec []float32) string
	if lc.RelatedSessions && lc.Embedder != nil && lc.Memory != nil {
		relatedFn = relatedEnrichmentFn(lc.Memory, agentID)
	}

	// Pull-surface stored key-value memories relevant to the current query. Same
	// preconditions as related sessions: needs an embedder to rank and the store
	// to read; disabled via [memory] surface_memories.
	var memoryFn func(ctx context.Context, queryVec []float32) string
	if lc.SurfaceMemories && lc.Embedder != nil && lc.Memory != nil {
		memoryFn = memoryEnrichmentFn(lc.Memory)
	}

	// The loop exposes a single enrichment channel (context builder priority 2.6,
	// shared token cap), so the two pull-surfacers are composed into one.
	enrichmentFn := composeEnrichment(relatedFn, memoryFn)

	// The role body is the persona; an empty body falls back to the daemon's
	// configured system prompt (R-ROLE.3). This is what gives leaves their
	// finite-task stance instead of the orchestrator prompt (R-ROLE.10).
	systemCore := role.SystemPrompt
	if systemCore == "" {
		systemCore = lc.SystemPrompt
	}
	// Role-selection steering is composed here rather than baked into the
	// daemon prompt, so it reaches every worker that can actually delegate —
	// including one running a role with its own body, which never sees the
	// daemon prompt at all (the executor is exactly that case). Roles that
	// cannot delegate never get run_agent, so steering them would be dead
	// text (R-ROLE.8).
	if delegates {
		systemCore += DelegationSteering(f.roles.LeafRoles(), f.roles.DefaultLeaf())
	}

	analystPrompt := ""
	if analyst := f.roles.Resolve(AnalystRole); analyst.Name == AnalystRole {
		analystPrompt = analyst.SystemPrompt
	}

	// Plan-approval checkpoint: interactive sessions only (R-HITL.1), and only when
	// the plan intends to use a risky tool (on-risky, reusing require_approval  as the single source of truth)
	// approve -> proceed; any other answer is folded in as clarification that re-runs analysis
	var planReviewFn func(ctx context.Context, plan string) (agent.PlanDecision, error)
	if role.Interactive && f.cfg.HITL != nil && f.cfg.PlanApproval != config.PlanApprovalModeOff {
		approval := f.cfg.ApprovalTools
		onRisky := f.cfg.PlanApproval != config.PlanApprovalModeOn // "on" always prompts; on-risky (default) filters
		planReviewFn = func(ctx context.Context, plan string) (agent.PlanDecision, error) {
			if onRisky && !planMentionRiskyTool(plan, approval) {
				return agent.PlanDecision{Proceed: true}, nil
			}

			answer, err := f.cfg.HITL.Ask(ctx, agentID, plan, []string{"approve", "clarify"})
			if err != nil {
				return agent.PlanDecision{}, err
			}

			if answer == "approve" {
				return agent.PlanDecision{Proceed: true}, nil
			}

			return agent.PlanDecision{Proceed: false, Clarification: answer}, nil
		}
	}

	return agent.NewLoop(agent.Config{
		Role:           role.Name,
		SystemCore:     systemCore,
		Priority:       llm.PriorityConversation,
		MaxTokens:      2048,
		Tools:          tools,
		Embedder:       lc.Embedder,
		SelfModelFn:    selfModelFn,
		EnrichmentFn:   enrichmentFn,
		ThinkPolicy:    agent.DefaultThinkPolicy,
		AnalysisPrompt: analystPrompt,
		PlanMode:       f.cfg.PlanMode,
		PlanReviewFn:   planReviewFn,
	}, perLoopBuilder, f.queuePtr.Load(), d)
}

// planMentionRiskyTool checks the presence of risky tools in a plan
func planMentionRiskyTool(plan string, riskyTools []string) bool {
	for _, t := range riskyTools {
		if t != "" && strings.Contains(plan, t) {
			return true
		}
	}

	return false
}

// registerCoreTools registers the gap-report, memory/file, and skill tools
// available to every role (allowlist pruning happens afterwards in build).
func (f *AgentBuilder) registerCoreTools(d *agent.Dispatcher, lc LoopConfig, agentID string) {
	agent.RegisterGapReport(d, func(desc string) {
		f.cfg.Sup.Post(Event{Kind: EventGapReported, AgentID: agentID, Payload: desc})
	})
	agent.RegisterMemoryTools(d, lc.Memory, lc.Embedder, protectedKeyPrefixes, lc.SurfaceMemories)
	agent.RegisterSkillTools(d, lc.Memory, lc.Embedder)
	// Over-cap tool results spill to the file store and come back by path, for
	// this loop and any sub-agent loop built from it.
	d.SetMaxOutputTokens(lc.MaxToolOutputTokens) // no-op when unset
	registerLargeOutput(d, lc.Memory, agentID)
}

// registerSubAgentTools registers run_agent/run_agents/workflow/goal tools
// for a delegating role. Spawned children run the leaf role named by the
// delegation call (default executor) with depthGuard-1 (R-ROLE.6/8/9).
func (f *AgentBuilder) registerSubAgentTools(d *agent.Dispatcher, lc LoopConfig, agentID string, role Role, depthGuard int, gate gateCtx) {
	spawnOne := func(ctx context.Context, parentID, task, extraCtx, roleName string) agent.SubAgentResult {
		leaf := f.roles.ResolveLeaf(roleName)
		subID := newUUID()
		prompt := task
		if extraCtx != "" {
			prompt = extraCtx + "\n\n" + task
		}
		taskPreview := truncate(task, 80)
		removeSubAgent := f.trackSubAgent(subID, task, leaf.Name)
		f.emitProgressEvent(parentID, protocol.NewSubAgentStartMsg(parentID, subID, task, leaf.Name))
		slog.Info("sub_agent_start", "parent_id", parentID, "sub_id", subID, "role", leaf.Name, "task", taskPreview)
		spawnStart := time.Now()
		// The child inherits this loop's gate owner, so an approval prompt from
		// any delegation depth still lands on the session a human is watching.
		result, err := RunSubAgentSync(ctx, subID, prompt, f.build(subID, leaf, depthGuard-1, f.subGate(gate, leaf.Name, task)))
		removeSubAgent()
		status := "done"
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			status = "timed_out"
		case err != nil:
			status = "failed"
		}
		slog.Info("sub_agent_end", "sub_id", subID, "status", status, "elapsed_ms", time.Since(spawnStart).Milliseconds())
		f.emitProgressEvent(parentID, protocol.NewSubAgentEndMsg(parentID, subID, task, status, leaf.Name))
		return agent.SubAgentResult{Task: task, Result: result, Err: err}
	}

	agent.RegisterRunAgent(d, func(ctx context.Context, task, extraCtx, roleName string) (string, error) {
		r := spawnOne(ctx, agentID, task, extraCtx, roleName)
		return r.Result, r.Err
	})

	agent.RegisterRunAgents(d, func(ctx context.Context, tasks []agent.SubAgentTask, timeoutSecs int) []agent.SubAgentResult {
		if timeoutSecs < minSubAgentTimeoutSeconds {
			timeoutSecs = f.cfg.TaskTimeoutSeconds
			if timeoutSecs <= 0 {
				timeoutSecs = 1800
			}
		}
		tctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSecs)*time.Second)
		defer cancel()

		results := make([]agent.SubAgentResult, len(tasks))
		var wg sync.WaitGroup
		for i, t := range tasks {
			wg.Add(1)
			go func(i int, t agent.SubAgentTask) {
				defer wg.Done()
				results[i] = spawnOne(tctx, agentID, t.Task, t.Context, t.Role)
			}(i, t)
		}
		wg.Wait()
		return results
	})

	agent.RegisterWorkflowTools(d, agentID, lc.Memory, func(ctx context.Context, task, extraCtx string) (string, error) {
		r := spawnOne(ctx, agentID, task, extraCtx, "")
		return r.Result, r.Err
	})

	// Only goal-spawning roles (the orchestrator) can spawn background pursue
	// sessions for the top-level goals they create (was: depth == 0).
	var goalSpawn agent.GoalSessionSpawnFn
	if role.SpawnsGoals {
		if p := f.goalSpawnFn.Load(); p != nil {
			goalSpawn = *p
		}
	}
	agent.RegisterGoalCreate(d, agentID, lc.Memory, goalSpawn)
}

// gateCtx routes a loop's approval-gate prompts to a human. owner is the
// interactive session whose progress stream carries them and whose ID answers
// them; an empty owner means this loop is not gated at all. origin attributes
// the prompt in the UI and is empty when the owner's own loop is the asker.
//
// A sub-agent inherits its parent's owner (see subGate), so a gate raised at
// any delegation depth surfaces on the one session a human is actually
// watching — emitting to the sub-agent's own ID would find no session and the
// call would block until timeout.
type gateCtx struct {
	owner  string
	origin string
}

// gated reports whether this loop should enforce approval gates.
func (g gateCtx) gated() bool { return g.owner != "" }

// subGate derives the gate context for a sub-agent spawned by a loop running
// under g. Gating stops at the parent when [hitl].gate_sub_agents is off.
func (f *AgentBuilder) subGate(g gateCtx, roleName, task string) gateCtx {
	if !g.gated() || !f.cfg.GateSubAgents {
		return gateCtx{}
	}
	return gateCtx{owner: g.owner, origin: fmt.Sprintf("sub-agent %q · %s", roleName, truncate(task, 60))}
}

// registerAskHuman registers the ask_human tool. Interactive sessions only
// (R-HITL.1) — a sub-agent is a bounded worker, not a conversational
// participant, so it never gets to raise its own questions.
func (f *AgentBuilder) registerAskHuman(d *agent.Dispatcher, agentID string) {
	agent.RegisterAskHuman(d, agentID, f.cfg.HITL.Ask)
}

// registerApprovalGates arms the [hitl].require_approval gates for a loop
// owned by an interactive session, routing each prompt through HITL.AskFrom so
// the TUI has one rendering path (R-HITL.5). askerID is this loop's own agent
// ID — it keys the persisted request, keeping parallel sub-agents from
// colliding on a single pending row.
func (f *AgentBuilder) registerApprovalGates(d *agent.Dispatcher, askerID string, gate gateCtx) {
	hitl := f.cfg.HITL
	d.SetApproval(f.cfg.ApprovalTools, func(ctx context.Context, toolName string, args json.RawMessage) error {
		ans, err := hitl.AskFrom(ctx, askerID, gate.owner, gate.origin, approvalQuestion(toolName, args), nil)
		if err != nil {
			return &agent.ApprovalError{Err: fmt.Errorf("tool %s approval failed: %w", toolName, err)}
		}
		if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(ans)), "y") {
			return &agent.ApprovalError{Err: fmt.Errorf("tool %s rejected by user", toolName)}
		}
		return nil
	})
}

// truncate shortens s to at most n runes, appending an ellipsis when it cuts.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}

// approvalQuestion builds a tool-aware approval prompt: shell shows its
// command, write_file/file_store its path, everything else truncated JSON args.
func approvalQuestion(toolName string, args json.RawMessage) string {
	var detail string
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(args, &fields)
	unquote := func(key string) string {
		var s string
		if raw, ok := fields[key]; ok {
			_ = json.Unmarshal(raw, &s)
		}
		return s
	}
	switch toolName {
	case "shell":
		detail = "Command: " + unquote("command")
	case "write_file", "file_store":
		detail = "Path: " + unquote("path")
	default:
		a := strings.TrimSpace(string(args))
		if len(a) > 200 {
			a = a[:200] + "…"
		}
		detail = "Args: " + a
	}
	return fmt.Sprintf("Run tool %q?\n\n%s\n\nEnter \"yes\" to proceed, anything else to cancel.", toolName, detail)
}

// filterByRole returns names intersected with the role's allowlist; wildcard
// roles pass through unchanged (R-ROLE.5: roles only narrow, never widen —
// names the role grants but the daemon doesn't expose are dropped by virtue
// of never being in names).
func filterByRole(names []string, role Role) []string {
	if role.AllTools {
		return names
	}
	allowed := make(map[string]bool, len(role.Tools))
	for _, n := range role.Tools {
		allowed[n] = true
	}
	var out []string
	for _, n := range names {
		if allowed[n] {
			out = append(out, n)
		}
	}
	return out
}

// buildToolList constructs the advertised tool list for a loop: plugin tools,
// the always-available core tools, and (for delegating roles with depthGuard
// remaining) the run_agent/workflow/goal tools — intersected with the role's
// allowlist (boundary 1 of R-ROLE.4).
func buildToolList(lc LoopConfig, role Role, shellTools []string, roleEnum string) []ninectx.ToolWithVector {
	var tools []ninectx.ToolWithVector
	for _, p := range lc.Mgr.Running() {
		if p == nil {
			continue
		}
		for _, td := range p.Tools {
			if !role.AllTools && !slices.Contains(role.Tools, td.Name) {
				continue
			}
			tools = append(tools, ninectx.ToolWithVector{
				Tool: td.ToLLMDef(),
			})
		}
	}
	tools = appendInterceptedTools(tools, filterByRole(coreToolNames, role), roleEnum)
	// Shell-conferred tools (delegation, goal self-management, notify_user) are
	// advertised unfiltered — the shell grants them, not the role's allowlist
	// (docs/predefined-agents.md §3.1).
	tools = appendInterceptedTools(tools, shellTools, roleEnum)
	return tools
}

func (f *AgentBuilder) trackSubAgent(id, description, role string) (remove func()) {
	f.subAgentMu.Lock()
	f.activeSubAgents = append(f.activeSubAgents, protocol.SubAgentInfo{ID: id, Description: description, Role: role})
	f.subAgentMu.Unlock()
	return func() {
		f.subAgentMu.Lock()
		for i, s := range f.activeSubAgents {
			if s.ID == id {
				f.activeSubAgents = append(f.activeSubAgents[:i], f.activeSubAgents[i+1:]...)
				break
			}
		}
		f.subAgentMu.Unlock()
	}
}
