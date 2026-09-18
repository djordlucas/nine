# Contract — human-in-the-loop (HITL)

**Status:** Built · **Depends on:** [`memory-store.md`](memory-store.md), [`wire-protocol.md`](wire-protocol.md), [`dispatcher.md`](dispatcher.md), [`agent-worker.md`](agent-worker.md), [`config.md`](config.md) · **Used by:** daemon, TUI

> **Built.** Implemented in the reference tree: `internal/runtime/hitl.go` (`HITL`,
> `NewHITL`, `ExpireStale`, approval gates), `internal/agent/register_human.go`
> (`ask_human` / `AskHumanDef`), the `human_requests` / `interactive_sessions` tables, and
> the `human_input_*` wire messages. Wired in `cmd/nine/daemon.go` (`ConfigureHITL`).

Nine can pause mid-turn and wait for a human. Two cases, one mechanism:

- **LLM-initiated** — the model calls `ask_human` when it needs clarification, a
  decision, or approval before it can proceed.
- **Automatic gates** — the dispatcher intercepts a configured set of tools and demands
  explicit approval before running them, by issuing an `ask_human` itself.

Both route through `ask_human`, so the TUI has one rendering path and the protocol has
one pair of message types.

---

## R-HITL.1 — interactive-session gating

`ask_human` is available **only in interactive sessions** — conversations started by a
human through the TUI. Every other session kind (self-reflection, goal/pursue,
sub-agents, the one-shot CLI request path) is **non-interactive** and **MUST NOT** be
given `ask_human`. A sub-agent is a bounded worker, not a conversational participant: it
may be stopped for permission, but it never gets to interrogate the user itself.

**Approval gates follow the owning session, not the asking loop** (R-HITL.5). A
sub-agent spawned by an interactive conversation inherits that conversation's gates, at
any delegation depth, so delegation cannot be used to run a `[hitl].require_approval`
tool unprompted. A sub-agent of a *non-interactive* root stays ungated — there is no
human attached, so blocking would hang on a question nobody can see.

The daemon learns interactivity from the `new_conversation` message's `Interactive` flag
(see [`wire-protocol.md`](wire-protocol.md) R-PROTO.2). The TUI sets it; the
fire-and-forget CLI path does not; background-session and sub-agent creation paths pass
`false`. Interactive session IDs **MUST** be persisted (table `interactive_sessions`) so
the flag survives a daemon restart. The loop factory takes an `interactive bool`, and the
agent builder registers HITL tools **conditionally** on it.

---

## R-HITL.2 — `ask_human` tool

Registered only for interactive sessions; available at all depths within such a session.

```schema
{
  "name": "ask_human",
  "description": "Pause and request input from the human operator before proceeding.",
  "input_schema": {
    "type": "object",
    "required": ["question"],
    "properties": {
      "question": { "type": "string" },
      "options":  { "type": "array", "items": { "type": "string" },
                    "description": "Optional multiple-choice options to present" }
    }
  }
}
```

---

## R-HITL.3 — `ask_human` behavior

A call **MUST** proceed as follows:

1. Check `HumanRequestGetPending(agentID)`. If a `pending` row exists (e.g. from before a
   restart), **reuse** its ID and question rather than creating a duplicate.
2. Otherwise create a `human_requests` row with `expires_at = now + timeout_seconds`.
3. Register an **answer slot** in the daemon's in-memory `hitlStore`, keyed by request ID
   — a one-shot hand-off the answer dispatch (R-HITL.6) delivers into.
4. Emit `human_input_required` on the session's progress stream.
5. Block until whichever happens first: the answer arrives, the turn is cancelled, or the
   expiry deadline passes.
6. On answer — mark the row `answered` (`answer`, `answered_at`) and return the answer
   string to the LLM as the tool result.
7. On timeout or cancellation — mark the row `timed_out` and return an error the model can
   reason about (`"no response from human (timed out)"`). Timeout returns an error to the
   model; it is **not** task cancellation — a subsequent tool call may still proceed.

At most **one** pending question per **asking agent**: concurrent `ask_human` calls within
one loop are serialized because the loop blocks on the first before issuing the second.
A session may nonetheless have several questions outstanding at once, one per asker, when
parallel sub-agents each hit an approval gate (R-HITL.5). Step 1's pending-row reuse is
therefore keyed by the **asker's** agent ID, never the owning session's — keying it by
session would make two concurrent sub-agents collide on one row, answering one twice
while the other waits out its timeout.

---

## R-HITL.4 — restart recovery

The checkpoint is written after each completed tool call, and `ask_human` blocks **before**
its result is recorded — so the checkpoint predates the call. On restart the loop is
rebuilt from that checkpoint and the model re-runs the turn, re-calling `ask_human`. By
R-HITL.3 step 1 the handler finds the existing `pending` row, re-emits the question to any
attached client, and resumes waiting. If the human answered while the daemon was down, the
row is already `answered` and the handler **MUST** return that answer immediately without
re-prompting.

At startup, `HumanRequestExpireStale(now)` **MUST** mark any `pending` row whose
`expires_at` is in the past as `timed_out`. Still-valid pending rows are recovered
naturally when their sessions resume and re-call `ask_human`.

**Sub-agent gates do not recover this way.** Reuse is keyed by the asker (R-HITL.3), and a
sub-agent's ID is a fresh UUID per spawn — so a restart mid-gate re-runs the parent's turn,
spawns a *new* sub-agent, and raises a *new* question. The orphaned row stays `pending`
until its deadline passes and a later `ExpireStale` collects it; nothing queries it in the
meantime and no stale question is re-emitted. The human simply sees the question again
once the re-spawned sub-agent reaches the same gate. Making these resumable would require
stable sub-agent identities across restarts, which the delegation path does not have.

---

## R-HITL.5 — approval gates

Tools named in `[hitl].require_approval` (see [`config.md`](config.md)) trigger an
auto-generated `ask_human` **before** the handler runs. A gate is armed for any loop with
an **owning interactive session**: the conversation's own loop, and — unless
`[hitl].gate_sub_agents` is `false` — every sub-agent it spawns, at any depth. A loop with
no interactive owner is never prompted even if its tool name is listed.

The generated tier (`spec/contracts/toolvm.md` R-TVM.14) arms `tool_write`/`js_eval` on this
same gate, keyed off `[tools.agent].require_approval` instead of the `[hitl]` list, and on
the identical owning-interactive-session terms. Its `on_capability` default is a **per-call**
decision — a write whose declared capabilities are empty is not prompted — which is the one
exception to the "single global list, no per-argument matching" rule below.

### Routing a sub-agent's gate

A sub-agent has no session of its own, so its prompt **MUST** be emitted on the **owning
session's** progress stream — emitting to the sub-agent's own ID would find no session,
drop the message, and leave the call blocked until timeout. Three identities are in play
and **MUST NOT** be conflated:

| Identity | Role |
|----------|------|
| **asker** | the loop that hit the gate; keys the `human_requests` row (R-HITL.3) |
| **owner** | the interactive session carrying the prompt and answering it; the message's `AgentID` |
| **origin** | display attribution (`sub-agent "executor" · <task>`); empty when the owner itself asks |

A client answers with the **owner**'s ID and the request ID, so it needs no knowledge of
sub-agent IDs.

The auto-question is tool-aware: `shell` shows `command`, `write_file` shows `path`,
everything else falls back to truncated JSON args. Example:

```text
Run tool "shell"?

Command: rm -rf /tmp/old

Enter "yes" to proceed, anything else to cancel.
```

### Refusal is terminal

The answer is checked **case-insensitively**: a response starting with `"y"` proceeds;
anything else fails the call with an error the model receives as a normal tool failure
(`"tool shell rejected by user"`). `require_approval` is a single global list — there is
no per-session or per-argument matching.

That failure **MUST NOT** be retried by the loop's tool-retry path: a refusal is a
decision, not a transient fault, and re-dispatching re-prompts the same human for the
same call — which a parallel sub-agent fan-out multiplies into a barrage. The same holds
when approval cannot be obtained at all (the question timed out, or the turn was
cancelled). The reference tree marks both with `agent.ApprovalError`, which
`dispatchWithRetry` returns on immediately instead of retrying.

---

## R-HITL.6 — protocol messages

Two message types ([`wire-protocol.md`](wire-protocol.md)):

| Type | Direction | Key fields |
|------|-----------|------------|
| `human_input_required` | daemon → client (on the progress stream) | `RequestID`, `Question`, `Options []string`, `TimeoutSeconds`, `Origin` |
| `human_input_answer` | client → daemon | `AgentID`, `RequestID`, `Answer` |

`human_input_required` is delivered on the **owning** session's existing progress stream
alongside `tool_start`/`response_chunk`/…; the client surfaces it as a `ProgressEvent`
carrying a `HumanRequest` field. `AgentID` is always that owning session, even when a
sub-agent of it is the asker (R-HITL.5), so a client replies with the ID it already has.
`Origin` attributes such a question (`sub-agent "executor" · <task>`) and is **omitted**
when the session's own loop asks; it is display metadata only and **MUST NOT** be echoed
back in `human_input_answer`. `human_input_answer` is a **top-level** message, **not** a turn: the
daemon routes it straight to the `hitlStore` answer slot for its `RequestID` and updates
the DB row. It **MUST NOT** start a new `agent.Loop.Run`.

---

## R-HITL.7 — persistence

Two tables (see [`memory-store.md`](memory-store.md) R-MEM.2):

```sql
CREATE TABLE IF NOT EXISTS human_requests (
    id          TEXT PRIMARY KEY,
    agent_id    TEXT NOT NULL,
    question    TEXT NOT NULL,
    options     TEXT,                              -- JSON array or NULL
    status      TEXT NOT NULL DEFAULT 'pending',   -- pending | answered | timed_out
    answer      TEXT,
    created_at  TEXT NOT NULL,
    answered_at TEXT,
    expires_at  TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS interactive_sessions (
    id TEXT PRIMARY KEY
);
```

Store methods (daemon-private; never exposed as agent tools, per I4):
`HumanRequestCreate`, `HumanRequestGetPending`, `HumanRequestAnswer`,
`HumanRequestExpireStale`, `InteractiveSessionAdd`, `InteractiveSessionExists`.

The in-memory side is a **guarded** map from `RequestID` to a one-shot **answer slot**:
`ask_human` registers a slot before blocking; `human_input_answer` finds it by `RequestID`,
delivers the answer, and removes the entry. On daemon shutdown all blocked calls are
cancelled and write `timed_out`; the rows survive in the DB for the next boot.

---

## R-HITL.8 — TUI rendering

On `human_input_required` the TUI **MUST**: render a question panel above the input box
with the question and any options; change the input prompt to `Answer:`; on submit send
`human_input_answer` (not a new turn) and clear the panel; append the question/answer to
the transcript as a distinct entry. If no client is attached when the question is issued,
it waits in the DB and is re-emitted automatically when the user reattaches (R-HITL.4). The
status bar **SHOULD** show a `?` badge while a pending unanswered question exists in the
current session.

A question carrying `Origin` **MUST** be rendered with that attribution — an approval
prompt for a tool the user never saw requested is unactionable without knowing which
sub-agent wants it.

### Queueing

Parallel sub-agents can each raise a gate at once (R-HITL.5), so a session may have
several unanswered questions. The client **MUST** queue them and render **one at a
time**, oldest first: a question arriving while another is on screen waits rather than
replacing the one the user is reading. Answering the head uncovers the next and keeps the
`Answer:` prompt active until the queue drains. The status bar **SHOULD** report how many
remain behind the visible one. When a turn ends with questions still unanswered (they
timed out with it), the whole queue is discarded — every asker's wait ended too.

---

## Reference symbols

`HITL` (`Ask`, `AskFrom`, `Answer`, `ExpireStale`) and the `human_input_answer` dispatch
in `internal/runtime/hitl.go`; `ask_human` in `internal/agent/register_human.go`; the
gate wiring — `gateCtx`, `subGate`, `registerAskHuman`, `registerApprovalGates` — in
`internal/runtime/builder.go`; `agent.ApprovalError` and its handling in
`dispatchWithRetry` (`internal/agent/`); the `[hitl]` block in
[`config.md`](config.md); `human_requests`/`interactive_sessions` and their methods on
`memory.Store`; the question queue (`chatState.humanQueue`, `pendingHuman`, `popHuman`)
in `internal/tui`. Design source: [`docs/hitl.md`](../../docs/hitl.md).
