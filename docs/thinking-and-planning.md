# Implementation Plan — Thinking & Planning Modes

**Status:** Complete (M1–M7 done) · **Roadmap:** replaces
`perform an initial request-analysis step with no tools, then include relevant tools only`

## Progress / resume here

- ✅ **M1 — L0 capability detection.** `ThinkingAware` interface (`internal/llm/provider.go`);
  Ollama `SupportsThinking` via `/api/show`, cached (`sync.Once`), conservative `false` on
  failure (`internal/llm/ollama/ollama.go`); `Complete` capability-gated, `/no_think` retired
  for `think:false`. Tested.
- ✅ **M2 — L1 primitive.** `Request.Think *bool` + `Response.ThinkingUsed bool`
  (`provider.go`); Ollama honors per-request override, gated by capability; effective decision
  surfaced. Tested (wire + surfaced).
- ✅ **M3 — L2 policy.** `Config.ThinkPolicy` + exported `DefaultThinkPolicy` (plan-then-execute)
  wired in `loop.go` `Run` (nil = defer, mechanism-only); runtime opts in via
  `builder.go` `agent.DefaultThinkPolicy`. **Policy A is LIVE** (capability-gated, Ollama-only;
  no config off-switch until M6). Tested.
- ▶ **M4 — analysis-pass fallback (in progress).** Sub-change plan, in order:
  1. ✅ `Queue.SupportsThinking(ctx)` delegating method (type-asserts `ThinkingAware`,
     `false` otherwise) — option A. Tested.
  2. ✅ `BuildInput.SystemPlan` at low priority **P4.5** (capped 500, yields to scratchpad) +
     spec contract update (R-CTX.2 table + R-CTX.4 order). Tested.
  3. ✅ Analysis pass in `Run`: no-tool completion (`Tools: nil`), `AnalysisPrompt`,
     `analysisMaxTokens=512`, gated on `AnalysisPrompt != "" && !SupportsThinking`; output
     injected as `SystemPlan`. Tested (`recordingThinker`, 3 cases, race-clean).
  4. ⬜ Query expansion — fold plan text into the embedding query for tool ranking.
     **Deferred** (optional polish; core pass works without it).
  5. ✅ Runtime wiring: `skills/roles/analyst.md` (builtin role, `tools: ""`) →
     `Config.AnalysisPrompt` in `builder.go` (name-guarded). Analyst excluded from
     `LeafRoles()` (not a delegation target). Tested.
  - **Gate correction (important):** the pass triggers on `Queue.ThinkingUnsupported`
    (provider IS `ThinkingAware` AND reports false) — NOT `!SupportsThinking`, which
    over-fires on providers without capability detection (test mocks). Tested.
- ✅ **M5 — Safety.** `PlanReviewFn`/`PlanDecision` on loop `Config`; invoked between the
  analysis pass and the exec loop with the **raw** plan text; reject-with-clarification folds
  the text in as a user message and re-runs analysis (`loop.go`). Runtime wires it in
  `builder.go` `build()` only when `role.Interactive && HITL != nil`, reusing `HITL.Ask`
  (generic `human_input` transport — no daemon changes) with approve/clarify options.
  **On-risky** gate via `planMentionRiskyTool` reusing `f.cfg.ApprovalTools`
  (`[hitl].require_approval`) — substring match biased toward prompting; the per-tool gate
  (`:481`) remains the real net. Config knob (`plan_approval` off/on/on-risky) deferred to M6.
  Tested: proceed, reject→re-analyze, on-risky filter. Race-clean.
- ✅ **M6 — L3 dynamic.** `[planning]` config block (`plan_mode`, `plan_approval`) with
  defaulting helpers (`config.go`); `plan_approval` drives the M5 checkpoint (off/on/on-risky),
  `plan_mode` now lives on the loop as mutex-guarded state (`loop.PlanMode`/`SetPlanMode`) that
  Run reads each turn to derive thinking + the analysis-pass gate — replacing M-C2's build-time
  switch so it can change live. `/think` per-turn force: `Msg.force_think` → `userTurn` →
  `turnReq` → `loop.SetForceThinkNextTurn` → `ThinkPolicy(_, forced)`, parsed from the `/think`
  prefix in the TUI (`client.TurnForced`). `set_plan_mode` wire command routes to
  `worker.setPlanMode`; TUI `/plan-mode <mode>`; current mode surfaced in `AgentInfo.plan_mode`
  (status). Tested (off suppresses think+analysis; live switch; force isolation; on-risky). Race-clean.
- ✅ **M7 — UX + docs.** `plan_start`/`plan_end` progress events bracket the analysis pass
  (`loop.onPlanStart/onPlanEnd` → worker `emitEvent` → TUI shows "planning…" via `renderThinking`);
  `notice` event fires **once per session** when native thinking is desired but unsupported
  (co-located with the analysis-pass gate, `loop.downgradeNoticed` one-shot; TUI `appendSystem`).
  Wire contract, `configuration.md`, `usage.md`, and `nine*.toml` updated. Tested
  (`TestLoopPlanEventsAndNotice`: 2 turns → 2×start/end, 1 notice). Race-clean.
  - **Deferred (small follow-up):** reflecting capability-unsupported directly in `status`/`context`
    (needs a cached-capability read without a blocking probe in `handleStatus`). The per-turn
    `notice` already tells the user; the status field is a nicety.

## Goal

Give Nine a **"plan before execute"** policy with graceful degradation. Reasoning
before tool use has two backends chosen automatically by model capability:

1. **Native extended thinking** (Ollama `think:true`) when the model supports it.
2. **A no-tool request-analysis pass** (a normal completion, tools stripped) when it
   doesn't — a poor-man's thinking that works on any model.

One intent, two backends, one capability gate. Plus dynamic user control and a
two-layer safety model so a bad plan can't run away.

## The unifying model

```
PlanPolicy (per turn, keyed on llmCallN):
  supports thinking  → native thinking on call #1        (set req.Think = true)
  does NOT support   → run analysis pass, inject plan, then execute with tools
  disabled           → neither

Plan safety (only the analysis-pass branch produces a durable, sticky plan):
  interactive session → optional plan-approval checkpoint (reuses HITL)   + trimmable
  autonomous session  → no human available → trimmable low-priority is the ONLY guard
```

Native-thinking traces are discardable (trace-only, never folded into
`Response.Text`), so they carry **no** sticky-plan risk and need no safety. The
analysis pass is the only branch that puts a durable plan into context — which is
exactly the branch where both the danger and (for interactive sessions) a concrete
artifact to review live.

---

## Layer 0 — Capability detection (gates everything)

**Why:** sending `think:true` to a model without the capability returns
`400 "model does not support thinking"`; and today the disable path unconditionally
prepends `/no_think` (`internal/llm/ollama/ollama.go:112-119`), which is noise on
non-Qwen models.

**Mechanism:** probe Ollama `POST /api/show {"model": "..."}` → `capabilities` array
(e.g. `["completion","tools","thinking","vision"]`). Presence of `"thinking"` is the
signal. Model is fixed per `Provider`, so probe **once, lazily (first `Complete`),
cache**. Lazy — not at construction — so a not-yet-running Ollama doesn't fail boot.

**Optional capability interface** (keep core `Provider` untouched):

```go
// internal/llm/provider.go — new, next to Provider (line 70)
type ThinkingAware interface {
    SupportsThinking(ctx context.Context) bool
}
```

Runtime type-asserts it; a provider that doesn't implement it → treated as no-thinking.

**Ollama changes** (`internal/llm/ollama/ollama.go`):
- Add cached `supportsThinking *bool` + `sync.Once`/mutex to `Provider` (struct at :18).
- Add `showResponse{ Capabilities []string }` wire type + `probeCapabilities(ctx)`.
- Implement `SupportsThinking(ctx) bool`.
- In `Complete` (:101), gate the flag per the table below.

| `req.Think` | supported | behavior |
|---|---|---|
| `true`  | yes | send `think:true` |
| `true`  | **no** | no-op — run normally, never send `think:true`, log once |
| `false` | yes | send `think:false` (retire the `/no_think` string hack for capable models) |
| any     | no  | never inject `/no_think` |

**Failure / unknown handling (decide now):**
- Probe fails (Ollama down, model not pulled, old Ollama with no `capabilities`) →
  treat as **unsupported** (conservative: never risk the 400), log a warning.
- Belt-and-suspenders: in `Complete`, catch a runtime `"does not support thinking"`
  error → cache unsupported, retry once without `think`.

**Config semantics:** `cfg.LLM.Thinking` (`config.go:66`, `ThinkingEnabled()` :71)
becomes *desired*; capability is *actual*. Configured-on + incapable model →
silently downgrade, one warning, never a hard error.

**User-facing (locked: notify + log + continue):**
- Log via `slog.Warn`.
- Emit a **one-time session-start notice**: *"model `<model>` doesn't support thinking
  — using a request-analysis pass instead."* Needs a `notice` wire event (see Wire
  Protocol below; verify no existing system-message channel first).

---

## Layer 1 — Per-request `Think` (the primitive)

Makes thinking per-call at all; unlocks every dynamic behavior above.

- `internal/llm/provider.go` `Request` struct (:49): add `Think *bool // nil = provider default`.
- Ollama `Complete`: honor `req.Think` (gated by Layer 0) instead of the
  construction-time `p.thinking` field. Keep `p.thinking` as the default when
  `req.Think == nil` for back-compat.
- Any future adapter: implement `SupportsThinking` and map `Think` onto whatever
  the backend calls a reasoning budget. Ollama is the only adapter today.

---

## Layer 2 — `PlanPolicy` + the analysis pass

**Config hook** (`internal/agent/loop.go` `Config` at :17):

```go
// nil → default policy A: think on call #1 of the turn, quiet after.
ThinkPolicy func(llmCallN int, forced bool) bool
// Default impl: func(n int, forced bool) bool { return forced || n == 1 }
```

**In `Run` (`loop.go:152`):** `llmCallN` already resets per turn (:169), so policy A
naturally re-plans on **every new user message** — including a clarification three
turns later. Set `req.Think = ptr(l.plan(llmCallN, l.forceThink))` before each
`Submit` (:206).

**The analysis pass** (unsupported-thinking branch), inside `Run` before the loop:
- One completion built with `Tools: nil` (build via `BuildInput{... Tools: nil}` —
  `internal/context/builder.go:40`) so the model *cannot* call tools.
- Steered by the **analyst role prompt** (see Roles below) — reuse the role's
  `SystemPrompt` as a prompt fragment; **do not spawn a sub-agent** (a second full
  context assembly per turn fights goal G5 on exactly the weak hardware this targets).
- Small `MaxTokens` (a plan, not an essay).
- Feed output two ways:
  1. Inject as a **low-priority, trimmable** plan block into the turn's context
     (new `BuildInput.SystemPlan` field — see Safety + Context Builder below).
  2. Use as **query expansion** into the R-CTX.3 tool-relevance filter (append plan
     text to the embedded query) — better tool selection on models that can't reason
     their way there natively. This is the original payoff.

**Gating — LOCKED:** run the analysis pass on **every turn** when thinking is
unsupported (no per-turn triviality heuristic). The only off-switch is
`plan_mode = off` / `set_plan_mode off` (Layer 3). Simple and predictable; the cost
(one extra call per turn on weak hardware) is accepted, and the plan feeds tool
selection so it's rarely wasted.

---

## Layer 3 — Dynamic controls

Both ride on Layer 1; both auto-disable/redirect per Layer 0.

- **Per-turn force (`/think <message>`):**
  - Add `ForceThink bool` to `protocol.Msg` (`internal/protocol/protocol.go:43`) and
    `NewUserTurnMsg` (:157) / a variant.
  - Add `RunOpts{ ForceThink bool }` → `Loop.Run(ctx, text, opts)` (or a
    `l.SetForceThinkNextTurn` setter). Feeds `forced` into `ThinkPolicy`.
  - Client parses `/think` prefix; where slash commands are handled today
    (cli/TUI) — wire it through `user_turn`.
  - Semantics: force thinking across the **whole turn** (all calls), for hard
    clarifications. On an unsupported model, redirect to "force the analysis pass."
- **Session plan mode toggle:** new wire command `set_plan_mode` with
  `off | plan-only | always`. Swaps the default `ThinkPolicy`. Today this needs a
  daemon restart (construction-time flag); Layer 1 makes it a live switch. Reflect
  unsupported state in `status`/`context`.

---

## Plan safety (two layers, complementary — NOT either/or)

Only the analysis-pass branch needs this.

**A. Low-priority / advisory (always on, structural):**
- New `BuildInput.SystemPlan string` at a **low priority** (propose **P4.5**, below
  scratchpad, above extras) so the budgeter trims it first when context tightens.
- Never let the plan hard-gate tools (same principle as always-include tools,
  R-CTX.3).
- This is the **only** guard available in autonomous sessions (goals, workflows,
  cron, subscribers) — `internal/runtime/builder.go:305`: *"background sessions never
  get ask_human or approval gates."*

**B. Plan-approval checkpoint (interactive only, richer):**
- A **new checkpoint**, distinct from the per-tool `SetApproval` gate
  (`internal/agent/dispatcher.go:27,47`, which fires inside `Dispatch` per tool).
  This fires **once, between the analysis pass and the execution loop**.
- **LOCKED — reuses the generic `human_input` transport**, no dedicated `plan_review`
  event: `internal/runtime/hitl.go` `HITL.Ask(ctx, agentID, question, options)` (:80),
  answered via the existing `human_input_answer` command (`daemon.go:456`). The plan
  is the question text; options are approve / clarify. Interactive-session-gated
  exactly like `ask_human` (`builder.go:305`, `role.Interactive`).
- Loop hook (loop is in `agent`, doesn't know `agentID` — inject a callback):

  ```go
  // internal/agent/loop.go Config — nil = no plan review
  PlanReviewFn func(ctx context.Context, plan string) (PlanDecision, error)
  type PlanDecision struct { Proceed bool; Clarification string }
  ```

  Runtime wires it to `HITL.Ask` in `builder.go` NewLoop construction (:366), only
  when `role.Interactive && f.cfg.HITL != nil`.
- **Reject → clarify folds in Layer 3's clarification concern:** a rejection with
  `Clarification` text becomes a new user message → re-runs analysis. One mechanism
  covers both "catch bad plan" and "let user refine intent."
- **Gate to avoid friction:** the analysis pass runs on *weak* models → more
  questionable plans → more prompts. Make it a mode
  (`plan approval: off | on | on-risky`), default **on-risky**. **LOCKED:**
  `on-risky` reuses the existing `[hitl].require_approval` tool list as its
  definition of "risky" — one source of truth, no second list. It prompts for plan
  approval only when the plan intends to call a tool named in `require_approval`.

| | interactive | autonomous |
|---|---|---|
| Plan approval | ✅ gate + clarify | ❌ no human — unavailable |
| Low-priority / advisory | ✅ trims if tight | ✅ **only** protection |

---

## Config surface (`internal/config/config.go`)

- `LLMConfig.Thinking *bool` — exists (:66). Keep; now "desired," gated by Layer 0.
- Add a `[planning]` block:
  ```toml
  [planning]
  plan_mode     = "plan-only"   # off | plan-only | always
  plan_approval = "on-risky"    # off | on | on-risky
  ```
  ```go
  type PlanningConfig struct {
      PlanMode     string `toml:"plan_mode"`     // off | plan-only | always
      PlanApproval string `toml:"plan_approval"` // off | on | on-risky
  }
  ```
- **LOCKED:** `on-risky` reuses `HITLConfig.RequireApproval` (:40) as the risky-tool
  set — no independent list. Config sets the *default*; the runtime `set_plan_mode`
  command overrides it per-session live (no restart).
- Config is intent only: capability (L0) + interactivity (`builder.go:305`) still
  gate whether approval actually fires.
- `nine.toml`: document defaults.

---

## Wire protocol (`internal/protocol/protocol.go`)

- `Msg` (:43): add `ForceThink bool \`json:"force_think,omitempty"\``.
- New events:
  - `notice` — capability downgrade + any session-level notices (verify no existing
    channel first; `RegisterNotifyUser` is a *tool*, not this).
  - `plan_start` / `plan_end` — surface the analysis pass in the TUI (parallels the
    existing `thinking` events, :362 / `NewThinkingMsg`, :301 `NewThinkingChunkMsg`).
  - **Plan review reuses the generic `human_input` channel** (`daemon.go:456`) — no
    dedicated `plan_review` event (LOCKED).
- New command: `set_plan_mode` (handled in `daemon.go` alongside `user_turn` :402).

---

## Roles (`skills/roles/`, `internal/runtime/roles.go`)

- Add `skills/roles/analyst.md` — the analysis-pass persona. Frontmatter
  `role.tools: ""` (empty) → the roles system *already* expresses "no tool calls"
  (`Role.AllTools=false` + empty `Tools` → allowlist of just `gap_report`+shell,
  `builder.go:311`). Body: "decompose the request, identify what's needed, do not
  act."
- **Usage:** the loop borrows this role's `SystemPrompt` as the analysis-pass prompt
  fragment. It is **not spawned** as a sub-agent. (Keep the door open to spawning it
  later if analysis ever needs its own retrieval tools — but that contradicts
  "no tool calls," so not now.)

---

## Context builder (`internal/context/builder.go`, spec update)

- Add `BuildInput.SystemPlan string` (:40) at **P4.5**.
- Update `spec/contracts/context-builder.md` R-CTX.2 priority table + R-CTX.4
  assembly order to include the plan block.
- Query expansion: allow the loop to pass an augmented query (user text + plan) into
  the embedder for tool ranking; keep the raw user text for history.

---

## Milestones (each independently shippable + tested)

1. **L0 detection** — `ThinkingAware`, Ollama `/api/show` probe + cache + error
   fallback. Tests: capable model → true; incapable → false; probe failure →
   unsupported + no `think:true` sent; runtime-error fallback retries without think.
2. **L1 primitive** — `Request.Think`, Ollama honors it. Tests: `Think=true` on
   capable → `think:true` in body; on incapable → absent, no `/no_think`.
3. **L2 policy (native branch)** — `ThinkPolicy`, default A, wired in `Run`. Tests:
   think on call #1 only; resets per turn; `forced` overrides.
4. **L2 analysis pass (fallback branch)** — no-tool completion, `SystemPlan`
   injection, query expansion. Runs every turn when unsupported (no triviality
   gate). Tests: tools stripped from the analysis request; plan reaches context at
   P4.5 and trims first; runs on every turn unless `plan_mode = off`.
5. **Safety** — `SystemPlan` low-priority (done in 4) + `PlanReviewFn` checkpoint via
   HITL, interactive-gated, reject→clarify re-runs analysis. Tests: autonomous
   session gets no review; interactive reject re-analyses; `on-risky` only prompts on
   flagged tools.
6. **L3 dynamic** — `/think` per-turn force + `set_plan_mode`. Tests: force flag
   flows user_turn→RunOpts→policy; mode swap changes default without restart;
   unsupported model reflects in `status`.
7. **UX + docs** — `notice` on downgrade, `plan_start/end` TUI surfacing, config docs
   in `nine.toml`, contract update.

## Locked decisions

- Policy A (plan-then-execute), per-turn reset → clarifications auto re-plan.
- Loop **phase**, not spawned sub-agent, for the analysis pass.
- Capability unsupported → **notify user + log + continue**, downgrade to analysis pass.
- Plan safety is **two complementary layers**; low-priority is unconditional
  (only guard in autonomous mode), approval is the interactive add-on that also
  absorbs the clarification loop.
- Reuse the roles system (`tools: ""`) and the HITL transport; add no parallel
  machinery.
- `plan_mode` / `plan_approval` are `[planning]` config defaults, live-overridable
  via `set_plan_mode`.
- `on-risky` reuses `[hitl].require_approval` as the risky-tool set (no second list);
  default `plan_approval = on-risky`.
- Analysis pass runs **every turn** when thinking is unsupported (no triviality
  heuristic); off-switch is `plan_mode = off`.
- Plan review reuses the generic `human_input` transport; no dedicated `plan_review`
  event.

## Open questions

_All resolved — see Locked decisions. Ready to implement, starting Milestone 1._
