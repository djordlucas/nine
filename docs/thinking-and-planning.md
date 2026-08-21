# Thinking and planning

Two separate questions, often confused: **does the model reason before it
acts**, and **do you get to see and approve that reasoning**. Nine answers them
with different settings, because the useful combinations are not the obvious
ones — you can plan without watching, and watch without approving.

## Reasoning before acting

A turn can involve several calls to the model: one to decide what to do, then
more as tool results come back. Reasoning is worth the latency on the first of
those and rarely on the rest, since by then the approach is already chosen.

`plan_mode` in the `[planning]` section controls this:

| Mode | Behavior |
|---|---|
| `off` | No reasoning pass. Fastest, and the model commits to its first instinct. |
| `plan-only` | **Default.** Reason on the first call of a turn, then execute without further reasoning. |
| `always` | Reason on every call. Slowest, and worth it when each tool result genuinely changes the plan. |

`plan-only` is the default because the cost of reasoning is paid per call while
the benefit lands almost entirely on the first one.

Change it live in the TUI with `/plan-mode off | plan-only | always`. The
setting applies to that session, leaving the configured default alone.

## Watching it reason

Reasoning happens whether or not you see it. `thinking` in the `[llm]` section
decides whether it is **streamed to the TUI as a live trace**, and defaults to
on.

This requires a model that advertises the capability, and currently works with
Ollama. Turning it off asks the model to skip reasoning aloud entirely, which
is a latency choice rather than a display one.

Two things follow that are easy to get backwards. Watching the trace does not
mean the model is planning more — that is `plan_mode`. And turning the trace
off does not disable planning, though it does remove the only direct evidence
you had that planning happened.

## Approving a plan

Reasoning produces an intention before anything acts on it, which is the moment
where a plan can still be cheaply corrected. `plan_approval` decides whether
you are asked:

| Mode | Behavior |
|---|---|
| `off` | Never prompt. |
| `on` | Always prompt before a plan executes. |
| `on-risky` | **Default.** Prompt only when the plan intends to use a tool that requires approval. |

`on-risky` follows the same list of approval-requiring tools as
[human-in-the-loop](hitl.md) generally, so a tool you have marked dangerous
gets a checkpoint at the planning stage as well as at the call itself. The
difference matters: at the planning stage you can redirect the approach, while
at the call you can only permit or refuse the step.

Approval is interactive by nature, so it applies to TUI conversations. A
background session has nobody to ask.

## Related

- [Human-in-the-loop](hitl.md) — the approval gate itself, and which tools require it
- [Configuration](configuration.md) — the `[planning]` and `[llm]` sections
- [Model compatibility](model-compatibility.md) — which models advertise thinking

> The design and its milestones — [../adr/thinking-and-planning.md](../adr/thinking-and-planning.md).
