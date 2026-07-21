# Contract — Wire Protocol (daemon ↔ client)

**Status:** Built · **Depends on:** nothing (shared package) · **Used by:** daemon, CLI, TUI

The protocol package defines the messages exchanged between the daemon and any client.
It depends on neither the daemon nor the client, so client code never pulls in the
runtime. Messages are **newline-delimited JSON** over an `AF_UNIX` stream socket: one
JSON object per line.

---

## R-PROTO.1 — Envelope

There is a single flat envelope type, `Msg`. Every message — request or event — is one
`Msg`. Implementations **MUST** use one flat object with a `type` discriminator and
omit-empty fields, not a tagged union per direction.

```schema
Msg {
  type              string   // discriminator; required
  agent_id          string   // session this message concerns
  text              string   // free text / JSON payload depending on type
  id                string
  name              string
  tool_name         string
  tool_display_name string   // UI only — see R-PROTO.6
  tool_input        json     // raw JSON args
  tool_output       string
  ts                int64    // unix millis
  context_used      int
  context_budget    int
  sub_agent_id      string
  status            string   // sub_agent_end: "done" | "failed" | "timed_out"
  role              string   // ok/conversation_id: session's role; sub_agent_*: sub-agent's leaf role
  llm_call_n        int      // thinking: 1-based LLM call count within the turn
  think             bool     // thinking: this call streams reasoning (thinking_chunk)
  replay_events     []Msg    // attach response only
  pending_response  string   // attach response only
}
```

---

## R-PROTO.2 — Client → daemon messages

| `type` | Fields | Meaning |
|--------|--------|---------|
| `new_conversation` | (`interactive` flag, see hitl) | create a conversation; daemon assigns an ID |
| `attach` | `agent_id` | reconnect to an existing session |
| `user_turn` | `agent_id`, `text`, (`force_think` flag) | send a user message (one turn); `force_think` forces reasoning for this turn (`/think`) |
| `set_plan_mode` | `agent_id`, `text` = `off` \| `plan-only` \| `always` | change a session's reasoning mode live, no restart |
| `status` | — | request daemon status |
| `context` | `agent_id` | request a session's assembled-context breakdown, **no LLM call** |
| `list_goals` | — | request goal list |
| `list_reflections` | — | request reflection history |
| `list_workflows` | — | request workflow list |
| `list_tools` | — | request all tools grouped by plugin |
| `workflow_stop` | `text` = workflow ID | live-cancel a workflow |
| `workflow_fail` | `text` = ID or `--all` | post-mortem fail |
| `plugin_call` | `tool_name`, `tool_input` | invoke a tool directly, **bypassing the LLM** |

`plugin_call` (R-PROTO.5) is the only way to reach a tool without an agent loop; the CLI
uses it for `nine workflow fail` when the daemon is up and for diagnostics.

---

## R-PROTO.3 — Daemon → client messages

**Lifecycle / results:**

| `type` | Fields | Meaning |
|--------|--------|---------|
| `conversation_id` | `id`, `role` | conversation created; `role` is the session's resolved role |
| `ok` | `agent_id`, `role`, `replay_events`, `pending_response` | attach succeeded (+ replay snapshot); `role` is the session's resolved role |
| `response` | `agent_id`, `text` | the assistant's final answer |
| `done` | `agent_id` | turn complete |
| `error` | `text` | failure |
| `status` / `list_*` | `text` = JSON payload | the requested read (StatusInfo / array) |
| `context` | `text` = JSON `ninectx.Report` | per-section token breakdown + assembled prompt/messages |
| `workflow_stop` / `workflow_fail` | `text` = `"stopped"` / `"failed"` | operator-command result |
| `set_plan_mode` | `text` = `"plan mode: <mode>"` | mode change acknowledged (or `error` on an unknown mode) |

**Streaming progress** (emitted during a turn, before `response`+`done`):

| `type` | Fields | Meaning |
|--------|--------|---------|
| `thinking` | `llm_call_n`, `think` | a new LLM round-trip started within this turn; `think` reports whether it will stream `thinking_chunk` |
| `stage` | `text` | the turn entered a named waiting phase; empty `text` clears it |
| `plan_start` | — | the no-tool request-analysis (planning) pass began (thinking-degraded models) |
| `plan_end` | — | the request-analysis pass finished |
| `notice` | `text` | a session-level notice (e.g. native thinking unavailable, planning pass used) |
| `context_update` | `context_used`, `context_budget` | context assembled |
| `tool_start` | `tool_name`, `tool_display_name`, `tool_input`, `ts` | tool call started |
| `tool_end` | `+ tool_output` | tool call finished |
| `response_chunk` | `text` | a streamed text token |
| `thinking_chunk` | `text` | a streamed reasoning ("thinking") token; ephemeral, never journaled |
| `sub_agent_start` | `sub_agent_id`, `text` (task), `role`, `ts` | a sub-agent spawned; `role` is its resolved leaf role, for display |
| `sub_agent_end` | `+ status` | a sub-agent finished (`done`/`failed`/`timed_out`); carries `role` too |

A conforming daemon **MUST** emit `thinking`/`context_update`/`tool_*`/`response_chunk`
during a turn and **MUST** terminate every turn with `response` then `done` (or
`error`). `thinking_chunk` is emitted only when the configured provider surfaces
extended thinking (`[llm] thinking`, Ollama-only today); clients that don't
recognise it **MUST** ignore it. `thinking.think` is false both when the policy declines
to think and when the model cannot, so a client **MUST NOT** promise the user reasoning it
will never receive. `plan_start`/`plan_end` bracket the request-analysis pass and appear
only on thinking-degraded turns; `notice` is informational and session-level. `stage`
names the phases of a turn that precede any model output — memory recall, context
assembly, a queue wait, waiting on the first token — so a client need not render dead
air; its labels are display text and a client **MUST NOT** branch on them. All four are
optional — clients that don't recognise them **MUST** ignore them. The `role` on
`sub_agent_start`/`sub_agent_end` is likewise a display affordance (the sub-agent's
resolved leaf role); it never reaches the LLM and a client **MUST NOT** branch on it.

---

## R-PROTO.4 — Read payloads

`status` returns `StatusInfo` JSON-encoded in `text`:

```schema
StatusInfo { agents []AgentInfo, sub_agents []SubAgentInfo, plugins []string, uptime string }
AgentInfo  { id string, name string, role string, plan_mode string }  // plan_mode: off | plan-only | always
SubAgentInfo { id string, description string, role string }  // role: the sub-agent's resolved leaf role
```

`list_tools` returns `[]ToolSummary{ plugin, name, description }`. The `list_*` reads are
thin proxies over the store (the daemon does not interpret the data; see I4 — these are
daemon-private reads exposed only as protocol queries, never as agent tools).

---

## R-PROTO.5 — `plugin_call` bypasses the LLM

`plugin_call` **MUST** invoke the named tool directly through the dispatcher/manager and
return its output, with no agent loop, no context assembly, and no LLM call. It is the
mechanism behind daemon-down operator commands and direct tooling.

---

## R-PROTO.6 — Display names never reach the LLM (I8)

`tool_display_name` is a UI affordance carried to the client only. The canonical
`tool_name` is what the model sees and calls. An implementation **MUST NOT** send display
names into LLM context.

---

## R-PROTO.7 — `EnsureDaemon` and auto-start

The client entry point **MUST** transparently auto-start the daemon if the socket is
dead, then connect:

1. Try to dial the configured socket. If it connects, use it.
2. Otherwise, start the daemon (the reference impl re-execs the same binary into a
   daemon mode), wait for the socket to become connectable (bounded retry), then dial.

The same `nine` binary is therefore both client and daemon. Closing a client never stops
the daemon (G1).

---

## R-PROTO.8 — Connection & concurrency

- The daemon accepts connections in a loop, handling **each connection concurrently and
  independently**, reading newline-delimited `Msg`s.
- During a `user_turn`, the connection handler registers a progress callback and forwards
  whichever arrives first — a progress event (from a bounded buffer, cap **256**) or the
  final response — to the client, as events arrive.
- Multiple clients **MAY** attach to the same session; each gets the live stream and, on
  attach, a replay snapshot (see [`agent-worker.md`](agent-worker.md) R-WORK.6).

---

## Reference symbols

`internal/protocol/protocol.go` (`Msg`, `ProgressEvent`, `StatusInfo`, constructors),
`internal/protocol/client.go` (`EnsureDaemon`, typed request methods),
`internal/runtime/daemon.go` (`handleConn`, `dispatch`).
