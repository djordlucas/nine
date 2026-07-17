# Self-Improvement & Boundaries

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
| Generate / build / hot-swap plugins | **Removed** | Runtime code generation makes the running system drift from its source |
| Modify `nine.toml` at runtime | **Removed** | Config is set by the operator; changes require a deliberate restart |
| Modify built-in skills at runtime | **Removed** | Curated defaults should not drift on a running instance; edit the repo + rebuild |
| Rebuild its own Go source (`nine_propose_rebuild`) | **Removed** | Nine never edits and recompiles itself |
| Read its own source tree (`self_read`) | **Removed** | The source tree is no longer shipped in the container |

Because of this, the runtime container carries **no Go toolchain, no git, and no
source tree** — only the compiled `nine` binary, the compiled default plugins, and
the browser plugin's JavaScript. Configuration changes are made by editing
`nine.toml` and restarting the daemon.

---

## Plugins are fixed

Nine still runs a set of **default plugins** (`shell`, `files`, `http`, `time`,
`browser`), but they are immutable image content built at `docker build` time and
run from `/opt/nine/bin`. There is no mechanism for an agent to add, build, or
replace a plugin at runtime. To add a capability, add a plugin to the source repo
and rebuild the image.

A plugin remains an isolation boundary: each runs as a subprocess, so a crash takes
down only that plugin, not the daemon. Recovery (restarting a crashed plugin from its
existing binary) is the plugin manager's responsibility — no recompilation is involved.
