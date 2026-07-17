# Contract — Agent Loop (ReAct)

**Status:** Built · **Depends on:** context builder, llm queue, dispatcher, embedder · **Used by:** agent worker, sub-agents

`agent.Loop` implements Reason → Act → Observe. One `Run(ctx, text)` call = **one turn** =
possibly many LLM round-trips (the inner loop). The loop holds `history` and `scratchpad`
and is **not safe for concurrent use** — the per-session serial worker guarantees
serialization (invariant I1).

---

## R-LOOP.1 — Turn structure

```text
Run(ctx, userText):
  history += {user, userText}
  scratchpad = []
  queryVec  = embed(userText)          // embedded ONCE, reused every iteration
  selfModel = SelfModelFn(queryVec)    // assembled ONCE (may be empty)

  loop (inner):
    llmCallN++ ; onThinking(llmCallN)
    req  = builder.BuildWithUsage(SystemCore+time, SystemExtras, SystemSelf,
                                  Tools, queryVec, History, Scratchpad)
    onContextUpdate(used, budget)
    resp = queue.Submit(ctx, Priority, req)

    if resp.ToolCalls is empty:
        answer = resp.Text  (or emptyAnswerFallback)
        history += {assistant, answer}
        scratchpad = []
        return answer                  // TURN COMPLETE
    for each toolCall tc:
        onToolStart(tc.Name, displayName, tc.Input)
        result = dispatchWithRetry(tc)
        onToolEnd(...)
        scratchpad += {thought, tc, observation}
    // loop back: rebuild context WITH the new scratchpad
```

The query vector and self-model are computed **once per turn**, not per inner iteration.

---

## R-LOOP.2 — The scratchpad is the turn's working memory

`scratchpad` is a list of `ScratchpadEntry{Thought, ToolName, ToolArgs, Observation}`.
When the context is rebuilt, each entry **MUST** expand into an assistant message (the
thought + tool call) and a user message (the tool result), so the model sees its own
prior actions on the next iteration.

- It is **cleared on every final answer** and the user↔assistant exchange is folded into
  `history`.
- It is persisted in the checkpoint (so it survives disconnect/restart mid-turn).

---

## R-LOOP.3 — History

`history` is the accumulated `[]Message` of user/assistant turns. It is **never** cleared
between turns unless `ClearHistory()` is called explicitly (e.g. `/new`). History is
trimmed *for context assembly* by the builder (R-CTX.*) but the stored history is not
mutated by trimming.

---

## R-LOOP.4 — Tool-call retry

`dispatchWithRetry` calls the dispatcher up to `maxToolRetries + 1` times
(`maxToolRetries = 2`, so **3 attempts total**), returning on the first success. After
the final failure, the error becomes the observation text — the failure is recorded, not
thrown. A conforming implementation **MUST** retry exactly 3 times total by default.

---

## R-LOOP.5 — Empty-answer fallback

If the model ends a turn with no tool calls **and** no text, the loop **MUST NOT** return
blank. `emptyAnswerFallback` turns the accumulated tool errors from the turn into a
visible message. Failures surface to the user rather than being swallowed.

---

## R-LOOP.6 — Progress callbacks

The loop exposes settable callbacks the worker wires per turn and clears after:
`OnThinking(llmCallN, think)`, `OnContextUpdate(used, budget)`, `OnToolStart(name, display,
input)`, `OnToolEnd(name, input, output)`, `OnChunk(text)` (streamed token),
`OnThinkingChunk(text)` (streamed reasoning token, when the provider surfaces
thinking), `OnStage(label)` (a named waiting phase; empty clears). These map 1:1 to the
streaming protocol events (R-PROTO.3).

`OnThinking`'s `think` **MUST** report whether the call will actually stream reasoning —
the policy's answer AND the model's capability — so a client never promises the user a
trace that cannot arrive. `OnStage` is the one callback that may fire from outside the
turn's goroutine (a queue wait is reported by the LLM queue, R-LLM.3), so an
implementation **MUST** make it safe against the worker clearing it after `Run` returns.

---

## R-LOOP.7 — Checkpoint unit & stall signal

- `SaveState()` serializes `{history, scratchpad}` to JSON; `LoadState(data)` restores
  them. Checkpointing happens **outside** the loop, in the worker, after `Run` returns
  (invariant I5).
- `LastRunToolCount()` returns the number of tool calls in the most recent `Run`; the
  worker uses it for stall detection (a turn with `0` increments the stall counter — see
  [`agent-worker.md`](agent-worker.md)).

---

## R-LOOP.8 — Priority is a property of the loop's owner

Each loop submits to the queue at the priority of its owner (supervisor / conversation /
background). The loop does not choose its own priority arbitrarily; it is set when the
loop is built for a given session role.

---

## Reference symbols

`internal/agent/loop.go` (`Loop`, `Run`, `dispatchWithRetry`, `maxToolRetries`,
`emptyAnswerFallback`, `SaveState`/`LoadState`, `LastRunToolCount`).
