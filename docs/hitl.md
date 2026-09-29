# Human-in-the-loop

Nine can pause mid-task and wait for human input. This is how you approve a
dangerous tool call before it runs, and how Nine asks you a question when it is
stuck. Two cases:

- **LLM-initiated**: Nine calls `ask_human` when it needs clarification, a decision, or approval before it can safely proceed.
- **Automatic gates**: a tool call you have marked as requiring approval is intercepted before it runs, and you are asked to permit or refuse it.

Both cases route through the same mechanism — `ask_human` — so the TUI has one rendering path and the protocol has one pair of message types.

---

## Session eligibility

`ask_human` is only available in **interactive sessions**: conversations started by a human through the TUI. Everything else — background sessions (self-reflection, goal/pursue), sub-agents, `nine query` — is treated as non-interactive and cannot raise its own questions. A sub-agent is a bounded worker, not a conversational participant.

**Approval gates are the exception, and they follow the owning session rather than the asking loop.** A sub-agent spawned by an interactive conversation inherits that conversation's gates at any delegation depth, so delegating a risky tool is not a way around `require_approval`. See [Approval gates](#approval-gates) below.

The daemon learns whether a session is interactive from the `new_conversation` protocol message (`Interactive: true`). The TUI sets this flag; the CLI fire-and-forget path does not. Interactive session IDs are persisted in the `interactive_sessions` DB table so the flag survives daemon restarts.

---

## `ask_human` tool

Available only in interactive sessions.

```json
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

### Behavior

1. If this agent already has a pending question — one asked before a restart — its ID and text are reused rather than a second one being asked.
2. Otherwise the question is recorded as pending, with an expiry `timeout_seconds` from now.
3. The question is emitted on the session's progress stream and the call blocks, waiting for an answer, the turn's cancellation, or that expiry.
4. An answer is recorded with its timestamp and returned to the model as the tool's result.
5. A timeout or cancellation is recorded as `timed_out` and returned to the model as an error it can reason about: `"no response from human (timed out)"`.

### Restart recovery

The loop's checkpoint is saved after each completed tool call. `ask_human` blocks before its result is recorded in the scratchpad, so the checkpoint predates the call. On daemon restart the loop is rebuilt from that checkpoint and the LLM re-runs the same turn — which re-calls `ask_human`. The handler finds the existing `pending` DB row, re-emits the question to any attached TUI client, and resumes waiting. If the human answered while the daemon was down the row is already `answered` and the handler returns immediately.

---

## Approval gates

Certain tools can be flagged for mandatory human approval before dispatch. This is configured in `nine.toml`:

```toml
[hitl]
timeout_seconds  = 300          # how long to wait before timing out (default 5 min)
require_approval = ["shell", "write_file"]  # empty by default
gate_sub_agents  = true         # default; false confines gates to the session's own loop
```

When the dispatcher encounters a tool whose name is in `require_approval`, it calls `ask_human` internally with an auto-generated question before running the handler:

```
Run tool "shell"?

Command: rm -rf /tmp/old

Enter "yes" to proceed, anything else to cancel.
```

The question format is tool-aware: `shell` shows `command`, `tool_write`/`js_eval` show the tool name and its declared capabilities, everything else falls back to truncated JSON args.

**A file write shows the change, not the path.** `write_file` and `edit_file` prompts carry a diff, bounded at 40 lines — a path names which file is about to change and does not answer the only question the gate asks. The diff comes from the tool's own `preview` argument rather than a second implementation, so what the human approves is what the tool then performs, and nothing is written to produce it. A preview that fails never blocks the approval: the prompt falls back to naming the path, since a human deciding with less information beats a tool that cannot run.

### The generated-tools gate

The generated tier (`sandboxed-tools.md` §9.4, `spec/contracts/toolvm.md` R-TVM.14) reuses this same gate for `tool_write` and `js_eval`, but keyed off its own switch, `[tools.agent].require_approval`, rather than the `[hitl]` list — and with a per-call decision the `[hitl]` list does not have:

| `[tools.agent].require_approval` | Gates |
|---|---|
| `on_capability` *(default)* | a write/eval that **declares any capability**, or whose source **imports an external package** — a pure transform with neither passes without a prompt |
| `always` | every write and every eval |
| `never` | nothing except a standing promotion — otherwise the ceiling is the only control |

Two decisions sit underneath that table:

- **A standing promotion always prompts**, including under `never`. A catalogued
  tool runs when a turn calls it; a standing one runs on its own cadence until
  somebody stops it, and the write that starts one is the only moment to refuse.
- **Unparseable arguments gate.** A `tool_write` whose JSON cannot be read counts
  as declaring everything, because fail-closed is the only safe direction for an
  approval decision.

The default gates on **substance, not frequency**: prompting on a capability-free date-formatting tool trains the reflex that defeats the prompt that matters — a tool asking for workspace read. Both switches share the one dispatcher gate, so a generated tool armed here is gated on exactly the same owning-interactive-session terms as any `[hitl]` entry (below); a non-interactive deployment has no gate, so there the ceiling in `[tools.agent.capabilities]` is the whole control.

The answer is checked case-insensitively: a response starting with `"y"` proceeds; anything else returns an error (`"tool shell rejected by user"`) that the LLM receives as a normal tool failure.

A refusal is **terminal** — the loop's tool-retry path (`dispatchWithRetry`, 3 attempts) must not re-dispatch it, or the same human gets asked the same question three times. Both refusal and failure-to-obtain-approval (timeout, cancellation) are wrapped in `agent.ApprovalError`, which the retry loop returns on immediately.

### Asking out of band

A gate needs a human on the other end of a live session. `capability_request` is the
path for when there is not one: the agent records a request, the operator decides later
from the CLI, and nothing about it is synchronous.

The tool can insert a pending row and nothing else — it has no path to a grant, which is
what lets it be advertised to the model at all — and each request lands on the operator's
notification feed. Decide with `nine grants approve <id>` or `nine grants deny <id>`,
`/grants` inside a session, or `nine grants revoke <grant-id>` to withdraw one an earlier
approval conferred. An approval installs on the running daemon without a restart, and
writes nothing to `nine.toml`.

This is the one control a non-interactive deployment still has. The approval gates above
need an owning interactive session, so without one the ceiling in
`[tools.agent.capabilities]` is otherwise the whole story; a request outlives the turn
that made it and waits for whenever an operator looks.

### Gates in sub-agents

A gate is armed for any loop with an **owning interactive session** — the conversation's own loop, plus every sub-agent it spawns unless `gate_sub_agents = false`. A loop with no interactive owner (a goal/pursue session and its children, reflection, `nine query`) is never prompted even if its tool names appear in `require_approval`: there is no human attached, so blocking would hang on a question nobody can see.

Routing keeps three identities apart:

| Identity | Role |
|----------|------|
| **asker** | the loop that hit the gate; keys the `human_requests` row so parallel sub-agents don't collide |
| **owner** | the interactive session whose stream carries the prompt and whose ID answers it |
| **origin** | display attribution — `sub-agent "executor" · <task>` — empty when the owner itself asks |

Emitting to the asker would go nowhere: a sub-agent's ID is not a registered session, so `Daemon.EmitProgress` finds no stream and drops the message, leaving the call blocked until timeout. `HITL.AskFrom` takes asker and owner separately for exactly this reason; `HITL.Ask` is the same call with the two collapsed.

Since a `run_agents` fan-out can put several sub-agents at a gate simultaneously, the TUI queues questions and renders them one at a time, oldest first, with a count of those still waiting.

---

## Protocol

Two new message types in `protocol.Msg`:

| Type | Direction | Key fields |
|------|-----------|------------|
| `human_input_required` | daemon → client (streaming) | `RequestID`, `Question`, `Options []string`, `TimeoutSeconds` |
| `human_input_answer` | client → daemon | `AgentID`, `RequestID`, `Answer` |

`human_input_required` is delivered via the session's existing progress stream alongside `tool_start`, `response_chunk`, etc. The client converts it to a `ProgressEvent` with a `HumanRequest` field.

`human_input_answer` is a top-level protocol message (not a turn). The daemon dispatches it directly to the `hitlStore` channel for the given `RequestID` and updates the DB row.

---

## What is persisted

Two tables carry human-in-the-loop state across restarts:

| Table | Holds | Why it is durable |
|-------|-------|-------------------|
| `human_requests` | one row per question: the asking agent, the text, any options, `pending`/`answered`/`timed_out`, the answer, and an expiry | A question outlives the daemon that asked it. A human who answers while the daemon is down has their answer honoured when it comes back. |
| `interactive_sessions` | the IDs of conversations a human started | Whether a session may ask at all is a property of the session, not of the current process, so it survives a restart. |

At startup, every `pending` row already past its expiry is marked `timed_out`. The rest are recovered when their sessions resume and ask again.

---

## TUI

When `human_input_required` arrives:
- A question panel renders above the input box with the question text and any options.
- The input box prompt changes to `"Answer: "`.
- Submitting sends `human_input_answer` (not a new turn) and clears the panel.
- The question and answer are appended to the chat transcript as a distinct entry type.

If the TUI is not attached when a question is issued (the user is away), the question waits in the DB. When the user reattaches, the session re-emits the pending question automatically and the panel appears.

The status bar shows a `?` badge whenever there is a pending unanswered question in the current session.

---

## Answer routing

The daemon keeps an in-memory map from request ID to a waiting call. A question registers there before it blocks; an incoming answer finds it by request ID, hands over the text, and removes the entry.

On shutdown every blocked question is cancelled and written to the store as `timed_out`. The rows survive for the next boot, which is what makes restart recovery work: the state that matters is in the store, and the map is only how a live answer reaches a live call.

---

## Limits

- **Non-interactive sessions cannot ask** — sub-agents and background sessions that need human input must surface it to their parent conversation via their return value or a notification; they cannot call `ask_human` directly.
- **One pending question per asking loop** — a loop has at most one question outstanding, and concurrent `ask_human` calls within one turn serialise behind each other. Several sub-agents of one conversation can each be waiting at once, which is why the TUI queues them.
- **An approval diff is truncated at 40 lines** — a larger change is approved on a partial view, with the line counts naming what was not shown. A prompt that scrolls is one nobody reads, which is the failure the bound exists to prevent.
- **No goroutine interruption on timeout** — when a question times out, `ask_human` returns an error to the LLM. Any tool call the LLM subsequently makes may still proceed; timeout is not equivalent to task cancellation.
- **Approval gate scope** — the `[hitl].require_approval` list is global; there is no per-session or per-argument pattern matching for it. Fine-grained rules (e.g., "approve `shell` only for destructive-looking commands") are deferred. The generated-tools gate (`[tools.agent].require_approval = "on_capability"`) is the one per-argument exception, and only for `tool_write`/`js_eval`.
