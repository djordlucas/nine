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

The discriminator is a **named string type** (`MsgType`) with one declared constant per
message, not a bare string. This is a source-level requirement with **no wire effect** —
a `MsgType` encodes and decodes as the same JSON string — and a conforming implementation
in a language without named string types satisfies it with whatever its equivalent is
(an enum, a symbol) or with plain constants.

The intent is that no message name is written as a literal outside the constant
declarations. Note what this does **not** buy in Go: an untyped constant converts
implicitly, so `msg.Type == "typo"` still compiles, and a declared constant that is never
added to the dispatch switch is not a compile error either. Those gaps are closed by
test, not by the type — see R-PROTO.9.

**The flat envelope is the wire, not the programming model.** R-PROTO.12 requires the
daemon to decode it into a per-message struct before dispatching, so handlers program
against a type carrying only their own fields. That is a source-level shape and changes
nothing here: a decoded request marshals back to exactly this flat object.

```schema
Msg {
  type              string   // discriminator; required
  agent_id          string   // session this message concerns
  text              string   // free text / JSON payload depending on type
  id                string
  name              string
  instance_name     string   // ok/conversation_id + set_instance_name: the daemon's display name
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
| `list_workflows` | — | request workflow list |
| `list_tools` | — | request all tools grouped by plugin |
| `workflow_stop` | `text` = workflow ID | live-cancel a workflow |
| `workflow_fail` | `text` = ID or `--all` | post-mortem fail |
| `session_stop` | `agent_id`, or `text` = `--all` | terminate a session (stop its worker + delete its persisted state) |
| `plugin_call` | `tool_name`, `tool_input` | invoke a tool directly, **bypassing the LLM** |
| `plugins_list` | — | request the plugin roster (built-in + user, with skip reasons) |
| `plugins_reload` | — | re-scan `[plugins].user_dir` and reload user plugins live (built-ins untouched) |
| `tools_list` | — | request the sandboxed-tool roster (with resolved capabilities and skip reasons) |
| `tools_reload` | — | re-scan `[tools].user_dir` and reload sandboxed tools live |

`plugin_call` (R-PROTO.5) is the only way to reach a tool without an agent loop; the CLI
uses it for `nine workflow fail` when the daemon is up and for diagnostics.

---

## R-PROTO.3 — Daemon → client messages

**Lifecycle / results:**

| `type` | Fields | Meaning |
|--------|--------|---------|
| `conversation_id` | `id`, `role`, `instance_name` | conversation created; `role` is the session's resolved role, `instance_name` the daemon's display name |
| `ok` | `agent_id`, `role`, `instance_name`, `replay_events`, `pending_response` | attach succeeded (+ replay snapshot); `role` is the session's resolved role, `instance_name` the daemon's display name |
| `response` | `agent_id`, `text` | the assistant's final answer |
| `done` | `agent_id` | turn complete |
| `error` | `text` | failure |
| `status` / `list_*` | `text` = JSON payload | the requested read (StatusInfo / array) |
| `plugins_list` / `plugins_reload` | `text` = JSON `[]PluginStatus` | the plugin roster (each: `name`, `source`, `loaded`, `tools`, `error`) |
| `tools_list` / `tools_reload` | `text` = JSON `[]SandboxedToolStatus` | the sandboxed-tool roster (see R-PROTO.4a) |
| `context` | `text` = JSON `ninectx.Report` | per-section token breakdown + assembled prompt/messages |
| `workflow_stop` / `workflow_fail` | `text` = `"stopped"` / `"failed"` | operator-command result |
| `session_stop` | `text` = human-readable outcome (`stopped <id>`, `stopped N session(s)`) | terminate result (or `error` when the id is unknown) |
| `set_plan_mode` | `text` = `"plan mode: <mode>"` | mode change acknowledged (or `error` on an unknown mode) |

**Streaming progress** (emitted during a turn, before `response`+`done`):

| `type` | Fields | Meaning |
|--------|--------|---------|
| `thinking` | `llm_call_n`, `think` | a new LLM round-trip started within this turn; `think` reports whether it will stream `thinking_chunk` |
| `stage` | `text` | the turn entered a named waiting phase; empty `text` clears it |
| `plan_start` | — | the no-tool request-analysis (planning) pass began (thinking-degraded models) |
| `plan_end` | — | the request-analysis pass finished |
| `notice` | `text` | a session-level notice (e.g. native thinking unavailable, planning pass used; context usage crossed the trim threshold) |
| `set_instance_name` | `instance_name` | the daemon's display name resolved or changed (e.g. async naming completed); broadcast to active sessions, optional — clients that don't recognise it **MUST** ignore it |
| `context_update` | `context_used`, `context_budget` | context assembled |
| `tool_start` | `tool_name`, `tool_display_name`, `tool_input`, `ts` | tool call started |
| `tool_end` | `+ tool_output` | tool call finished |
| `response_chunk` | `text` | a streamed text token |
| `thinking_chunk` | `text` | a streamed reasoning ("thinking") token; ephemeral, never journaled |
| `sub_agent_start` | `sub_agent_id`, `text` (task), `role`, `ts` | a sub-agent spawned; `role` is its resolved leaf role, for display |
| `sub_agent_end` | `+ status` | a sub-agent finished (`done`/`failed`/`timed_out`); carries `role` too |
| `human_input_required` | `request_id`, `question`, `options`, `timeout_seconds`, `origin` | the turn is blocked on a human ([`hitl.md`](hitl.md) R-HITL.6); `agent_id` is the **owning** session even when a sub-agent is the asker, and `origin` attributes it to that sub-agent (absent when the session itself asks) |

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

`plugins_list` and `plugins_reload` return `[]PluginStatus` JSON-encoded in `text`:

```schema
PluginStatus { name string, source string, loaded bool, tools []string, error string,
               disabled bool }
// source: builtin | user. A skipped user plugin has loaded=false and error set.
// disabled=true marks a plugin switched off in [plugins].disabled (plugin.md
// R-PLUG.14) rather than one that failed — a decision, not a fault. Additive and
// optional; a client that does not know it sees an unloaded plugin with a reason.
```

`plugins_reload` re-scans `[plugins].user_dir` (stopping and restarting only user
plugins) before returning the resulting roster; both are operator reads/actions,
never agent tools.

### R-PROTO.4a — Sandboxed tool roster

`tools_list` and `tools_reload` return `[]SandboxedToolStatus` JSON-encoded in `text`
(`spec/contracts/toolvm.md`):

```schema
SandboxedToolStatus { name string, kind string, loaded bool, generated bool,
                      capabilities string, description string, manifest_path string,
                      error string, deps []string }
// kind: js | wasm. A skipped tool has loaded=false and error set.
// capabilities is the RESOLVED grant — what the tool runs with, never what its
// manifest asked for.
// generated=true marks a tool Nine authored via tool_write (toolvm.md R-TVM.14):
// its code lives in the store, so it has no manifest_path.
// deps are the external npm packages a generated tool's bundle carries, as
// "name@version" (toolvm.md R-TVM.15); empty for a tool with none. All additive/optional.
```

The skipped entries are the point: a capability mismatch is deliberately a load failure
rather than a degraded tool (R-TVM.6), and that promise is only kept if an operator can
read the reason. Both messages are operator reads/actions, never agent tools; with
`[tools] enabled` unset, `tools_list` returns an empty array and `tools_reload` returns an
error naming the disabled subsystem.

---

## R-PROTO.5 — `plugin_call` bypasses the LLM

`plugin_call` **MUST** invoke the named tool directly through the dispatcher/manager and
return its output, with no agent loop, no context assembly, and no LLM call. It is the
mechanism behind daemon-down operator commands and direct tooling.

Its reach **MUST** match what `list_tools` advertises: the core-intercepted store tools
(`memory_*`, `file_*`, `skill_*`, `doc_*` — see [`plugin.md`](plugin.md) R-PLUG.5) are
listed under the `core` plugin but served in-process, so the daemon resolves the name
against the core dispatcher first and only then against the plugin roster. A tool the
client can *see* but not *call* is a contract violation — it is what broke the TUI's
`/skills` and `/memory`. The delegation tools (`run_agent`, `workflow_*`, `goal_*`) are the
one exception: they need a live session to spawn into, so they are registered per loop and
are not reachable this way.

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

---

## R-PROTO.9 — Every client message type is dispatched

The daemon **MUST** route every message type a client is documented to send, and an
implementation **MUST** carry a check that this holds — the set of client-sendable types
and the set the dispatcher handles cannot be allowed to drift apart silently.

The reference keeps the first set as an exported value (`protocol.ClientMsgTypes`) and
sends each member to a running daemon, asserting the reply is not the dispatcher's
`unknown message type` error. The assertion is deliberately weak on semantics: a
validation error or "no such conversation" **passes**, because it proves the message
reached a handler. Only the default branch fails.

This is what makes the typed discriminator (R-PROTO.1) load-bearing rather than a
convention. Without it, a message type can be declared, documented, and sent by a client
while the daemon has no case for it, and nothing reports that until a user hits it.

---

## R-PROTO.10 — A reply echoes its request's type

Every request that expects an answer is answered with a message of **the same type**,
carrying the payload in `text` (JSON-encoded where structured). `status` answers
`status`, `list_goals` answers `list_goals`, and so on. The exceptions are the two
requests that create or join a session — `new_conversation` answers `conversation_id`
and `attach` answers `ok` — and `user_turn`, which streams progress and ends with
`response` then `done`. A failure answers `error` in every case.

A client **MUST** verify the type before reading the payload, and **MUST NOT** treat any
non-`error` reply as success.

The reason is that the failure is otherwise **silent**. Fields are `omitempty` and the
envelope is flat, so a reply of the wrong type decodes cleanly with an empty `text` — a
client that screened only for `error` would hand its caller a successful-looking zero
value. In the reference this had already happened: `Context` returned `("", nil)` for any
non-error reply, and `Status` failed with a JSON decode error naming neither the request
nor the mismatch. Both would surface to a user as blank output rather than as a fault,
and both are the kind of thing a version skew produces.

The reference centralizes the check in one `expectReply(reply, want)` used by every
reply-reading method, because the defect it replaced was precisely non-uniformity — two
methods checked, thirteen did not.

---

## R-PROTO.11 — Required fields are checked on ingress

The envelope is flat and every field is `omitempty`, so which fields a given type
actually needs cannot be expressed in the schema. An implementation **MUST** carry that
as data — a table from message type to required fields — and check it **before routing**,
rejecting a message that cannot be served with an error naming **the type and the missing
field**.

Scope is **presence, not meaning**. Whether an agent id names a live session, or a mode
string is one of the legal modes, belongs to the handler that knows; this only rejects a
message no handler could serve.

The requirements **MUST** be derived from what handlers read, not from prose describing
the protocol — the prose is unverified by construction. Deriving them in the reference
found a real defect: `attach` tolerated an empty `agent_id`, and the resolver falls back
to a prefix match, where **every id has `""` as a prefix**. An empty agent id therefore
attached the client to whichever session the runtime's randomized map iteration yielded
first. Requiring the field turns an arbitrary outcome into a clear error.

A type with no requirements, and a type the dispatcher does not route, both pass — the
latter is R-PROTO.9's to report, and one fault should not be reported in two voices.

---

## R-PROTO.12 — Requests decode to per-message types before dispatch

A daemon **MUST NOT** dispatch on the envelope. It **MUST** first decode an inbound client
message into a **per-message request type** carrying only the fields that message has, and
route on that.

The envelope is a union of every field any message might need — 29 in the reference — so
dispatching on it means each handler reaches into a shared bag and knows *by convention*
which fields its own message populates. That convention is unwritten, uncheckable, and the
reason the reference's own contributor guide carries a five-place lockstep checklist for
adding a message.

Decoding **MUST** also validate (R-PROTO.11) and **MUST** reject an unroutable type, so a
handler receives a request that is not merely typed but populated, and the router needs no
"unknown" branch.

This is a **source-level** requirement with **no wire effect**, exactly as R-PROTO.1's
named discriminator is. A decoded request marshals back to the same flat object it came
from, and an implementation in a language without sum types satisfies it with whatever it
has — a tagged record, a class per message, a match on a variant. What is forbidden is
handing every handler the union.

Requests that carry no fields of their own (the status and list/reload verbs) **MAY** share
a single type distinguished by the message name; there is only one shape between them, and
a type apiece would be names differing in nothing.
