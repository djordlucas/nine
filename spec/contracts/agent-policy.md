# Contract — agent policy

**Status:** Planned · **Depends on:** agent loop, llm provider (queue), dispatcher, context builder, agent worker · **Used by:** agent worker, roles (planned, `docs/roles.md`)

A **policy** is the reasoning strategy of a session: the pure decision logic that turns
turn state and events into the next action. The **host** (the loop shell owned by the
agent worker) executes those actions — it owns the LLM queue, tool dispatch,
checkpointing, and all durable state. Today's ReAct inner loop
([`agent-loop.md`](agent-loop.md)) becomes the **default policy** behind this seam.

The seam exists to make the reasoning strategy pluggable *later* (a plan-then-execute
policy, a critic/verifier loop) without re-implementing platform commodities, and to
keep the door open to hosting a policy out-of-process or in a sandboxed runtime. To
preserve that option, the contract is deliberately **transport-shaped**: every
host↔policy interaction is a single exchange of JSON-serializable values, with no live
references crossing the boundary (R-POLICY.4).

**Relationship to [`agent-loop.md`](agent-loop.md).** When this contract ships,
`agent-loop.md` splits conceptually in two: the *host shell* obligations (R-LOOP.4
retries, R-LOOP.5 empty-answer fallback, R-LOOP.6 progress callbacks, R-LOOP.7
checkpoint unit, R-LOOP.8 priority) move to the host side of this seam, and the *ReAct
strategy* (R-LOOP.1 turn structure, R-LOOP.2 scratchpad) becomes the specification of
the default `react` policy (R-POLICY.9). Until then, `agent-loop.md` remains the
authoritative description of the built system.

---

## R-POLICY.1 — the ownership split: the policy decides, the host executes

Responsibilities are split as follows. A conforming implementation **MUST NOT** move a
host-side responsibility into a policy (a policy that talks to the database or an LLM
provider directly is non-conformant, whatever its behavior).

| Concern | Owner |
|---------|-------|
| Reasoning strategy: what to do next (think / act / answer) | **Policy** |
| Intra-turn working state (the scratchpad generalized) | **Policy** (held via the host, R-POLICY.3) |
| LLM submission, concurrency, priority (I2) | **Host** |
| Tool dispatch: routing, retries, approval gates, output cap (I9, [`dispatcher.md`](dispatcher.md)) | **Host** |
| Context assembly under the token budget ([`context-builder.md`](context-builder.md)) | **Host** |
| Cross-turn history and checkpoints (I5, R-LOOP.7) | **Host** |
| Turn serialization (I1), stall detection, notifications ([`agent-worker.md`](agent-worker.md)) | **Host** |
| Progress/streaming events to the client (R-PROTO.3) | **Host** |
| Sub-agent spawning, workflows, goals, HITL ([`orchestration.md`](orchestration.md), [`hitl.md`](hitl.md)) | **Host** (reached by the policy only as tools) |

A policy never sees tool display names (I8), never chooses its own queue priority
(R-LOOP.8), and never touches `memory.Store`, `llm.Queue`, or `plugin.Manager`.

---

## R-POLICY.2 — the decision exchange

A policy implements one operation, invoked by the host once per event:

```interface
// Policy is the reasoning strategy of a session. It is pure decision logic:
// it MUST NOT perform I/O and MUST NOT retain references to host resources.
type Policy interface {
    // Name is the stable registry identifier (R-POLICY.8).
    Name() string

    // Decide consumes one event and the current turn state, and returns the
    // next action plus the updated turn state. It is called serially per
    // session (I1) and must be safe to call on a state that was serialized
    // and deserialized at any prior boundary (R-POLICY.3).
    Decide(state TurnState, event Event) (Action, TurnState, error)
}
```

**Events** (host → policy). Exactly one of the variants is set:

```schema
Event =
  | turn_start   { text: string,                      // the turn's input text
                   source: "user" | "idle" | "notification" | "sub_agent" }
  | llm_response { text: string,
                   tool_calls: [{id, name, input}],
                   stop_reason: "end_turn" | "tool_use" | "max_tokens" }
  | tool_results { results: [{call_id, name, output: string, is_error: bool}] }
```

`turn_start.source` mirrors the four turn producers of `overview.md` §4. A
`tool_results` event carries the outcome of **every** call requested by the preceding
`call_tools` action, in request order; a failed call (after host-side retries,
R-POLICY.5) arrives with `is_error: true` and the instructive error text as `output` —
failure is an observation, never a policy-visible exception.

**Actions** (policy → host). Exactly one of the variants is set:

```schema
Action =
  | llm        { turn_messages: [Message],   // the turn's working exchange,
                                             // appended after history at assembly
                 max_tokens?: int }          // 0/absent → the loop default
  | call_tools { calls: [{id?, name, input}] }
  | answer     { text: string }
```

The exchange grammar per turn is:

```text
host: Decide(state₀, turn_start)        → action₁, state₁
      execute action₁                   → event₁   (llm → llm_response; call_tools → tool_results)
      Decide(state₁, event₁)            → action₂, state₂
      …
      until Decide returns answer       → host commits the turn (R-POLICY.6)
```

An `answer` action produces no further `Decide` calls for the turn. There is no
reference-default cap on exchanges per turn (matching the current inner loop);
cancellation and timeouts bound it (R-POLICY.6).

---

## R-POLICY.3 — turn state is externalized

`TurnState` is the policy's intra-turn working memory (the generalization of the ReAct
scratchpad). Its schema is **policy-owned and opaque to the host**, with these hard
requirements:

- It **MUST** round-trip through JSON: for any state a policy can produce,
  `deserialize(serialize(state))` followed by any `Decide` call **MUST** yield the same
  action and successor state as calling `Decide` on the original. (This is the testable
  form of "no hidden state": an in-process host **MAY** pass typed state without
  serializing every call, but a conformance test MAY insert a round-trip at *any*
  boundary and observe no behavioral difference.)
- The **host owns persistence**: the checkpoint after every turn (I5) becomes
  `{history, policy, policy_state}`, where `policy` is the policy name and
  `policy_state` its serialized `TurnState`. On a final answer the policy's turn state
  is reset to empty before checkpointing (mirroring scratchpad clearing, R-LOOP.2); a
  mid-turn checkpoint (disconnect/restart) carries the live state so the turn is
  reconstructable.
- Cross-turn `history` remains **host-owned** (R-LOOP.3). A policy reads the
  conversation only through what the host assembles into LLM requests; it **MUST NOT**
  keep its own copy of history across turns inside `TurnState`.

---

## R-POLICY.4 — purity and the single-exchange discipline

These rules are what keep the seam liftable to another transport or runtime later.
They are conformance requirements *now*, while everything is in-process:

- A policy **MUST NOT** perform I/O of any kind in `Decide`: no network, no filesystem,
  no database, no clock-driven blocking. Anything a policy needs from the world arrives
  as an `Event`; anything it wants done leaves as an `Action`.
- The interface **MUST NOT** grow parameters that cannot cross a process boundary:
  no callbacks, no channels, no live object references. Host services are reached by
  *returning actions*, never by *calling into* the host.
- Current time, random values, and any other nondeterminism a policy depends on
  **MUST** be delivered through events or state, never sampled directly. (The reference
  host already stamps the current time and the session id into the assembled system
  prompt host-side — R-LOOP.1 — so the default policy needs no clock.)
- `Decide` **SHOULD** be deterministic given `(state, event)`. See R-POLICY.10.

---

## R-POLICY.5 — host execution semantics

How the host executes each action, preserving the existing contracts unchanged:

- **`llm`** — the host assembles the request via the context builder
  ([`context-builder.md`](context-builder.md)): system core (+ time), extras,
  self-model, relevance-filtered tools, history, then the action's `turn_messages`,
  all under the token budget. `turn_messages` occupy the scratchpad's budget slot
  (R-CTX priority 4) and are trimmed oldest-first under pressure. The request is
  submitted through the queue (I2) at the **session owner's** priority (R-LOOP.8);
  the policy cannot influence priority. Streamed chunks flow to the client via the
  host's `OnChunk`; the policy sees only the complete `llm_response`.
- **`call_tools`** — the host dispatches each call through the dispatcher
  ([`dispatcher.md`](dispatcher.md)) sequentially in request order, applying approval
  gates, the per-call retry discipline (3 attempts total, R-LOOP.4), and the
  output cap. Results are batched into one `tool_results` event.
- **`answer`** — the host appends the user text and answer to `history`, resets the
  policy's turn state, checkpoints (I5), and completes the turn. If `text` is blank,
  the host **MUST** apply the empty-answer fallback (R-LOOP.5), building the visible
  failure message from the `is_error` tool results it observed during the turn — a
  policy cannot cause a silent blank turn.
- **Progress events** — the host emits the R-LOOP.6 callbacks from the action stream:
  `OnThinking` before executing each `llm` action, `OnToolStart`/`OnToolEnd` around
  each dispatched call, `OnContextUpdate` after each assembly. Policies do not emit
  progress; equivalent observable client behavior (R-PROTO.3) is required regardless
  of policy.
- The host counts dispatched tool calls per turn for stall detection
  (`LastRunToolCount` semantics, R-LOOP.7); a turn whose actions included no
  `call_tools` counts as a no-tool turn.

---

## R-POLICY.6 — turn lifecycle, cancellation, and failure containment

- **Serialization.** `Decide` is invoked by the session's serial worker only; it is
  never called concurrently for the same session (I1).
- **Cancellation.** When the turn's cancellation handle fires (client disconnect
  handling, sub-agent timeout), the host aborts the in-flight action and **MUST NOT**
  call `Decide` again for that turn. The last checkpointed state stands (I5).
- **Policy failure.** If `Decide` returns an error, panics (the host **MUST** recover
  it), or returns a malformed action (no variant set, unknown variant), the host fails
  the **turn** with a visible error — surfaced like a tool failure, not swallowed —
  and the **session survives**: history and the last checkpoint are intact, and the
  next turn proceeds normally. A policy defect is contained to a turn the way a plugin
  crash is contained to a subprocess (I9).
- **Unknown tools.** A `call_tools` naming a tool that is not registered for this
  session dispatches to the normal "unknown tool" error path and comes back as an
  `is_error` result; it is not a policy failure.

---

## R-POLICY.7 — context assembly stays host-side

The token budget, priority classes, tool relevance filtering, and the 4-chars/token
model of [`context-builder.md`](context-builder.md) are platform behavior and apply
identically to every policy. A policy **MUST NOT** count tokens or attempt its own
trimming as a conformance matter (it MAY shape `turn_messages` however it likes, but
the budget is enforced after it). Per-policy assembly *preferences* (reordered
priorities, different trim disciplines) are a **MAY** for a future revision of this
contract and are out of scope for v1.

---

## R-POLICY.8 — registry, selection, and the code boundary

- Policies are identified by a stable **name** and resolved through a
  `PolicyRegistry` built at daemon boot.
- **v1 ships exactly one policy: `react`** (R-POLICY.9). The registry exists so that
  selection plumbing is in place, not to ship alternatives.
- Selection is per-session at loop-build time: from the session's role once roles ship
  (`docs/roles.md` — a role MAY name a policy), else from config
  (`policy.default`, default `"react"`). An unknown policy name **MUST** resolve to
  the default with a logged warning, never an error — the system degrades to today's
  behavior (mirroring R-ROLE.9's fallback discipline).
- **Policies are compiled into the binary.** They are code, and Nine's executable
  shape is fixed: a policy **MUST NOT** be loadable from disk at runtime, and agents
  **MUST NOT** be able to author, modify, or select policies via tools (extends
  I10/N1–N3; self-improvement remains skills only). Changing the available policies
  means editing the repo and rebuilding, exactly like plugins.

---

## R-POLICY.9 — the default `react` policy

The `react` policy is the ReAct inner loop of [`agent-loop.md`](agent-loop.md)
re-expressed as decisions, and it anchors backward compatibility:

- Its `TurnState` **is** the scratchpad: `[]ScratchpadEntry{Thought, ToolName,
  ToolCallID, ToolArgs, Observation}` (R-LOOP.2).
- On `turn_start` it decides `llm` with empty `turn_messages`. On an `llm_response`
  with tool calls it decides `call_tools` (all calls, in order). On `tool_results` it
  appends scratchpad entries (thought text on the first entry only) and decides `llm`
  with the scratchpad expanded to messages exactly as R-LOOP.2 specifies. On an
  `llm_response` without tool calls it decides `answer` with the response text.
- **Behavior gate:** for any input, the sequence of LLM requests, tool dispatches, and
  the final answer **MUST** be observably identical to the pre-seam loop (R-LOOP.1
  through R-LOOP.5, with .4/.5 now host-side per R-POLICY.5). The refactor is
  behavior-preserving by construction; the regression suite from `agent-loop.md`
  conformance is the gate.
- **Checkpoint migration:** a legacy checkpoint `{history, scratchpad}` (no `policy`
  field) **MUST** load as `policy: "react"` with the scratchpad as its `TurnState`.
  New checkpoints write the R-POLICY.3 envelope.

---

## R-POLICY.10 — determinism and replay

Because all inputs arrive as events (R-POLICY.4), a turn is characterized by
`(initial state, event sequence)`. Re-running `Decide` over a recorded event sequence
**SHOULD** reproduce the identical action sequence. The host **MAY** record per-turn
event logs to enable this as a debugging/verification tool ("replay a turn"); the
`react` policy **MUST** be deterministic in this sense (it contains no randomness).
A future policy that requires randomness **MUST** receive its seed via `turn_start`
or `TurnState` so replay remains possible.

---

## Reference symbols (planned)

- `internal/agent/policy.go` — `Policy`, `Event`, `Action`, `TurnState` (new).
- `internal/agent/policy_react.go` — the `react` policy, extracted from `Loop.Run`
  (`internal/agent/loop.go`).
- `internal/agent/loop.go` — `Loop` becomes the host shell: executes actions, owns
  history, applies R-POLICY.5/.6; `SaveState`/`LoadState` carry the R-POLICY.3
  envelope.
- `internal/runtime/builder.go` — resolves the session's policy (config, later role)
  and passes it to the loop; `internal/runtime/daemon.go` — `PolicyRegistry`
  construction at boot.
