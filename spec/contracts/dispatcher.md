# Contract — Tool Dispatcher

**Status:** Built · **Depends on:** plugin manager, memory store, embedder · **Used by:** agent loop

The dispatcher is a `name → handler` map with post-call hooks and a hard output cap. It
routes every tool call from the model to either a plugin subprocess or an in-process core
handler. Both kinds appear in the same LLM tool list; the agent cannot tell them apart.

---

## R-DISP.1 — Dispatch path

```text
Dispatch(toolName, args):
   fn = handlers[toolName]               // unknown tool → error result (observable)
   if gated[toolName]: approve(ctx, toolName, args)     // original args (R-DISP.7)
   callArgs = expandRefs(ctx, toolName, args)           // R-DISP.7
   output, err = fn(ctx, callArgs)
   if err: return failure observation
   for h in hooks[toolName]: h(toolName, args, output)  // original args (R-DISP.7)
   return capOrSpill(ctx, toolName, output)             // R-DISP.2
```

A call to an unregistered tool returns an error result the agent observes — it **MUST
NOT** panic the loop.

---

## R-DISP.2 — Output cap and spill

Every successful result is capped at `DefaultMaxOutputTokens = 2048` tokens (`maxChars
= 2048 × 4 = 8192`) **before** being appended to the scratchpad. The cap is applied by
the dispatcher at result time, independent of context-budget trimming (R-CTX.*), and is
overridable per dispatcher with `SetMaxOutputTokens` (non-positive values ignored).

An over-cap result **MUST** set `Truncated` and record `OutputChars` (the original
length, in **characters**). What replaces the output depends on whether a spill sink is
registered (`SetSpill`):

| Sink | Result |
|------|--------|
| registered, succeeds | full output written to the file store; `SpillPath` set; output replaced by a **banner + head + elision marker + tail** preview |
| registered, fails | `SpillPath` empty; output truncated to `maxChars` + `[output truncated]`; the error is logged, **never** returned |
| not registered | as above — the pre-spill behavior |

The preview **MUST**:

- **lead with the banner**, before the head slice, naming the total size, the spill
  path, and how to read the rest. The head can be thousands of characters; a model
  reading top-down would otherwise consume a wall of data before learning it was
  truncated. (Empirically this is what makes the preview actionable: moving the notice
  from the elision point to the front took a small local model from 1/3 to 2/3 on the
  `tool-output-spill` eval.)
- **name tools to call, imperatively** — not function signatures. Live models shown
  `file_fetch(path, offset, limit)` responded by writing code instead of calling a tool.
- **keep both ends** — head ≈ ⅔, tail ≈ ⅓ of the preview budget. Errors, totals, and
  closing structure live at the *end* of a long output; a head-only cut discards
  exactly that.
- **not split a UTF-8 rune** at either boundary.
- **be much smaller than the cap when the spill succeeded** — the preview budget is
  `spilledPreviewChars` (reference: 1536), never more than `maxChars`. The cap is sized
  for discarded data; once the output is retrievable, a large preview is pure context
  cost and measurably drowns the request. On the **failure** path the full `maxChars`
  applies, because there the data really is lost.
- **bound its fixed overhead** — banner plus elision marker within
  `maxPreviewOverhead` (reference: 1024 characters).

A spill failure **MUST NOT** fail the tool call: a store outage degrades output
quality, it does not break the turn.

See [`../../adr/tool-output-spill.md`](../../adr/tool-output-spill.md).

---

## R-DISP.3 — Tool taxonomy

```text
TOOL CALL
   ├── PLUGIN TOOLS  (RegisterPlugin)        handler → manager.Call(plugin, …) → JSON-RPC
   │      shell, read_file, write_file, http_*, web_*, skill_*, time, <mcp>__*
   ├── SANDBOXED TOOLS (RegisterSandboxed)   handler → toolvm.Host.Call → wasm, in-process
   │      operator-installed, from [tools].user_dir (see toolvm.md)
   └── CORE-INTERCEPTED TOOLS (Register* at build) handled in-process, no subprocess:
          gap_report
          memory_embed, memory_query, file_search_semantic
          tool_list, tool_search  (skill_list/skill_search: see skills.md)
          tool_write, tool_delete, js_eval  (generated tier; only with [tools.agent] — toolvm.md R-TVM.14)
          run_agent, run_agents
          workflow_create/update/get/list/retry_step
          goal_create/get/list/update_status/append_subtree
          (planned: ask_human — see hitl.md)
```

Core-intercepted tools appear in the tool list but are routed to daemon-mediated handlers
— this is how the agent reaches goal/workflow state without a raw table handle
(invariant I4).

Sandboxed tools are a **second backend behind this same dispatcher**
(`spec/contracts/toolvm.md`): they are registered, advertised, role-filtered, gated, and
capped exactly as plugin tools are, and are indistinguishable from them downstream of
registration. They run in-process like a core-intercepted tool but are neither built in
nor daemon-mediated — they are operator-installed code in a wasm sandbox with an
explicitly conferred capability set. The branch is empty unless `[tools] enabled` is set.

---

## R-DISP.4 — Wiring model

`Dispatcher.New()` returns an **empty** handler map. The daemon adds handlers at startup
via the `Register*` functions (`RegisterGapReport`, `RegisterRunAgent(s)`,
`RegisterWorkflowTools`, `RegisterGoalTools`, `RegisterMemoryTools`, `RegisterSkillTools`,
…), and plugin tools are added dynamically by `RegisterPlugin` when a plugin starts. Only
the tools a role is allowed to see are registered; an unregistered tool name simply has no
handler and dispatches as `unknown tool`. This lets the loop be constructed before the
daemon exists, then have daemon-closure handlers injected.

```text
Dispatcher.New()  → empty handler map
   └ RegisterGapReport(...)  └ RegisterRunAgent(s)(...)  └ RegisterWorkflowTools(...)
   └ RegisterGoalTools(...)  └ RegisterMemoryTools/RegisterSkillTools(...)  └ RegisterPlugin(mgr, plugin)  // per plugin
   └ RegisterSandboxed(host)  // all sandboxed tools at once; no-op when the host is nil
```

---

## R-DISP.5 — Post-call hooks

`AddHook(toolName, fn)` registers callbacks that fire after a **successful** call to that
tool. The reference use: after `skill_write`/`skill_modify`, the skill's description is
embedded into the `skills` vector namespace (see [`skills.md`](skills.md)). Hooks
**MUST** run only on success and **MUST NOT** alter the returned output.

---

## R-DISP.6 — Role-aware registration (invariant I6)

The loop builder takes `(agentID, role, depthGuard)`. Tool registration is governed by
the role ([`roles.md`](roles.md)): `role.Delegates` (with `depthGuard > 0`) gates
`run_agent`/`run_agents`/`workflow_*`/`goal_*`, `role.SpawnsGoals` gates the goal
pursue-session spawn function, and an allowlist role prunes the handler set to its
tools plus `gap_report` (R-ROLE.4/5). Ungated tools are simply not registered, so the
model never sees them and a hallucinated call dispatches as `unknown tool`.

The delegation surface by role and depth guard (orchestrator; executor leaf at guard 1
and guard 0):

```text
                       orchestrator     executor (guard 1)   executor (guard 0)
plugin tools                ✓               ✓                    ✓
core memory/file/vector     ✓               ✓                    ✓
gap_report                  ✓               ✓                    ✓
run_agent / run_agents      ✓               ✓                    ✗   (guard exhausted)
workflow_* / goal_*         ✓               ✓                    ✗   (guard exhausted)
goal pursue-session spawn   ✓ (SpawnsGoals) ✗                    ✗
```

`depthGuard` (seeded from `roles.max_delegation_depth`, default 2, decremented per
spawn) is the recursion backstop behind `role.Delegates` (R-ROLE.6), so runaway
delegation stays structurally impossible.

---

## R-DISP.7 — Reference arguments (`x-nine-ref`)

A tool **MAY** declare a top-level string property as carrying a **file-store path**
rather than a literal value, by setting `"x-nine-ref": true` on it in its input schema.
Before dispatch, the dispatcher resolves each such argument through the registered
`RefResolver` and substitutes the stored content, so the tool receives the bytes while
the model handles only a short path.

Ref parameters are indexed from the schemas the model is already shown: `RegisterPlugin`
reads each plugin `ToolDefinition.InputSchema`; `New()` reads `InterceptedDefs`.

Rules:

- **Declared, never inferred.** Only marked properties are resolved. An argument that
  merely *looks like* a path **MUST NOT** be expanded — `file_fetch(path)` takes a real
  path, and expanding it would replace the path with the file's contents.
- **Optional.** A marked property is ref-*capable*, not required; an absent or empty
  value is left untouched.
- **Errors surface.** An unresolvable path **MUST** fail the call, naming the path, so
  the model can correct it. It is never silently passed through as a literal.
- **Bounded.** One expansion is capped at `MaxRefBytes` (8 MiB); over it the call fails
  and the error points at `file_fetch` windowing.
- **Approval and hooks see the original arguments.** The R-HITL approval gate and
  R-DISP.5 hooks receive the model's unexpanded arguments — a human approving a call
  reads the handle the model chose, not the payload behind it — and a rejected call
  never resolves the ref. The journal likewise records the handle (R-JRN `tool_start`).

With no resolver registered, ref arguments pass through unchanged.

See [`../../adr/tool-output-spill.md`](../../adr/tool-output-spill.md) §5.

---

## Reference symbols

`internal/agent/dispatcher.go` (`Dispatcher`, `Dispatch`, `DefaultMaxOutputTokens`,
`AddHook`, `RegisterPlugin`, `RestrictTo`), `internal/agent/spill.go` (`capOrSpill`,
`SpillFn`, `SpillPathPrefix`, `spillPreview`), `internal/agent/refs.go` (`RefMarker`,
`RefResolver`, `expandRefs`, `MaxRefBytes`), `internal/agent/register_*.go`
(intercepted-tool wiring), `internal/runtime/spill.go` (`registerLargeOutput`,
`spillPath`, `RunSpillSweeper`), `internal/runtime/builder.go`
(`AgentBuilder.build(agentID, role, depthGuard)`).
