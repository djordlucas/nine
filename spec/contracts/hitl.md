# Contract — Human-in-the-loop (HITL)

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

## R-HITL.1 — Interactive-session gating

HITL is available **only in interactive sessions** — conversations started by a human
through the TUI. Every other session kind (self-reflection, goal/pursue, sub-agents, the
one-shot CLI request path) is **non-interactive** and **MUST NOT** be given `ask_human`
or approval gates, even if a gated tool's name appears in `[hitl].require_approval`.

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

At most **one** pending question per session: concurrent `ask_human` calls within a turn
are serialized because the loop blocks on the first before issuing the second.

---

## R-HITL.4 — Restart recovery

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

---

## R-HITL.5 — Approval gates

Tools named in `[hitl].require_approval` (see [`config.md`](config.md)) trigger an
auto-generated `ask_human` **before** the handler runs. Gates are registered alongside
`ask_human` — i.e. only for interactive sessions (R-HITL.1) — so a non-interactive session
is never prompted even if its tool name is listed.

The auto-question is tool-aware: `shell` shows `command`, `write_file` shows `path`,
everything else falls back to truncated JSON args. Example:

```text
Run tool "shell"?

Command: rm -rf /tmp/old

Enter "yes" to proceed, anything else to cancel.
```

The answer is checked **case-insensitively**: a response starting with `"y"` proceeds;
anything else fails the call with an error the model receives as a normal tool failure
(`"tool shell rejected by user"`). `require_approval` is a single global list — there is
no per-session or per-argument matching.

---

## R-HITL.6 — Protocol messages

Two message types ([`wire-protocol.md`](wire-protocol.md)):

| Type | Direction | Key fields |
|------|-----------|------------|
| `human_input_required` | daemon → client (on the progress stream) | `RequestID`, `Question`, `Options []string`, `TimeoutSeconds` |
| `human_input_answer` | client → daemon | `AgentID`, `RequestID`, `Answer` |

`human_input_required` is delivered on the session's existing progress stream alongside
`tool_start`/`response_chunk`/…; the client surfaces it as a `ProgressEvent` carrying a
`HumanRequest` field. `human_input_answer` is a **top-level** message, **not** a turn: the
daemon routes it straight to the `hitlStore` answer slot for its `RequestID` and updates
the DB row. It **MUST NOT** start a new `agent.Loop.Run`.

---

## R-HITL.7 — Persistence

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

---

## Reference symbols

Planned — no reference implementation yet. Target shape: `hitlStore` and the
`human_input_answer` dispatch in the daemon; `ask_human` and `approvalFn` wiring in
`internal/agent/register_*.go`; the `[hitl]` block in [`config.md`](config.md);
`human_requests`/`interactive_sessions` and their methods on `memory.Store`; the question
panel in `internal/tui`. Design source: [`docs/hitl.md`](../../docs/hitl.md).
