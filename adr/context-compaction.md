# Context Compaction: summarize the folded prefix instead of dropping it

**Status:** **Implemented as an additional, per-agent mode; "compact" is the
intended future default.** The existing trim behavior (the context builder
drops the oldest messages when the window fills) stays the default for now.
Both modes are live at once: each agent picks one.

Ported from seven's compaction mechanism (`seven/agent/compact.py`,
docs/compaction.md), adapted to nine's architecture: nine assembles every
request through `internal/context.Builder` inside a token budget rather than
against a fixed `SEVEN_CONTEXT_TOKENS`, and agents are stored definitions, so
the mode is a per-agent field rather than a global env var.

## Decision

When an agent in `compact` mode nears its context window, the older part of
its conversation is folded into a running summary that rides in the system
prompt; only the recent part is sent message by message. The folded messages
stay in the agent's persisted state (checkpoint), so a restart resumes with
the summary intact.

| Trigger | Condition | Then |
|---------|-----------|------|
| Threshold | Before building a request, the estimated request exceeds 75% of the builder's budget (`ninectx.CompactAt`) | Compact, then build and call |
| Overflow | The provider rejects the request as too long — a 400/413/422 whose text names tokens, context or length (`ninectx.IsOverflow`) | Compact, rebuild, and make the call once more; a second rejection fails the turn |

- The estimate is `ninectx.RequestChars` (system + history + tools) through
  `EstimateTokensFromChars`, the same chars-per-token estimate the builder
  already uses; the existing `Calibration` refines it after replies that
  report usage.
- The cut (`ninectx.CutIndex`) keeps the most recent messages fitting in 25%
  of the window, never starting at a tool-result message (its call would be
  folded away), preferring a user message when one fits.
- The folded prefix goes to the summarizer — the same model, no tools, at
  background priority — in chunks of at most 50% of the window, each chunk's
  summary becoming the previous summary of the next (`ninectx.Summarize`).
  Each message is clipped to 4,000 chars (start + end). The reply is capped
  at 8% of the window.
- The summary is assembled into the system prompt as a `## Earlier in this
  conversation` section at builder priority 2.7 (after the self-model and
  enrichment, before history), capped at `max(256, 8% of budget)` tokens.
  When the kept history does not start with a user message, a one-line lead
  ("(Earlier messages are summarized in the system prompt. Continue.)") is
  prepended to the request but not stored.
- The loop records `summary` and a `compactions` counter in its
  `ConversationState`, so the daemon's checkpoint persists them.

## Where the mode lives

- Per agent: `agents.context` column (migration 27→28), exposed as
  `context` on the wire `AgentDefinition`, the REST `AgentDefinition` /
  `AgentPatch` schemas, and the Builder UI's agent form ("daemon default" /
  "trim" / "compact"). Empty means inherit.
- Daemon default: `[context] mode` in nine.toml (`trim` today; `compact` is
  the intended future default), validated in `config`, threaded through
  `AssemblyConfig` → `LoopConfig.ContextMode` → `AgentBuilder.build`, which
  lets the agent's own setting win.
- The loop (`internal/agent/loop.go`) receives the resolved mode in
  `agent.Config.ContextMode`; `""`/`trim` never compacts.

## Failures

| Failure | Result |
|---------|--------|
| Summarizer fails at the threshold | Logged; the turn proceeds with the whole conversation |
| Summarizer fails after an overflow | The turn fails with that error |
| Nothing can be folded | Logged; at an overflow the provider's error fails the turn |

## Limits

- Summaries lose detail; the folded messages remain in the checkpoint for
  people and for re-runs, but the agent no longer sees them verbatim.
- The latest turn is always kept whole; many large tool results in one turn
  can still exceed a small window.
- Overflow is recognized by status code and wording, not by a structured
  error field.
