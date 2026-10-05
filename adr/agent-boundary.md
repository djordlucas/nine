# Design note — The agent boundary

**Status:** **Proposed** · **Related:** `adr/roles-design.md`, `adr/personality-pattern.md`,
`adr/thinking-and-planning.md`, `adr/reactive-events.md`, `adr/daemon-assembly-refactor.md` ·
**Amends:** nothing in default behavior — see §6

Nine is split into two parts behind one interface: a **runtime** — the agent loop, tool
dispatch, conversations and their persistence, the event journal, the LLM queue and the API —
and an **agent** — everything that makes a conversation Nine's: its persona, self-model,
memory surfacing, skills, planning, supervision, reflection and goals. The runtime calls the
agent only through the `Agent` interface defined here. Nine's current behavior becomes the
**internal agent**, one implementation of that interface. A **mode** decides whether the
internal agent drives the daemon, or whether an **external agent** does, through the API, by
supplying in each conversation what the internal agent would have supplied. Nothing is removed:
in `internal` mode, the default, Nine is exactly what it is today.

The motivating external agent is nine-will, which runs artificial persons that bring their own
identity, memory and code and use Nine as their LLM runtime.

---

## 1. Problem

The agent layer is woven through the runtime, so a client that brings its own identity gets
Nine's anyway, in every turn. Measured on v4.4.0 with Mistral Medium, a conversation created
through `POST /api/v1/conversations` and asked "Reply with exactly the word: pong" sends the
model **8,518 input tokens**:

| What rides in the turn | Where it is wired today | Effect for an external client |
|---|---|---|
| The role's system prompt, with delegation steering | `runtime/builder.go` (`systemCore`) | Nine's persona, not the caller's |
| The self-model block | `SelfModelFn` → `selfmodel.Assembler` | A second identity in every turn |
| ~50 role-scoped, ranked tool definitions | `ToolsForTurn` | ~7,200 tokens before the first word |
| Memory, related-session and job enrichment | `EnrichmentFn` | One namespace for the daemon: one client's sessions reach another's turns |
| The planning pass | `AnalysisPrompt`, `plan_mode` | An extra model call per turn; with a reply cap it can come back empty and trigger the empty-response retry |

And around the turn: stall detection (`agent_worker.go`, `StallConfig`), which flags any
session whose turns use no tool; notifications prepended to the next turn; skills seeded and
ranked; the self-reflection session; and the indexers feeding enrichment.

Some of this can be switched off daemon-wide, some cannot, and none of it is behind one seam:
the agent's concerns are reached from the builder, the worker, the daemon wiring and the
context builder separately. That makes it hard to offer a runtime without the agent, and hard
to reason about the agent as one component.

## 2. Decision

| Part | Holds | Changes |
|---|---|---|
| Runtime | `agent.Loop` (ReAct, tool dispatch, scratchpad, checkpoints), the tool host and plugins, the dispatcher, conversations, the event journal with trace and replay, the LLM queue, the context builder's mechanics, the HTTP API and streams | Calls the agent only through `Agent`; knows nothing of personas, skills or reflection |
| Agent (interface) | What a conversation is for, and what runs between turns | New: `internal/agent` gains the interface; the runtime depends on it |
| Internal agent | Today's behavior: roles, self-model, enrichment, skills, planning, stall supervision, notifications, self-reflection, goals and pursue sessions, standing agents, the supervisor | Moved behind `Agent` without behavior change |
| External agent | What an API client supplies per conversation: system prompt, tool allowlist | New, small: a conversation's policy built from the request |

**Mode.** `[agent] mode = "internal" | "external"` chooses the daemon's agent, and a daemon
runs exactly one. In `external` mode the internal agent is not constructed: no reflection
session, no indexers, no supervisor, no skills seeded, and every conversation takes its policy
from its creator. In `internal` mode, a request for an external conversation is refused.

A daemon never mixes the two. Both would share one store, one memory namespace and one
journal, so one agent's content could reach the other's turns through any hook that missed the
distinction; an external client is exactly the case that must not see Nine's memories, nor
leave its own in them. Keeping the mode exclusive makes that structural rather than a rule
every indexer and enrichment function has to remember.

## 3. The interface

Two levels, matching the two kinds of concern: what one conversation's turns contain, and what
an agent does on its own.

```go
// Agent is what drives the runtime: it decides what each conversation is
// for, and may run work of its own between turns.
type Agent interface {
	// Conversation returns the policy for a new conversation. spec carries
	// what its creator asked for (role, interactive, and for an external
	// conversation the system prompt and tool allowlist).
	Conversation(spec ConversationSpec) (TurnPolicy, error)

	// Start runs the agent's own background work (reflection, indexers,
	// standing agents, the supervisor) against the runtime until ctx ends.
	// The external agent's Start returns at once.
	Start(ctx context.Context, rt Runtime) error
}

// TurnPolicy is consulted by the loop at fixed points of every turn.
type TurnPolicy interface {
	SystemPrompt() string                                    // the conversation's persona
	Tools(ctx context.Context, query []float32) []llm.Tool   // the tools this turn may call
	Context(ctx context.Context, query []float32) []Block    // self-model, enrichment, … (none for external)
	BeforeTurn(ctx context.Context, history []llm.Message) (plan string, err error) // planning pass; "" for none
	AfterTurn(ctx context.Context, result TurnResult)        // stall detection, notifications, self-improvement
}
```

`Runtime` is the narrow surface the agent may use: the LLM queue, the dispatcher, the store,
the journal and session control. The internal agent already uses exactly these; making them an
interface is what keeps the runtime from depending back on the agent.

| Today's hook | Becomes |
|---|---|
| `role.SystemPrompt`, delegation steering | `TurnPolicy.SystemPrompt` |
| `ToolsForTurn`, role allowlists, catalog meta-tools | `TurnPolicy.Tools` |
| `SelfModelFn`, `EnrichmentFn` | `TurnPolicy.Context` |
| `AnalysisPrompt`, `PlanMode`, `PlanReviewFn` | `TurnPolicy.BeforeTurn` |
| `StallConfig`, notification prepending, the self-improvement loop | `TurnPolicy.AfterTurn` |
| The reflection session, related-session indexer, memory indexing hooks, supervisor, standing agents | `Agent.Start` |

## 4. The external agent

An external conversation is created with:

| Field on `CreateConversationRequest` | Meaning |
|---|---|
| `agent: "external"` | Optional, since an `external` daemon has no other kind; refused by an `internal` daemon |
| `system_prompt` | The conversation's persona; a neutral one-paragraph prompt when absent |
| `tools` | Exactly the tools the conversation may call; none when absent. Unknown names are a 400 naming them |

Its `TurnPolicy` returns that prompt and that tool set, contributes no context, plans nothing,
and does nothing after a turn beyond what the runtime always does: the checkpoint and the
journal. The caller supplies the rest: identity in the prompt, memory as it sees fit, its own
supervision.

## 5. What the runtime keeps in both modes

| Concern | Kept |
|---|---|
| Loop | ReAct turns, tool dispatch, scratchpad, the empty-response retry |
| Tools | The sandboxed-tool host, plugins, MCP servers, the dispatcher's output cap and spill |
| Conversations | Creation, turns, stop, delete, checkpoints, attach |
| Observation | Event journal, trace, replay, SSE and WebSocket streams |
| LLM | Queue and `max_concurrent`, `[llm] max_tokens`, providers |
| API | Every route; `GET /conversations/{id}/context` shows what an external conversation's turn holds |

## 6. Compatibility

`internal` is the default mode and its behavior does not change: the refactor moves code
behind the interface, and its acceptance test is that every assembled context and every
journal of the existing test and eval suites is identical before and after. The new request
fields are optional. Contracts to update: `spec/contracts/config.md` (`[agent] mode`), the
HTTP API contract and `internal/api/openapi.yaml`, `docs/architecture.md` (the split),
`docs/configuration.md`, `docs/api.md`, and a new `docs/external-agents.md` for clients.

## 7. Phases

| Phase | Delivers | Acceptance |
|---|---|---|
| 1 | `Agent`, `TurnPolicy`, `Runtime` interfaces; today's code moved behind them as the internal agent | Identical contexts and journals across the test and eval suites |
| 2 | The external agent and `[agent] mode`; in `external` mode the internal agent is not built | A daemon in `external` mode runs no background model calls |
| 3 | `agent`, `system_prompt`, `tools` on `CreateConversationRequest`, validated | The pong turn's input drops from 8,518 tokens to a few hundred; its context holds no self-model, enrichment or unlisted tool |
| 4 | Docs and contracts | `/sync-nine` |

## 8. Open questions

| Question | Decision or leaning |
|---|---|
| May a daemon host both kinds of conversation? | Decided: no. The mode is exclusive (§2) |
| Can a conversation change agent after creation? | No: which agent a conversation belongs to is part of what it is |
| Where does memory live for external agents? | Their own concern. Nine's memory tools stay callable when listed in `tools`; nothing is surfaced into their turns |
| Names | Decided: `[agent] mode`, `internal`, `external`; the component is the agent |

## 9. Possible follow-up

**Narrowing internal conversations.** In `internal` mode an API client could pass `tools` when
creating a conversation to restrict it to a subset of its role's tools: a script that only
summarizes documents could create a conversation limited to `read_file` and
`file_search_text`, which then cannot write files or run commands whatever the model decides. A
name outside the role's set would be refused, so narrowing can never grant more than the role.
It is independent of the boundary and not part of this design.

---

**nine-will's use.** will runs nine in `external` mode and creates each conversation with a
`system_prompt` written from the instance's being and the `tools` that mind process needs. The
instance's identity then reaches the model as a system prompt instead of being repeated in each
message, and none of Nine's own identity competes with it. will gives up memory surfacing as a
route between instances; the shared workspace remains.
