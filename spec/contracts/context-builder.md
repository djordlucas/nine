# Contract — context builder & token budget

**Status:** Built · **Depends on:** embedder (relevance) · **Used by:** agent loop (every inner iteration)

The builder packs one LLM `Request` into a fixed token budget on every inner-loop
iteration. This is what lets Nine run usefully on small local models (goal G5).

---

## R-CTX.1 — token model

Token counting is a deliberate approximation: **4 characters ≈ 1 token**, with no
tokenizer dependency. The budget is `context_budget` (falling back to `num_ctx` when
unset). A conforming implementation **MUST NOT** require a real tokenizer for budgeting;
the approximation is the contract.

The approximation is **observable, not blind**: the estimate the builder returns from
`BuildWithUsage` is journaled as `llm_request.tokens_used`, and the provider's measured
count arrives as `llm_response.input_tokens` under the same span
([`event-journal.md`](event-journal.md), [`llm-provider.md`](llm-provider.md) R-LLM.8).
The divisor stays fixed and no measurement feeds back into assembly — reconciliation
informs a future change to the constant, it does not adjust it at runtime, so assembly
stays deterministic for a given input (R-CTX.4).

---

## R-CTX.2 — priority allocation

The budget is consumed by priority. Higher priorities are subtracted first; lower ones
get whatever remains. When a component doesn't fit, it is trimmed or dropped per its
strategy — **never** a higher-priority one.

| Priority | Component | Strategy when tight |
|----------|-----------|---------------------|
| **P1** | System core (current time + session ID + identity/instructions) | **never trimmed** |
| **P2** | Tool definitions | relevance-filtered to top-N + always-include (R-CTX.3) |
| **P2.5** | Self-model (`SystemSelf`) | capped ~**600 tokens**; dropped if it won't fit |
| **P2.6** | Enrichment (`SystemEnrichment`, a related prior session) | capped ~**300 tokens**; dropped first when tight (see [`subscriptions.md`](subscriptions.md)) |
| **P3** | Message history | keep newest, trim oldest first (`trimFront`) |
| **P4** | Scratchpad | keep newest entries, trim oldest first |
| **P4.5** | Analysis plan (`SystemPlan`) | capped ~**500 tokens**; dropped when tight, yields to scratchpad (advisory — a weak model's plan never crowds the turn) |
| **P5** | System extras (memory snapshot, plugin list) | included only if ≥ **200 tokens** remain (`extrasBudget`), else dropped |

P1 is inviolable. P5 is the first to go.

---

## R-CTX.3 — tool relevance filtering

```text
queryVec = embed(user query)             // provided by the loop, once per turn
for each tool:  score = cosineSim(queryVec, toolVector)   // 0 if no stored vector
sort:  always-include tools first, then by descending score
take:  ALL always-include tools
     + up to ToolTopN (default 20) others that still fit the remaining P2 budget
```

- **Always-include tools bypass filtering entirely**: the memory/file/vector tools and
  the core-intercepted tools (`gap_report`, `run_agent(s)`, `workflow_*`, `goal_*`).
  These are never dropped, so the agent never "loses" its fundamental capabilities.
- `ToolTopN` defaults to **20** (configurable; `0` → 20).
- With embedder provider `none`, ranking is bypassed and tools are included unranked up
  to budget.

Per-tool `toolVector`s are populated by the `AgentBuilder`: it embeds each tool's
`name: description` once and caches it by tool name (descriptions are static after boot),
setting the vector on the `ToolWithVector` it hands the loop. These vectors are held **in
memory** on the builder, **not** in the `vectors` table (there is no `tools:` namespace).
With `provider = "none"` the vectors are nil and ranking degrades as above.

A conforming implementation **MUST** keep always-include tools present every turn and
**MUST** cap the ranked remainder at `ToolTopN`.

---

## R-CTX.4 — assembly output & usage

`BuildWithUsage(...)` returns the assembled `llm.Request` **and** the approximate tokens
used, which the loop reports via `OnContextUpdate(used, budget)` → the `context_update`
progress event. The system block is assembled in order as `SystemCore (+ current time) +
SystemSelf + SystemEnrichment + SystemPlan + SystemExtras` (each included only if it fit its budget
check above); the tool list is `always-include + ranked plugin tools`; the messages are
`trimmed history + scratchpad expansion` (R-LOOP.2).

`BuildReport(...)` performs the **same assembly** through one shared code path and, in
addition to the request, returns a `Report`: a per-section token breakdown
(`system-core`, `tools`, `self-model`, `enrichment`, `history`, `scratchpad`, `extras`;
each with tokens, an included flag, and a detail note) plus the fully-assembled system
prompt and messages. It exists to power the `context` inspection command
([wire-protocol.md](wire-protocol.md)) and **MUST NOT** call the LLM. The loop exposes
it via `InspectContext(ctx)`, which the agent worker serves on its run goroutine so it
never races an in-flight turn.

---

## R-CTX.5 — memory as compression (behavioral guidance)

Because context is budgeted, the system prompt **SHOULD** steer the agent to offload to
memory rather than carry bulk in-context: store large tool outputs and retrieve by key,
write learned facts to K/V immediately, and summarize older scratchpad pairs. This is the
intended interplay between the builder and the store; it is guidance, not a hard gate.

---

## Reference symbols

`internal/context/builder.go` (`Builder`, `BuildWithUsage`, `BuildReport`, `assemble`,
`toolTopN`, `extrasBudget`, priority handling), `internal/context/report.go` (`Report`,
`Section`), `internal/selfmodel/` (the P2.5 `SystemSelf` block — see
[`session-plans.md`](session-plans.md)).
