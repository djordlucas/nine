# Contract — LLM Provider & Priority Queue

**Status:** Built · **Depends on:** config · **Used by:** agent loop (only, via the queue)

The LLM is reachable **only** through a prioritized queue (invariant I2). No agent, stage,
or supervisor calls a provider directly. The queue is the single choke point for
concurrency and priority across all sessions.

---

## R-LLM.1 — Provider interface

A provider is a thin **single-method** interface:

```interface
Provider {
  Complete(ctx, Request) (Response, error)
}

Request  { System string; Messages []Message; Tools []ToolDef; MaxTokens int; Think *bool; OnChunk func(string); OnThinkingChunk func(string); OnQueued func(); OnDequeued func() }
Response { Text string; ToolCalls []ToolCall; StopReason string }  // "end_turn" | "tool_use" | "max_tokens"
Message  { Role ("user"|"assistant"); Text string; ToolCalls []ToolCall; ToolResults []ToolResult }
ToolCall { ID string; Name string; Input json }
```

Adding a backend is therefore a **one-method** job. Streaming is delivered through the
request's optional `OnChunk` callback — there is no separate streaming method. The only
adapter shipped today is **Ollama** (R-LLM.7), and it is required.

The single implementation is **not** a reason to collapse the interface. Nine is
local-first as a commitment (**G8**) and expects to be **multi-backend within that
commitment** — llama.cpp and vLLM are planned, with model routing across them a stated
direction. Note that a self-hosted server speaking a hosted provider's wire format is
still local-first (N5): vLLM serves the OpenAI chat API, so the currently-empty
`internal/llm/openai` placeholder is the likely home of that adapter rather than a
hosted-OpenAI integration. Today OpenAI is available for **embeddings only**.

A `[llm].provider` value other than `ollama` is reported as unknown and the Ollama
adapter is built anyway, so a stale config still boots. **This fallback is safe only
while one adapter exists**; once a second ships, silently serving Ollama to an operator
who asked for something else is a misconfiguration worth failing on rather than logging.

A conforming implementation **MUST** keep the provider behind the queue.

---

## R-LLM.2 — Tool calls and final answers

`Complete` returns a `Response` that either carries `ToolCalls` (the model wants to act)
or is a final answer (`Text`, no tool calls). The agent loop distinguishes the two (see
[`agent-loop.md`](agent-loop.md)). When `Request.OnChunk` is set, the provider streams
text through it while still returning the assembled `Response`.

---

## R-LLM.3 — The priority queue

```text
Submit(cancel, priority, req):
    if inflight < maxConcurrent:  inflight++ ; run item concurrently → provider.Complete
    else:                          enqueue item by priority ; req.OnQueued()   // waits for a slot
when an item finishes:
    inflight--
    if any pending: take the highest-priority waiter ; inflight++ ; req.OnDequeued() ; run it concurrently
```

The queue is **priority-ordered** in front of the provider (the reference uses a min-heap),
bounding in-flight calls to `max_concurrent`. When a slot frees, the lowest priority
*value* (highest urgency) waiting item is served next.

`OnQueued`/`OnDequeued` report a wait that is otherwise indistinguishable from a slow
model: an interactive turn can sit behind reflection, standing agents, or sub-agents
before it reaches the provider. They are optional, fire only for a request that actually
parks, and run on the queue's goroutines — so they may outlive the `Submit` that
registered them, and an implementation **MUST NOT** fire `OnDequeued` once the caller's
context is done (the callback would report against whatever turn is running by then).

---

## R-LLM.4 — Priorities

```text
1  PrioritySupervisor    — supervisor diagnostic calls
2  PriorityConversation  — active user conversations (someone is waiting)
3  PriorityBackground    — tasks, goals, reflection, pursue sessions
```

Lower value served first. So a waiting **user turn always preempts a queued background
reflection** when a slot opens. A conforming implementation **MUST** use exactly these
three tiers and this ordering.

`max_concurrent` is configurable (`[llm].max_concurrent`):

- `1` (default, correct for a single local model) serializes **everything**, strictly by
  priority.
- higher values allow that many parallel provider calls, for an Ollama host with the
  headroom to serve them.

---

## R-LLM.5 — Cancellation & timeout

`Submit` honors a **cancellation handle**. Cancelling it removes a still-queued item and
aborts an in-flight call. `[llm].timeout_seconds` bounds individual provider HTTP calls:
0 uses the adapter default (300s, sized for a large local model on CPU) and a negative
value removes the bound, leaving cancellation as the only stop.
Sub-agent group timeouts are enforced one layer up (see
[`orchestration.md`](orchestration.md)), not in the queue.

---

## R-LLM.6 — Token counting

Token budgeting uses a 4-chars-≈-1-token approximation in the context builder (see
[`context-builder.md`](context-builder.md)); the provider interface carries no
token-counting method.

---

## R-LLM.7 — Ollama adapter (required)

Local-model support is a first-class goal (run usefully on small local models), so a
conforming implementation **MUST** provide the **Ollama** adapter over Ollama's streaming
`/api/chat` endpoint (`[llm].provider = "ollama"`). It **MUST**:

- send `stream: true` and accumulate the newline-delimited JSON chunks into the response
  `Text`, mirroring each content delta to `Request.OnChunk`;
- forward tool definitions and surface `tool_calls` from any chunk as `Response.ToolCalls`
  (with a synthesized id when the model omits one);
- map Ollama's `done_reason` to `StopReason` (`tool_calls`→`tool_use`, `length`→
  `max_tokens`, else `end_turn`; any tool call forces `tool_use`);
- apply `[llm].num_ctx` as the request's `options.num_ctx` when > 0;
- when `[llm].thinking` is enabled, send `think: true` (instead of prepending
  `/no_think`) and mirror each streamed `message.thinking` delta to
  `Request.OnThinkingChunk` — reasoning tokens are trace-only and **MUST NOT** be
  folded into `Response.Text`;
- surface an HTTP-error body and an in-stream `error` field as a Go error.

The adapter **MUST** be covered by a **hermetic** unit test (a mock `/api/chat` server),
so it runs in CI without a live model. Tests that need a real model **MUST** be gated on
`NINE_LIVE_MODEL` (an Ollama tag) and skip when it is unset.

---

## Reference symbols

`internal/llm/provider.go` (`Provider`, `Request`, `Response`, `Message`, `ToolCall`,
`ToolResult`, priority constants), `internal/llm/queue.go` (`Queue`, `Submit`),
`internal/llm/ollama/` (adapter + `ollama_test.go`, a hermetic `httptest` unit test).
(`internal/llm/openai/` is an empty placeholder.)
