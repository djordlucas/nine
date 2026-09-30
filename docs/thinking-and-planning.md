# Thinking and planning

Three independent settings, often confused with each other:

| Setting | Section | Controls |
|---------|---------|----------|
| `plan_mode` | `[planning]` | Whether the model reasons before acting |
| `thinking` | `[llm]` | Whether that reasoning is streamed to the TUI |
| `plan_approval` | `[planning]` | Whether you are asked to approve a plan before it runs |

They combine freely: you can plan without watching, and watch without
approving.

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
setting applies to that session, leaving the configured default alone. For a
single turn, `/think <message>` forces reasoning on without changing the
session's mode — the way to think hard about one question in a session running
`off`.

## Watching it reason

Reasoning happens whether or not you see it. `thinking` in the `[llm]` section
decides whether it is **streamed to the TUI as a live trace**, and defaults to
on.

This requires a model that advertises the capability, and currently works with
Ollama. Turning it off asks the model to skip reasoning aloud entirely, which
is a latency choice rather than a display one.

Nine reads the capability from Ollama itself — `POST /api/show` for the
configured model, whose `capabilities` list either holds `thinking` or does not.
`qwen3.5:4b`, `qwen3.5:9b`, `gemma4:e4b` and `gemma4:e2b` all advertise it. The
probe is lazy, so Nine can start before Ollama does; while it cannot get an
answer — Ollama not up yet, the model not pulled yet — Nine assumes no native
thinking (sending `think:true` to a model that lacks it is an error) and logs
`could not read model capabilities from Ollama` with the reason. That assumption
is provisional: it is retried on later calls and replaced the moment Ollama
answers. Only a real answer is cached, and then for the life of the process,
because the model cannot change under a running daemon.

So *"This model does not support native thinking — running a planning pass
instead"* is a statement about the model, not about a hiccup. If you see it for a
model you know reasons, check the warning in the daemon log and that
`[llm].model` names what you pulled.

Watching the trace does not make the model plan more; that is `plan_mode`.
Turning the trace off does not disable planning, but it removes the only direct
evidence that planning happened.

## Approving a plan

Reasoning produces an intention before anything acts on it, which is the moment
where a plan can still be cheaply corrected. `plan_approval` decides whether
you are asked:

| Mode | Behavior |
|---|---|
| `off` | Never prompt. |
| `on` | Always prompt before a plan executes. |
| `on-risky` | **Default.** Prompt only when the plan intends to use a tool that requires approval. |

`on-risky` reads the same `require_approval` list as
[human-in-the-loop](hitl.md), so a tool you have marked dangerous gets a
checkpoint at the planning stage as well as at the call itself. The difference
matters: at the planning stage you can redirect the approach, while at the call
you can only permit or refuse the step.

**The risky check is a text match on the plan.** It looks for an approval-listed
tool *named in the plan the model wrote*, not for what the plan will actually do.
A plan that intends to run a command without saying `shell` does not prompt, and
the call-time gate is what catches it.

The prompt offers `approve` and `clarify`. Approving proceeds; anything else is
folded back in as clarification and the reasoning pass runs again with it, which
is how a plan gets redirected rather than merely stopped.

Approval is interactive by nature, so it applies to TUI conversations. A
background session has nobody to ask.

## Limits

| Limit | Detail |
|-------|--------|
| Thinking needs model support | The live trace requires a model that advertises the capability, and currently works with Ollama. See [model compatibility](model-compatibility.md). |
| Approval is interactive only | `plan_approval` applies to TUI conversations. A background session has nobody to ask, so a plan there runs unapproved whatever the setting. |
| Approval redirects, it does not edit | Answering anything but `approve` re-runs the reasoning pass with your answer as clarification. There is no way to edit the plan in place and continue from it. |
| The risky check reads the plan's words | `on-risky` matches an approval-listed tool name in the plan text. A plan that will use a gated tool without naming it is not caught here — only at the call. |
| Reasoning cost is per call | `always` pays the reasoning cost on every call in a turn. The benefit lands almost entirely on the first, which is why `plan-only` is the default. |

## Related

- [Human-in-the-loop](hitl.md) — the approval gate itself, and which tools require it
- [Configuration](configuration.md) — the `[planning]` and `[llm]` sections
- [Model compatibility](model-compatibility.md) — which models advertise thinking

> The design and its milestones — [../adr/thinking-and-planning.md](../adr/thinking-and-planning.md).
