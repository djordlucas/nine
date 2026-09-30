# ADR — decision records and implementation notes

This directory holds the **record of how Nine got here**: design notes,
investigation reports, implementation plans, and reviews. It is reference
material for people working on Nine.

It is **not embedded in the `nine` binary**. `nine docs` and the model's own
`doc_search` see `docs/` only, and that is deliberate — the embedded docs
describe how Nine works *now*, and a plan for work already finished would
answer questions about the present with the reasoning of the past.

## What goes where

| | `docs/` | `adr/` |
|---|---|---|
| Answers | how Nine works today | why it works that way |
| Audience | users and operators | people changing Nine |
| Tense | present | historical |
| Code references | avoided — components and concepts instead | expected: file paths, symbols, commits |
| Embedded | yes | no |

A document belongs here if its value is the *record* — the options weighed, the
migration sequence, the problem that motivated a change. A document belongs in
`docs/` if someone would read it to find out what Nine does.

The distinction is not "finished vs unfinished". Most of what is here is
complete; that is precisely why it moved. A design note whose design is now
simply *how Nine works* has had its content absorbed into `docs/`, and what
remains here is the reasoning.

## Contents

Sorted by status, then by subject. "Superseded" records a decision that later
changed; the document stays as the record and is not rewritten.

### Implemented

| Document | What it records | Shipped |
|----------|-----------------|---------|
| `architecture-review.md` | Rev-2 review: findings F1–F13, severity, sequencing | 2026-08-21, all closed |
| `concept-consolidation.md` | C1–C7: stages, reflection and goal bookkeeping, consolidated | 2026-08-19 |
| `daemon-assembly-refactor.md` | Collapsing two wiring paths into one `runtime.Assemble` | `cea290d` |
| `plugin-http-transport-design.md` | HTTP over a Unix socket for plugins: constraints and phased rollout | yes |
| `rich-js-tools.md` | The JS environment a sandboxed-tool author can call | yes |
| `thinking-and-planning.md` | Thinking and planning modes, M1–M7 | yes |
| `tool-exposition.md` | Top-K ranking vs. on-demand lookup; the hybrid is live behavior | yes |
| `tool-output-spill.md` | Spilling oversized tool output to the store, passing it back by reference | yes |
| `tui-slash-suggestions.md` | Slash-command suggestions in the TUI | yes |
| `architecture-wiring.md` | Boot order, end-to-end data flows, component map. Names types and call sites. | yes |
| `roles-design.md` | Worker kinds as data; migration off depth-based gating | yes |
| `capability-grants.md` | Defaulting both tool tiers on, and moving the generated tier's ceiling into the store so an operator can grant a requested capability without a restart | 2026-09-27 |
| `file-namespaces.md` | The workspace as the agent's only file namespace: external changes, editing large files, deletion to a trash. Retires `file_store`. Three divergences corrected in place; document extraction unbuilt. | phases 1–8, 2026-09-21 |
| `standing-tools.md` | Running a resumable tool on its own cadence. A second run mode, not a second kind of tool. | yes — `R-TVM.20`, all five phases |

### Proposed

| Document | What it records |
|----------|-----------------|
| `accurate-token-counting.md` | Real tokenizer behind a feature flag, replacing the character estimate |
| `codebase-improvement.md` | Design note: prioritized improvements across the tree |
| `critic.md` | Assessment of the feature set, the limits and the positioning: findings `C1`–`C11`, what is differentiated, and where Nine should be aimed |
| `durable-and-long-running-tools.md` | Giving a sandboxed tool memory and letting work outlive a turn. Amends I-TVM.3. |
| `generated-tool-authoring-loop.md` | Shortening the write→fail→rewrite loop for agent-authored tools: parse at write time, dry-run, logs on failure, stdlib gaps, catalog hygiene. |
| `event-log.md` | Investigation behind the session journal. Written against PostgreSQL; the store is SQLite. |
| `personality-pattern.md` | Packaging complete Nine instances as specialized agents; self-model bootstrapping |
| `predefined-agents-design.md` | Standing agents as config-seeded goals: motivation, work breakdown, phasing |
| `reactive-events.md` | Event subscriptions: reacting to the journal |
| `tool-facilities.md` | Host-injected credentials, a wider HTTP method allowlist, streaming progress, and reactions as a third run mode delivered by `reactions_get`. Rejects tool→tool dispatch and MCP reach. |
| `tui-boxed-messages.md` | Boxed message rendering in the TUI. `internal/tui` only. |

### Superseded in part

| Document | What changed |
|----------|--------------|
| `single-container.md` | The single-container design. The browser plugin it assumes no longer exists — browser automation is an `[[mcp.server]]`. |
| `event-log.md` | Assumes a PostgreSQL store; persistence is a single SQLite file. |

### Not about Nine

| Document | What it records |
|----------|-----------------|
| `github-hardening.md` | GitHub repository configuration for public release: secret audit, branch protection, scanning, metadata. Kept here so it does not ship in the binary. |

## Limits

| Limit | Detail |
|-------|--------|
| Status is recorded by hand | Each document's `**Status:**` line and this index are updated manually; a stale status is possible. Trust the document over the index. |
| Proposed documents may never ship | Nothing here is a commitment. A proposal that was rejected keeps its `Proposed` status rather than being deleted. |
