# Self-improvement and boundaries

Nine improves itself by writing **skills** — and deliberately nothing more. It does
not generate plugins, rewrite its own configuration, or rebuild its own source code.
This is a design decision: a daemon that changes its own form over time becomes hard
to reason about, debug, and trust. Nine's executable shape is fixed; only its
*knowledge* grows.

---

## What Nine can change: its own skills

Skills are markdown how-to notes stored in the `skills` table of the memory
database. They are **data, not code** — writing one cannot alter Nine's behaviour
in any way other than giving a future turn something to read.

A line is drawn between two kinds of skill:

- **Built-in skills** are seeded from the binary (the repo's `skills/*.md`, embedded
  at build time) on every boot, and are **immutable at runtime**. They are changed
  only through normal development — edit the file, rebuild. This prevents the curated
  defaults from drifting on a running instance, which could cause inconsistent
  behaviour across sessions.
- **Agent skills** are authored by Nine via `skill_write` / `skill_modify`, persist
  in the store, and are mutable. `skill_write`/`skill_modify` refuse to overwrite or
  modify a built-in skill.

**How it works:**
1. Agent calls `skill_write` (create/replace its own) or `skill_modify` (update its own)
2. The row is written to the `skills` table with `source = agent`
3. The handler embeds the skill's description into the `skills` vector namespace
4. The self-model surfaces it on a relevant future turn; the agent then `skill_read`s it

**Example:**
```bash
./nine "Create a skill with guidelines for writing SQL queries against our analytics schema"
```

See [skills.md](skills.md) for the full skill format and lifecycle.

---

## What Nine cannot change

| Capability | Status | Why |
|---|---|---|
| Generate / build / hot-swap **native plugins** | **Removed** | Runtime code generation makes the running system drift from its source |
| Modify `nine.toml` at runtime | **Removed** | Config is set by the operator; changes require a deliberate restart |
| Modify built-in skills at runtime | **Removed** | Curated defaults should not drift on a running instance; edit the repo + rebuild |
| Rebuild its own Go source (`nine_propose_rebuild`) | **Removed** | Nine never edits and recompiles itself |
| Read its own source tree (`self_read`) | **Removed** | The source tree is no longer shipped in the container |

**Sandboxed tools are the one adjacent thing that is *not* removed, and Nine can now write
them — but only their code, never their capabilities.** Two shapes exist. A **developer
tool** is installed by an operator from `[tools].user_dir` and granted capabilities in
`nine.toml` — operator action, the same category as `[plugins].user_dir`. A **generated
tool** is written by Nine itself through `tool_write` and lives as a row in the store
(`spec/contracts/toolvm.md` R-TVM.14); it is *store state*, exactly like a goal, a workflow,
or an agent skill — listable, deletable, and journalled — so it does **not** drift the binary
the way a rebuilt native plugin would, which is the distinction that makes it permissible
where plugin generation is not.

The invariant holds: **no agent-reachable path writes a capability grant.** `tool_write` writes JavaScript and a capability *declaration*; the operator writes
the **ceiling** (`[tools.agent.capabilities]`) that bounds what any generated tool may be
granted, and a tool that declares nothing gets nothing. *Nine cannot grant itself
capabilities* (R-PLUG.7) holds unchanged — the agent writes the code, the operator writes the
grants, and they are never the same actor. The generated tier is **on by default**, bounded
by a ceiling that defaults to the workspace
(`[tools.agent] enabled`). Nothing about the toolchain property below changes: the QuickJS
interpreter is built ahead of time from pinned tags and committed as an artifact, so even a
generated tool adds no compiler to the runtime image — the agent writes JavaScript for a
pre-supplied interpreter, never anything that is built.

Because of this, the runtime container carries **no Go toolchain, no git, and no
source tree** — only the compiled `nine` binary, which is also the default
plugins. Configuration changes are made by editing
`nine.toml` and restarting the daemon.

---

## Plugins are fixed

Nine ships one plugin, `shell`, alongside its sandboxed tools. It is immutable
image content: compiled into the `nine` binary and served as
`nine plugin serve shell`. Reading and writing files, fetching over HTTP and
reading the clock were plugins too, and are now shipped sandboxed tools — first-party
capabilities under the capability model rather than subprocesses holding the
daemon's uid. There is no mechanism for an agent to add, build, or replace a
plugin at runtime. To add a built-in capability, add a plugin to the source repo
and rebuild the image.

An `[[mcp.server]]` is the operator's escape hatch from that, not the agent's: it
adds a capability without a rebuild, but only by editing `nine.toml` and
restarting. Nothing an agent does at runtime can declare one.

A plugin remains an isolation boundary: each runs as a subprocess, so a crash takes
down only that plugin, not the daemon. Recovery (restarting a crashed plugin from its
existing binary) is the plugin manager's responsibility — no recompilation is involved.

---

## Limits

| Limit | Detail |
|-------|--------|
| Skills only | Nine writes skills and, where enabled, sandboxed tools. It does not generate native plugins, rewrite `nine.toml`, or rebuild its own source. Deliberate: a daemon that changes its own form is hard to reason about, debug and trust. |
| Built-in skills are immutable at runtime | `skill_write` and `skill_modify` refuse to touch a skill seeded from the binary. Changing one means editing `skills/*.md` and rebuilding. |
| Generated tools are bounded by a ceiling, not by being off | `[tools.agent] enabled` defaults to true. The bound is `[tools.agent.capabilities]`, which defaults to the workspace — the same directory the shipped file tools and `shell` already reach — and confers nothing a tool has not declared. Set `enabled = false` to keep the host without the tier. |
| Duration is gated separately from reach | A generated tool that runs as a job needs `[tools.agent] allow_long_running`, and one that runs as a **process** — until stopped — needs `allow_processes`, with its turns under a role from `process_roles` (default `process`, no tools). The capability ceiling cannot express this: it bounds what a tool may *reach*, and duration is not reach. A process write prompts under `require_approval = "on_capability"` even when it declares nothing. |
| Capabilities are never agent-writable | `tool_write` writes JavaScript and a capability *declaration*. The operator writes every grant. `capability_request` lets the agent **ask** — it records a pending request a human decides on, and cannot confer anything. A tool that declares nothing gets nothing. |
| Config changes need a restart, except a grant | Editing `nine.toml` takes effect on daemon restart, for agent or operator. The one thing that changes live is the generated tier's capability ceiling: an approved request is installed on the running host and the tool catalog re-projected against it. Nothing writes `nine.toml` — the grant is recorded in the store, and the file is reconciled into it at each boot ([sandboxed-tools.md](sandboxed-tools.md) §7.2). |
| Adding a built-in plugin needs a rebuild | An `[[mcp.server]]` is the operator's way to add a capability without rebuilding; it still requires editing `nine.toml` and restarting. Nothing an agent does at runtime can declare one. |
