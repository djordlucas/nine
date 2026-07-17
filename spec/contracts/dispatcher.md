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
   output, err = fn(ctx, args)
   if err: return failure observation
   for h in hooks[toolName]: h(toolName, args, output)
   return capOutput(output)
```

A call to an unregistered tool returns an error result the agent observes — it **MUST
NOT** panic the loop.

---

## R-DISP.2 — Output cap

Every successful result is capped at `maxOutputTokens = 2048` tokens (`maxChars =
2048 × 4 = 8192`) **before** being appended to the scratchpad. Truncation sets a
`Truncated` flag and appends a note. This cap is applied by the dispatcher at result
time, independent of context-budget trimming (R-CTX.*).

---

## R-DISP.3 — Tool taxonomy

```text
TOOL CALL
   ├── PLUGIN TOOLS  (RegisterPlugin)        handler → manager.Call(plugin, …) → JSON-RPC
   │      shell, read_file, write_file, http_*, web_*, skill_*, time, browser_*
   └── CORE-INTERCEPTED TOOLS (Register* at build) handled in-process, no subprocess:
          gap_report
          memory_embed, memory_query, file_search_semantic
          run_agent, run_agents
          workflow_create/update/get/list/retry_step
          goal_create/get/list/update_status/append_subtree
          (planned: ask_human — see hitl.md)
```

Core-intercepted tools appear in the tool list but are routed to daemon-mediated handlers
— this is how the agent reaches goal/workflow state without a raw table handle
(invariant I4).

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

## Reference symbols

`internal/agent/dispatcher.go` (`Dispatcher`, `Dispatch`, `capOutput`, `maxOutputTokens`,
`AddHook`, `RegisterPlugin`, `RestrictTo`), `internal/agent/register_*.go`
(intercepted-tool wiring), `internal/runtime/builder.go`
(`AgentBuilder.build(agentID, role, depthGuard)`).
