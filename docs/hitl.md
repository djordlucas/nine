# Human-in-the-Loop (HITL)

Nine can pause mid-task and wait for human input. This covers two cases:

- **LLM-initiated**: Nine calls `ask_human` when it needs clarification, a decision, or approval before it can safely proceed.
- **Automatic gates**: The dispatcher intercepts a configured set of tools before running them and asks for explicit approval.

Both cases route through the same mechanism — `ask_human` — so the TUI has one rendering path and the protocol has one pair of message types.

---

## Session eligibility

HITL tools are only available in **interactive sessions**: conversations started by a human through the TUI. Everything else — background sessions (self-reflection, goal/pursue), sub-agents, `nine query` — is treated as non-interactive and does not have access to `ask_human` or approval gates.

The daemon learns whether a session is interactive from the `new_conversation` protocol message (`Interactive: true`). The TUI sets this flag; the CLI fire-and-forget path does not. Interactive session IDs are persisted in the `interactive_sessions` DB table so the flag survives daemon restarts.

`LoopFactory` gains a second parameter, `interactive bool`. `AgentBuilder.build()` registers HITL tools conditionally on this flag. All background session creation paths pass `false`.

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

1. Check `store.HumanRequestGetPending(agentID)` — if a pending request exists from before a restart, reuse its ID and question, re-emit the question to the TUI.
2. Otherwise create a new row in `human_requests`, compute `expires_at = now + timeout_seconds`.
3. Register a `chan string` in the daemon's in-memory `hitlStore`.
4. Emit `human_input_required` via the session's progress stream.
5. Block: `select` on the channel, `ctx.Done()`, or the expiry time.
6. On answer: update DB row (`status='answered'`, `answer`, `answered_at`), return the answer string to the LLM.
7. On timeout or cancellation: update DB row (`status='timed_out'`), return an error the LLM can reason about: `"no response from human (timed out)"`.

### Restart recovery

The loop's checkpoint is saved after each completed tool call. `ask_human` blocks before its result is recorded in the scratchpad, so the checkpoint predates the call. On daemon restart the loop is rebuilt from that checkpoint and the LLM re-runs the same turn — which re-calls `ask_human`. The handler finds the existing `pending` DB row, re-emits the question to any attached TUI client, and resumes waiting. If the human answered while the daemon was down the row is already `answered` and the handler returns immediately.

---

## Approval gates

Certain tools can be flagged for mandatory human approval before dispatch. This is configured in `nine.toml`:

```toml
[hitl]
timeout_seconds  = 300          # how long to wait before timing out (default 5 min)
require_approval = ["shell", "write_file"]  # empty by default
```

When the dispatcher encounters a tool whose name is in `require_approval`, it calls `ask_human` internally with an auto-generated question before running the handler:

```
Run tool "shell"?

Command: rm -rf /tmp/old

Enter "yes" to proceed, anything else to cancel.
```

The question format is tool-aware: `shell` shows `command`, `write_file` shows `path`, everything else falls back to truncated JSON args.

The answer is checked case-insensitively: a response starting with `"y"` proceeds; anything else returns an error (`"tool shell rejected by user"`) that the LLM receives as a normal tool failure.

Because approval gates are registered alongside `ask_human` — only for interactive sessions — non-interactive sessions are unaffected even if their tool names appear in `require_approval`.

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

## DB schema

Two tables in the PostgreSQL store:

```sql
CREATE TABLE IF NOT EXISTS human_requests (
    id          TEXT PRIMARY KEY,
    agent_id    TEXT NOT NULL,
    question    TEXT NOT NULL,
    options     TEXT,            -- JSON array or NULL
    status      TEXT NOT NULL DEFAULT 'pending',  -- pending | answered | timed_out
    answer      TEXT,
    created_at  TEXT NOT NULL,
    answered_at TEXT,
    expires_at  TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS interactive_sessions (
    id TEXT PRIMARY KEY
);
```

New methods on `memory.Store`:
- `HumanRequestCreate(id, agentID, question string, options []string, expiresAt time.Time) error`
- `HumanRequestGetPending(agentID string) (*HumanRequest, error)`
- `HumanRequestAnswer(id, answer string) error`
- `HumanRequestExpireStale(now time.Time) error` — called at daemon startup
- `InteractiveSessionAdd(id string) error`
- `InteractiveSessionExists(id string) (bool, error)`

At daemon startup, `HumanRequestExpireStale` marks any `pending` row whose `expires_at` is in the past as `timed_out`. Remaining `pending` rows are recovered naturally when their sessions resume and re-call `ask_human`.

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

## Daemon wiring (`hitlStore`)

```go
type hitlStore struct {
    mu      sync.Mutex
    pending map[string]chan string  // requestID → answer channel
}
```

- `ask_human` registers a channel before blocking.
- `human_input_answer` dispatch finds the channel by `RequestID`, sends the answer, and removes the entry.
- On daemon shutdown all blocked `ask_human` calls receive a cancellation via `ctx.Done()` and write `timed_out` to the DB. The pending requests survive in the DB for the next boot.

---

## Implementation order

1. DB schema + `memory.Store` methods
2. Protocol message types + `ProgressEvent` extension
3. `hitlStore` + `human_input_answer` dispatch in daemon
4. `newConversation(interactive bool)` + `interactive_sessions` persistence
5. `LoopFactory` signature change + `AgentBuilder` conditional registration
6. `ask_human` tool handler (restart recovery included)
7. TUI question panel + answer routing
8. Approval gates: dispatcher `approvalFn`, `[hitl]` config, auto-question generation

Steps 1–7 deliver a working `ask_human`. Step 8 adds automatic gates on top.

---

## Limitations

- **Non-interactive sessions cannot ask** — sub-agents and background sessions that need human input must surface it to their parent conversation via their return value or a notification; they cannot call `ask_human` directly.
- **One pending question per session** — `HumanRequestGetPending` returns at most one row. Concurrent `ask_human` calls within the same turn are serialised (first one blocks the loop before the second is issued).
- **No goroutine interruption on timeout** — when a question times out, `ask_human` returns an error to the LLM. Any tool call the LLM subsequently makes may still proceed; timeout is not equivalent to task cancellation.
- **Approval gate scope** — `require_approval` is a global list; there is no per-session or per-argument pattern matching. Fine-grained rules (e.g., "approve `shell` only for destructive-looking commands") are deferred.
