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
| Write its own tools (`tool_write` / `js_eval`) | **Removed** | The model-authored tool tier went with the toolvm (adr/modular_nine.md §9.10); custom capabilities are operator-installed MCP servers and user plugins |

**The adjacent thing that *was* permissible is now removed too: Nine no longer writes
tools.** The generated tier (`tool_write`/`js_eval`, store rows in the `tools` table) ran
in the wasm sandbox and was removed with it (adr/modular_nine.md §9.10); the store tables
stay dormant so an existing database still opens. Custom capabilities now arrive only as
operator action: an `[[mcp.server]]` or a user plugin under `[plugins].user_dir`.

The invariant is unchanged and now structural: **no agent-reachable path writes a
capability grant.** `capability_request` lets the agent *ask* — it records a pending
request a human decides on, and cannot confer anything (docs/plugin-capabilities.md §3).

Because of this, the runtime container carries **no Go toolchain, no git, and no
source tree** — only the compiled `nine` binary, which also carries the built-in
tools and the `shell` plugin server. Configuration changes are made by editing
`nine.toml` and restarting the daemon.

---

## Plugins are fixed

Nine ships one plugin, `shell`, alongside its built-in tools. `shell` is immutable
image content: compiled into the `nine` binary and served as
`nine mcp serve shell`. Reading and writing files, fetching over HTTP and
reading the clock are built-ins too — direct in-process Go calls, not subprocesses
holding the daemon's uid (adr/modular_nine.md §9.8/§9.10). There is no mechanism
for an agent to add, build, or replace a plugin or a built-in at runtime. To add a
built-in capability, edit the source repo and rebuild the image.

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
| Skills only | Nine writes skills. It does not generate tools, plugins, rewrite `nine.toml`, or rebuild its own source. Deliberate: a daemon that changes its own form is hard to reason about, debug and trust. |
| Built-in skills are immutable at runtime | `skill_write` and `skill_modify` refuse to touch a skill seeded from the binary. Changing one means editing `skills/*.md` and rebuilding. |
| Capabilities are never agent-writable | The operator writes every plugin capability grant. `capability_request` lets the agent **ask** — it records a pending request a human decides on, and cannot confer anything. |
| Config changes need a restart, except a grant | Editing `nine.toml` takes effect on daemon restart, for agent or operator. The one thing that changes live is a plugin capability grant: an approved request is recorded in the store and the file is reconciled into it at each boot (docs/plugin-capabilities.md §3). |
| Adding a built-in plugin needs a rebuild | An `[[mcp.server]]` is the operator's way to add a capability without rebuilding; it still requires editing `nine.toml` and restarting. Nothing an agent does at runtime can declare one. |
