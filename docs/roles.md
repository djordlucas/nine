# Roles

A **role** is a worker kind, expressed as data: what a session may do, what it
knows about how to do it, and what structural machinery it gets. Every session
runs under one — the conversation you type into, a background goal session, and
a one-shot sub-agent all differ by role and not by special cases in the loop.

A role bundles three things:

- **A persona.** How this kind of worker approaches its job, written as prose.
- **A tool allowlist.** Which tools it can call, enforced rather than requested.
- **Structural wiring.** Whether it persists across restarts, runs routines,
  can ask you questions, may delegate, and may spawn goal sessions.

The tool allowlist is the part that has to be enforced rather than described. A
persona is advisory context and can be dropped when the context budget is
tight; a tool boundary that could be dropped under budget pressure would not be
a boundary. So the allowlist is applied when the session is built **and**
checked again when a tool is called — a role that cannot run shell commands
does not merely lack the description of one.

## Roles are skills

A role is a skill that declares a role block in its frontmatter. A persona is
the kind of content skills already hold, so roles reuse skill storage, seeding
and authoring wholesale. Writing a new role means writing a skill.

Built-in roles ship with Nine. Operators can author their own, and so can
agents — with one restriction covered under [boundaries](#the-self-improvement-boundary).

One detail matters when authoring one: a `tools` list of `"*"` grants every
tool, and **omitting the `tools` key entirely also grants every tool**. An empty
list grants none. So a role written without thinking about tools is a maximally
privileged role, not a minimal one — write the allowlist deliberately.

## The roles Nine ships

**Structural roots** — the long-lived session kinds:

| Role | Tools | Delegates | Spawns goals | Persists | Interactive |
|---|---|:---:|:---:|:---:|:---:|
| `orchestrator` | all | ✓ | ✓ | ✓ | ✓ |
| `pursue` | all | ✓ | ✗ | ✓ | ✗ |
| `reflection` | memory tools, `skill_read` | ✗ | ✗ | ✓ | ✗ |

`orchestrator` is the session that follows your conversation. `pursue` works a
[goal session](goal-sessions.md) in the background — it can delegate, but it
cannot spawn further goal sessions, so pursuit does not multiply itself.
`reflection` maintains Nine's self-model and is deliberately given almost
nothing: it reads and writes memory and nothing else.

**Leaf roles** — the workers a delegation spawns:

| Role | Tools | Notes |
|---|---|---|
| `executor` | all | The default when a delegation names no role, and the one `[roles] default_leaf` names. The only leaf that may sub-delegate. |
| `software-dev` | shell, the workspace file tools, search, memory, skills | Implement and modify code; run builds and tests. |
| `sysadmin` | shell, `read_file`, `write_file`, `http_get`, `http_post`, memory | Inspect and operate the system. The narrowest file surface of the writing roles: no edit, move, copy or delete. |
| `report-writer` | web search, page reading, `http_get`, the workspace file tools, search, memory | Research and write. **No shell**, and no network beyond GET — it writes files, so its output has somewhere to go. |
| `monitor` | web search, page reading, `http_get`, file reads, search, memory | Read-only: it can see the workspace and change nothing in it. The usual role for a [standing agent](predefined-agents.md). |
| `code-reviewer` | `read_file`, `list_files`, `file_search_text`, `diff_file`, `trash_list`, memory, skills | Review code without changing it. **No shell**, so it cannot run the tests of what it reviews. |
| `analyst` | none | Reasons about a request and produces a short plan. The substitute for models with no native thinking. |

Every leaf role except `executor` is barred from delegating, so a coarse leaf
cannot spawn further work. `analyst` having *no* tools is the point of it: it
is asked to think, and it has nothing else available to do.

## Why roles are coarse

Handing each sub-agent a bespoke tool list would be more precise. Named roles
are used instead, for reasons that compound:

**The tool block stays cacheable.** A role's tool definitions and system prompt
are stable across every spawn of that role, so they can be cached. A bespoke
per-spawn allowlist changes the prefix every time and defeats that.

**A small tool surface is a more accurate one.** A leaf sifting five relevant
tools makes fewer wrong calls than one sifting the entire catalog, and spends
fewer tokens doing it.

**Delegation gets cheaper to decide.** Picking a role by name from a short menu
is a smaller decision than enumerating tool names, and it is a decision about
the *kind of work*, which is what the caller actually knows.

**The blast radius is structural.** `report-writer` cannot run shell commands.
That is a property of the system, not an instruction in a prompt that a model
might reason its way around.

## Choosing a role when delegating

A delegating session picks a role by name. Naming none gets `executor`, which
carries the full toolset and is the compatible default.

Delegation depth remains capped independently, at `[roles] max_delegation_depth`
(default 2), decremented on every spawn. At zero the delegation tools are not
registered at all, even for a delegating role. The cap is a guardrail behind the
role system, not the mechanism by which roles work: termination is primarily
structural, because leaf roles do not delegate in the first place.

`[roles] default_leaf` names the role a delegation gets when it names none, and
defaults to `executor`.

## Roles and MCP tools

An MCP server's tools carry a prefix chosen by the operator in configuration,
and allowlists match tool names exactly rather than by pattern. A built-in role
therefore can never name an MCP tool, and will not receive one however the
deployment is configured. This is why `report-writer` researches over HTTP even
where a browser is available.

Two ways around it, both the operator's to take: author a role naming the
prefixed tools — the operator knows their own server names — or use a role that
takes all tools, which receives whatever is loaded.

## The self-improvement boundary

Nine can write skills, and roles are skills, so Nine can in principle write
roles. The structural flags are what it cannot set: an agent-authored role is
forced to leaf defaults, so it cannot grant itself persistence, delegation,
goal-spawning, or the ability to prompt a human.

A system able to author its own privileges has a preference, not a boundary. An
agent may describe a new kind of worker; it may not promote one.

## Limits

| Limit | Detail |
|-------|--------|
| Omitting `tools` grants everything | A `tools` list of `"*"` grants every tool, and leaving the key out does the same. An empty list grants none. A role written without thinking about tools is maximally privileged, not minimal. |
| Built-in roles cannot reach MCP tools | Allowlists match tool names exactly, and an MCP server's tools carry an operator-chosen prefix, so no built-in role can name one. Author a role naming the prefixed tools, or use a role that takes all tools. |
| Agent-authored roles are forced to leaf defaults | Nine can write roles, because roles are skills, but cannot set the structural flags: no persistence, delegation, goal-spawning, or prompting a human. |
| Only `executor` sub-delegates | Every other leaf role is barred from delegating, so a coarse leaf cannot spawn further work. |
| Personas are advisory | A persona is context and can be dropped under budget pressure. Only the tool allowlist is enforced, at session build and again at call time. |
| Delegation depth is capped separately | The cap is a guardrail behind the role system, not part of it. A role that may delegate still does not delegate without limit. |

## Related

- [`spec/contracts/roles.md`](../spec/contracts/roles.md) — the normative
  requirements. Where this document and the spec disagree, the spec wins.
- [Skills](skills.md) — the storage and authoring roles are built on
- [Goal sessions](goal-sessions.md) and [pre-defined agents](predefined-agents.md) — the roots that use these roles
- [Self-improvement & boundaries](self-modification.md) — what Nine may change about itself

> Why worker kinds became data, and the migration off depth-based gating —
> [../adr/roles-design.md](../adr/roles-design.md).
