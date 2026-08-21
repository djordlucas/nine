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

- **architecture-review.md** — the rev-2 review: findings F1–F13, severity,
  sequencing. All findings are closed.
- **concept-consolidation.md** — C1–C7: stages, reflection and goal
  bookkeeping, consolidated. Complete 2026-08-19.
- **event-log.md** — investigation report behind the session journal.
  Written against a PostgreSQL store; the store is SQLite now.
- **reactive-events.md** — event subscriptions: reacting to the journal.
- **tool-output-spill.md** — spilling oversized tool output to the store and
  passing it back by reference.
- **tool-exposition.md** — top-K ranking vs. on-demand lookup, and why the
  hybrid won.
- **rich-js-tools.md** — what a sandboxed-tool author can actually call: the JS
  environment, and what belongs beneath it.
- **roles-design.md** — why worker kinds became data, the migration off
  depth-based gating, and the acceptance gates for it.
- **thinking-and-planning.md** — thinking and planning modes, M1–M7.
- **architecture-wiring.md** — the boot order, end-to-end data flows, and the
  component relationship map. Names types and call sites, which is what tracing
  a flow needs and what the architecture doc deliberately avoids.
- **daemon-assembly-refactor.md** — collapsing two wiring paths into one.
- **single-container.md** — the single-container design. Partly superseded:
  the browser plugin it assumes no longer exists.
- **tui-slash-suggestions.md** — slash-command suggestions in the TUI.
