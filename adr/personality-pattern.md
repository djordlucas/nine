# Personality Pattern for Nine

**Status:** Phase 1 built; phase 2 withdrawn · **Date:** proposed 2025, revised 2026-09-24

> **Where this stands.** §4 (self-model bootstrapping) is implemented, with the
> revisions recorded in §4.6 — the per-field key mapping this note originally
> sketched would not have worked. §5 (buffered input) is **withdrawn**: its goal
> was met by queued messages, built separately, and §5.6 records what that does
> and does not cover. §7's phases are updated to match.
>
> The pattern itself shipped as `docs/personalities.md` before either feature
> existed, so that page documented both as working for a year. The corrected page
> now names them; this note is the record of what was proposed and what happened.
**Related:** [predefined-agents-design.md](predefined-agents-design.md), [roles-design.md](roles-design.md), [session-plans.md](../docs/session-plans.md)

---

## 1. Summary

This document describes the **personality pattern**: a way to package a complete, autonomous Nine instance as a specialized agent with its own identity, knowledge, tools, and growth loop. A personality **takes the entire Nine instance**—there is no composition, no namespacing, and no CLI changes. Each personality is a separate repository that builds on the Nine Docker image and provides:

- A **bootstrapped self-model** (identity, capabilities, goals)
- A **custom toolset** (sandboxed and/or plugin)
- A **custom skill set** (procedural knowledge)
- An **autonomous growth loop** (via standing agents and goals)

Two new Nine-side features are required to support this pattern:

1. **Self-model bootstrapping** – seed the agent's self-model from a declarative file on first boot
2. **Buffered input** – allow operators to queue multiple messages for processing

Everything else is achievable with existing primitives (standing agents, roles, skills, tools, goals).

---

## 2. Motivation

Nine already provides all the pieces needed for an autonomous agent:

- **Roles** define what a session can do and how it approaches tasks
- **Skills** provide on-demand procedural knowledge
- **Sandboxed tools** extend capabilities safely
- **Standing agents** run autonomously on schedules
- **Goals** provide open-ended, persistent objectives
- **The self-model** (`self/identity`, `self/capabilities`, `self/learned`) is a live, evolving description of the agent

However, two gaps prevent a clean "personality as a deployable artifact" pattern:

1. **No self-model seeding** – On first boot, the self-model is either empty or populated by Nine's built-in defaults. A personality needs to start with a predefined identity, capabilities, and growth goal.

2. **No message queuing** – When Nine is busy (e.g., processing a long turn), new messages cannot be queued. For personalities that receive external input (webhooks, APIs, scheduled data dumps), this means messages are dropped or the sender must retry.

This ADR addresses both gaps with minimal changes to Nine's core, while keeping the personality itself as a **separate repository** that builds on the Nine Docker image.

---

## 3. Design Overview

### 3.1 The Personality Pattern

A personality is **not a new primitive** in Nine. It is a **packaging convention** for a complete Nine deployment with:

- A `nine.toml` configuring the personality's role, standing agents, and capabilities
- A `self-model.toml` (or similar) that seeds the agent's self-model on first boot
- A `skills/` directory with personality-specific knowledge
- A `tools.d/` directory with personality-specific sandboxed tools
- Optionally, a `plugins.d/` directory with native plugins

The personality repository builds a Docker image **from** the Nine runtime image:

```dockerfile
FROM ghcr.io/djordlucas/nine:latest

# Copy personality artifact
COPY . /etc/nine-personality

# Set config path
ENV NINE_CONFIG=/etc/nine-personality/nine.toml
ENV NINE_BOOTSTRAP_SELF_MODEL=/etc/nine-personality/self-model.toml

# Set data path
ENV NINE_DB_PATH=/data/nine.db

VOLUME /data
```

### 3.2 Key Constraints

- **One personality per instance** – A personality takes the whole Nine instance. No namespacing, no composition.
- **No Nine code changes for personalities** – All personality logic lives in the personality repository. Nine only provides the bootstrapping and buffered input mechanisms.
- **Separate repositories** – Personalities are developed and versioned independently, building on the Nine Docker image.
- **Nine-side changes only** – Self-model bootstrapping and buffered input must be implemented in Nine itself.

---

## 4. Self-Model Bootstrapping

### 4.1 Goal

Allow a Nine instance to **seed its self-model from a declarative file on first boot**, before any agent turns run. This enables a personality to start with a predefined identity, capabilities, and initial knowledge.

### 4.2 Design

#### 4.2.1 The Bootstrap File

A new optional configuration key and environment variable:

```toml
[bootstrap]
self_model_path = "/etc/nine-personality/self-model.toml"
```

Or via environment:
```bash
NINE_BOOTSTRAP_SELF_MODEL=/etc/nine-personality/self-model.toml
```

If neither is set, Nine behaves as it does today (self-model starts empty or with built-in defaults).

#### 4.2.2 Bootstrap File Format

The bootstrap file is a **TOML file** with a flat key-value structure that maps directly to memory KV entries under the `self/` prefix:

```toml
# self-model.toml
[identity]
name = "Alice"
version = "1.0.0"
purpose = "Code review assistant that learns from PRs"

[persona]
description = "Alice is a senior engineer focused on code review. She analyzes PRs, identifies patterns, and writes skills to remember them."
role = "code-reviewer"
tools = ["file_read", "file_search_text", "memory_get", "memory_set", "skill_write", "skill_list"]
growth_goal = "Analyze 5 PRs/week and write skills for new patterns"

[capabilities]
initial = "Can analyze Go code, identify patterns, write skills, store insights in memory"

[state]
last_pr_checked = "2024-01-01T00:00:00Z"
skills_written = 0
prs_analyzed = 0
```

This file is **loaded once, at first boot**, and its contents are written to the KV store under `self/`. The mapping is:

- `[identity]` → `self/identity/*`
- `[persona]` → `self/persona/*`
- `[capabilities]` → `self/capabilities`
- `[state]` → `self/state/*`
- Any other section `[foo]` → `self/foo/*`

#### 4.2.3 Bootstrap Logic

In `cmd/nine/daemon.go`, after the store is opened but **before** `BootstrapSelfReflection` and `BootstrapSelfKV`:

```go
func loadSelfModelBootstrap(ctx context.Context, store *memory.Store, cfg *config.Config) error {
    bootstrapPath := cfg.Bootstrap.SelfModelPath
    if bootstrapPath == "" {
        bootstrapPath = os.Getenv("NINE_BOOTSTRAP_SELF_MODEL")
    }
    if bootstrapPath == "" {
        return nil // No bootstrap file; use defaults
    }
    
    data, err := os.ReadFile(bootstrapPath)
    if err != nil {
        if os.IsNotExist(err) {
            return nil // File doesn't exist; not an error
        }
        return fmt.Errorf("failed to read bootstrap file: %w", err)
    }
    
    // Parse TOML
    var bootstrap map[string]toml.Primitive
    if err := toml.Unmarshal(data, &bootstrap); err != nil {
        return fmt.Errorf("failed to parse bootstrap file: %w", err)
    }
    
    // Write to store under self/ prefix
    for section, values := range bootstrap {
        prefix := "self/" + section + "/"
        // Handle both table and primitive values
        // ...
    }
    
    return nil
}
```

#### 4.2.4 Bootstrap Guarantees

- **Idempotent** – If `self/identity/name` already exists, the bootstrap is skipped entirely. This prevents accidentally overwriting a running personality's self-model on restart.
- **First-boot only** – The bootstrap runs exactly once per database lifetime. After the first successful load, the presence of `self/_bootstrapped` (or similar sentinel) skips future attempts.
- **Optional** – If no bootstrap file is configured, Nine behaves exactly as it does today.
- **No partial writes** – If parsing or writing fails, the entire bootstrap is aborted and the daemon fails to start (config-time error).

#### 4.2.5 Interaction with Built-in Bootstrapping

The bootstrap runs **before** `BootstrapSelfKV` (which seeds `self/identity` and `self/capabilities` with Nine's defaults). If the bootstrap file provides these keys, the built-in defaults are never written.

Order of operations:
1. Open store
2. `loadSelfModelBootstrap` (if configured)
3. `BootstrapSelfKV` (only if bootstrap didn't write `self/identity`)
4. `BootstrapSelfReflection`
5. ...

#### 4.2.6 Security Considerations

- The bootstrap file is **operator-provided**, not agent-authored. It is read from a path specified in config or environment, outside the workspace.
- The file is **read-only** at runtime. Nine never writes to it.
- The bootstrap cannot grant capabilities. It only populates memory KV entries, which are advisory context for the agent.
- A malicious bootstrap file can only **pollute the self-model**, not escalate privileges.

---

### 4.6 What the build changed (2026-09-24)

Three revisions, each because the sketch above would not have produced the
behavior it promised.

**One key per section, not one per field.** §4.2.2 mapped `[identity]` onto
`self/identity/name`, `self/identity/version` and so on. The self-model assembler
reads *whole keys by name* — `self/identity`, `self/capabilities`,
`self/learned` — so none of those per-field keys would ever have reached a turn.
Worse, `self/identity` would then be filled by the generic default, and a
packaged instance would have introduced itself as stock Nine while its authored
identity sat unread in the store. A section is now rendered to `field: value`
lines and written to `self/<section>`, which is the key the assembler already
reads. Fields are sorted, so one file always produces one text.

**`self/persona` is surfaced.** A `[persona]` section was central to the
reference personality and had nowhere to go. The assembler now reads it between
identity and capabilities. It is absent on a stock deployment and costs a lookup.

**The sentinel is the only idempotency check.** §4.2.4 offered two — "skip if
`self/identity/name` exists" *or* a sentinel. The first cannot work: `self/identity`
is written on every fresh database by `BootstrapSelfKV`, so keying on it would
make the packaged file win only a race. `self/_bootstrapped` answers the question
that actually matters, *has this file already run here*, and is written last so a
failure part-way retries the whole file rather than resuming into a half-applied
self-model.

A fourth point the sketch got right and is worth keeping explicit: a **missing**
configured file warns and boots with defaults, while a **malformed** one stops
the boot. The difference is that a typo'd path is a deployment mistake the
operator can see in the log, whereas a file that parses halfway would produce an
instance whose identity is quietly wrong.

---

## 5. Buffered Input

> **Withdrawn (2026-09-24).** The goal below was met by **queued messages**,
> designed and built separately: a `queued_messages` column on the conversation
> row, five model-facing tools, and a post-turn drain that starts a turn for each
> message the model left unconsumed (`docs/queued-messages.md`). Building §5 on
> top of that would be a second queue for the same job. §5.6 records the delta —
> the parts of this design that queued messages does *not* provide, and whether
> each is worth building on its own.
>
> The rest of this section is kept as the original proposal.

### 5.1 Goal

Allow operators to **queue multiple messages** for a Nine instance to process, even when the agent is busy. This is essential for personalities that receive external input (e.g., webhooks, scheduled data dumps, API calls).

### 5.2 Design

#### 5.2.1 The Input Queue

Introduce a new **input queue** that sits between external senders and the agent's turn loop. The queue:

- Is **persistent** (stored in SQLite alongside the rest of Nine's state)
- Is **ordered** (FIFO)
- Is **per-session** (each conversation/agent has its own queue)
- Has **optional priority** (normal vs. high-priority messages)

New table in the store:

```sql
CREATE TABLE IF NOT EXISTS input_queue (
    id TEXT PRIMARY KEY,           -- UUID
    agent_id TEXT NOT NULL,        -- Session/agent this message is for
    message TEXT NOT NULL,         -- The message content
    priority INTEGER DEFAULT 0,     -- 0 = normal, 1 = high
    created_at TEXT NOT NULL,      -- ISO 8601 timestamp
    processed_at TEXT,             -- ISO 8601 timestamp (null if not processed)
    error TEXT                     -- Error message if processing failed (null if success)
);

CREATE INDEX IF NOT EXISTS idx_input_queue_agent_id ON input_queue(agent_id);
CREATE INDEX IF NOT EXISTS idx_input_queue_created ON input_queue(created_at);
```

#### 5.2.2 Queueing Messages

A new core tool: `input_queue`

```json
{
  "name": "input_queue",
  "description": "Queue a message for an agent to process. Returns the queue ID.",
  "inputSchema": {
    "type": "object",
    "properties": {
      "agent_id": {"type": "string", "description": "The agent/session ID to queue for. If empty, queues for the current session."},
      "message": {"type": "string", "description": "The message to queue."},
      "priority": {"type": "integer", "enum": [0, 1], "default": 0, "description": "Priority: 0=normal, 1=high"}
    },
    "required": ["message"]
  }
}
```

When called, `input_queue`:
1. Validates the message
2. Inserts a row into `input_queue`
3. Returns the queue ID
4. If the target agent is **idle and waiting for input**, triggers a turn immediately

#### 5.2.3 Processing the Queue

The queue is processed in two places:

1. **At turn start** – Before assembling context, the agent worker checks if there are queued messages for its agent ID. If so, it dequeues the highest-priority, oldest message and uses it as the turn input.

2. **After turn completion** – If the queue is not empty and the agent is idle, the worker immediately starts a new turn with the next message.

This ensures:
- Messages are **never dropped** (persistent queue)
- Messages are **processed in order** (FIFO within priority levels)
- High-priority messages **jump the queue**
- The agent **remains responsive** even under load

#### 5.2.4 CLI and API Integration

The existing `nine send` command and `nine <message>` syntax already queue a message for the orchestrator session. With buffered input:

- If the agent is idle, the message is processed immediately (existing behavior)
- If the agent is busy, the message is queued and processed when the agent becomes idle (new behavior)

New CLI commands for queue management:

```
nine queue list [--agent <id>]    # List queued messages (default: all agents)
nine queue show <id>             # Show a specific queued message
ine queue delete <id>           # Remove a queued message
nine queue flush [--agent <id>] # Clear all queued messages for an agent
```

#### 5.2.5 Flow Control

To prevent queue overload:

- **Max queue size** – Configurable per-agent limit (default: 100 messages). New messages are rejected with an error if the limit is exceeded.
- **Processing rate limit** – Configurable max messages per minute per agent (default: no limit).

Configuration:

```toml
[daemon]
# Global defaults for buffered input
max_queue_size = 100
max_queue_rate_per_minute = 0  # 0 = no limit

# Per-agent overrides (optional)
[agent."alice-dev"]
max_queue_size = 50
max_queue_rate_per_minute = 10
```

#### 5.2.6 Error Handling

If processing a queued message fails:
- The `error` field in the queue row is set with the failure reason
- The message is **not retried automatically** (idempotency)
- The operator can inspect with `nine queue list` and retry with `nine queue retry <id>` (which re-queues the message)

#### 5.2.7 Interaction with Existing Input

The existing input paths (TUI, CLI `nine <message>`, `nine send`) are **unified** with the queue:

- TUI input goes directly to the agent (no queuing, as the human is waiting)
- CLI input is queued if the agent is busy, processed immediately if idle
- API input (via Unix socket) is queued if the agent is busy, processed immediately if idle

This maintains backward compatibility while adding queuing for non-interactive input.

---

### 5.6 What queued messages covers, and what it does not (2026-09-24)

| This design asked for | Queued messages | Verdict |
|---|---|---|
| Accept input while the agent is busy | Yes — the message is queued, not dropped, and the model is told it is waiting | Met |
| Persistent across restarts | Yes — a column on the conversation row | Met |
| FIFO per session | Yes | Met |
| Nothing is lost if the model ignores it | Yes, and more directly than this design: the post-turn drain starts a turn per unconsumed message | Met |
| Priority (high jumps the queue) | No | **Not built.** Worth revisiting only with a caller that has two urgency classes. No such caller exists. |
| A durable row per message with `processed_at` and `error` | No — a message is text with a consumed flag | **Not built.** The journal already records what a turn did with its input, which is where a failure would be read. |
| `nine queue list/show/delete/flush` | No | **Not built.** Nothing lists a queue from the CLI, which is a real gap for an operator debugging a stuck personality. The cheapest version is `nine queue list <agent-id>` alone. |
| Queue size and rate limits | No | **Not built.** Unbounded, and consumed messages are never pruned, so a long-lived conversation's queue only grows. |

The first four are the reason this section is withdrawn rather than deferred. The
last four are the honest residue: none of them blocks the personality pattern,
and the CLI view is the one an operator is most likely to miss.

---

## 6. Personality Structure (Reference Implementation)

A personality repository has the following structure:

```
personality-alice/
├── Dockerfile                  # Builds on Nine runtime image
├── README.md                   # Personality documentation
├── nine.toml                   # Nine configuration
├── self-model.toml             # Self-model bootstrap file
├── skills/                     # Personality-specific skills
│   ├── code-reviewer.md        # Role definition
│   ├── go-best-practices.md    # Knowledge skill
│   └── ...
├── tools.d/                    # Personality-specific tools
│   ├── analyze_pr.js           # PR analysis tool
│   ├── analyze_pr.toml         # Tool manifest
│   └── ...
└── plugins.d/                  # Optional: native plugins
    └── ...
```

### 6.1 Example `nine.toml`

```toml
[llm]
provider = "ollama"
model = "qwen3.5:9b"
num_ctx = 49152

[daemon]
max_goal_sessions = 5

[bootstrap]
self_model_path = "/etc/nine-personality/self-model.toml"

# The personality as a standing agent
[[agent]]
id = "alice"
description = "Alice: autonomous code review assistant"
role = "code-reviewer"
schedule = "0 * * * *"  # Wake hourly to check for new PRs

# Tool configuration
[tools]
enabled = true
user_dir = "/etc/nine-personality/tools.d"

[tools.agent]
enabled = true
max_tools = 32
require_approval = "on_capability"

[tools.agent.capabilities.net]
http = [{ allow_hosts = ["api.github.com", "raw.githubusercontent.com"] }]

# Skills configuration
[skills]
user_dir = "/etc/nine-personality/skills"

# Buffered input configuration
[daemon]
max_queue_size = 100
```

### 6.2 Example `self-model.toml`

```toml
[identity]
name = "Alice"
version = "1.0.0"
purpose = "Autonomous Go code review assistant"

[persona]
description = "Alice is a senior Go engineer. She reviews PRs, identifies patterns, writes skills, and maintains a knowledge base of best practices."
role = "code-reviewer"
growth_goal = "Analyze PRs and write skills for new patterns"

[capabilities]
initial = "Can analyze Go code, identify patterns, write skills, store insights in memory, search files"

[state]
last_pr_checked = "2024-01-01T00:00:00Z"
skills_written = 0
prs_analyzed = 0
```

### 6.3 Example Role Skill (`skills/code-reviewer.md`)

```markdown
---
name: code-reviewer
description: Code review specialist with autonomous learning
role:
  tools: [file_read, file_search_text, file_list, memory_get, memory_set, memory_list, skill_write, skill_list, goal_create, goal_get, goal_list, goal_update_status, input_queue]
  delegates: true
  spawns_goals: false
  persists: true
  interactive: true
---

# Code Reviewer Role

You are {{ memory_get("self/identity/name") }}, version {{ memory_get("self/identity/version") }}.
Your purpose: {{ memory_get("self/identity/purpose") }}.

## Growth Protocol

1. On wake (hourly):
   - Read your growth goal: `memory_get("self/persona/growth_goal")`
   - Check for new PRs since `memory_get("self/state/last_pr_checked")`
   - For each PR, analyze and leave review comments
   - Extract patterns and write skills with `skill_write`
   - Update state: `memory_set("self/state/last_pr_checked", now)`

2. On queued message:
   - Read the message from the queue
   - Process it according to your role
   - If the message is a PR diff, analyze it
   - If the message is a command, execute it

3. On new insight:
   - Write a skill documenting the pattern
   - Increment `self/state/skills_written`
   - Update `self/capabilities` with the new capability
```

---

## 7. Implementation Plan

### 7.1 Phase 1: Self-Model Bootstrapping — **built (2026-09-24)**

1. ✅ `[bootstrap] self_model_path`, with `NINE_BOOTSTRAP_SELF_MODEL` overriding it
2. ✅ `runtime.BootstrapSelfModel`, called from `cmd/nine/daemon.go` before
   `BootstrapSelfKV` — in `internal/runtime/selfmodel_bootstrap.go` rather than in
   `cmd`, so it is testable without a daemon
3. ✅ `self/_bootstrapped`, written last
4. ✅ Sections rendered to `self/<section>`, not per-field keys (§4.6)
5. ✅ `self/persona` surfaced by the assembler (§4.6)
6. ✅ Twelve tests, including the two orderings that matter: the packaged
   identity beats the generic default, and a second boot does not overwrite a
   self-model the instance has since revised

`BootstrapSelfKV` needed no change: it already skips a database that has
`self/identity`, which the bootstrap has written by the time it runs.

**Gate:** met — a configured instance starts with the file's self-model, the file
runs once per database, and a malformed file stops the boot.

### 7.2 Phase 2: Buffered Input — **withdrawn (2026-09-24)**

Superseded by queued messages (`docs/queued-messages.md`), which met the goal
with a column rather than a table. §5.6 lists what that leaves unbuilt; the only
item with a plausible caller today is a CLI view of a session's queue, which is
one command and not a phase.

### 7.3 Phase 3: Documentation — **built, then corrected**

`docs/personalities.md` shipped with this note, before either feature existed,
and documented both as working until 2026-09-24. It now describes the built
bootstrap and points at queued messages, and its Limits name what this note
proposed and never got. The example personality under `examples/` was never
added.

1. Add `docs/personalities.md` with:
   - The personality pattern overview
   - How to create a personality repository
   - Example `Dockerfile`, `nine.toml`, `self-model.toml`
   - How self-model bootstrapping works
   - How buffered input works
   - Best practices for personality development

2. Update `README.md` to reference the personalities documentation

3. Add example personality in `examples/personality-alice/`

---

## 8. Open Questions

1. **Bootstrap file format** – Should we support JSON in addition to TOML? TOML is more readable for operators, but JSON might be easier for programmatic generation.

2. **Bootstrap file validation** – Should we validate the bootstrap file structure at load time, or just let invalid keys be ignored?

3. **Queue persistence** – Should processed messages be retained (for audit) or deleted immediately? Current design retains them with a `processed_at` timestamp.

4. **Queue visibility** – Should queued messages be visible to the agent (e.g., via a `queue_list` tool), or only to the operator? Current design is operator-only.

5. **Priority levels** – Are two priority levels (normal/high) sufficient, or do we need more granularity?

---

## 9. Alternatives Considered

### 9.1 Self-Model Bootstrapping

**Alternative: Use skills for bootstrapping**
- Could seed skills at boot from a directory, and have a special "init" skill that writes the self-model.
- **Rejected:** Skills are for agent knowledge, not for instance configuration. The self-model is instance state, not shared knowledge.

**Alternative: Use memory tools in a bootstrap session**
- Could run a one-shot agent on first boot that calls `memory_set` to seed the self-model.
- **Rejected:** Requires an LLM turn on first boot, which adds complexity and latency. A declarative file is simpler and more reliable.

**Alternative: Extend `nine.toml` with a `[self_model]` section**
- Could add self-model keys directly to `nine.toml`.
- **Rejected:** Mixes instance configuration with agent state. The self-model is mutable at runtime; config is not. Keeping them separate is cleaner.

### 9.2 Buffered Input

**Alternative: Use the existing session message history**
- Could store queued messages in the session's message history.
- **Rejected:** Message history is for the agent's context, not for input queuing. It would pollute the context and complicate history management.

**Alternative: Use a separate Redis/Postgres queue**
- Could offload the queue to an external service.
- **Rejected:** Adds operational complexity and external dependencies. Nine's design philosophy is to minimize external dependencies (hence SQLite).

**Alternative: Plugin-based queuing**
- Could implement queuing as a plugin.
- **Rejected:** Plugins cannot modify the agent loop or intercept input. This requires core changes.

---

## 10. References

- [Pre-defined agents design](predefined-agents-design.md) – Standing agents and their lifecycle
- [Roles design](roles-design.md) – Worker kinds as data
- [Session plans and routines](../docs/session-plans.md) – How autonomous behavior works
- [Configuration reference](../docs/configuration.md) – Existing configuration options
- [Skills documentation](../docs/skills.md) – How skills work
- [Sandboxed tools documentation](../docs/sandboxed-tools.md) – How tools work

---

## Appendix A: Example Personality Workflow

### A.1 First Boot

1. Operator starts the personality container:
   ```bash
   docker run -v /data:/data nine-personality-alice
   ```

2. Nine daemon starts:
   - Opens store at `/data/nine.db`
   - Detects first boot (no `self/_bootstrapped` key)
   - Loads `self-model.toml` from `/etc/nine-personality/self-model.toml`
   - Seeds `self/identity/*`, `self/persona/*`, `self/capabilities`, `self/state/*`
   - Sets `self/_bootstrapped = true`
   - Seeds skills from `/etc/nine-personality/skills/`
   - Loads tools from `/etc/nine-personality/tools.d/`
   - Starts the `alice` standing agent

3. The `alice` standing agent wakes on its schedule (hourly):
   - Reads its growth goal from `self/persona/growth_goal`
   - Checks for new PRs
   - Processes any queued messages

### A.2 Receiving Input

1. Operator sends a message while Alice is busy:
   ```bash
   nine "Alice, review this PR: https://github.com/owner/repo/pull/123"
   ```

2. The message is queued in `input_queue` (since Alice is busy)

3. When Alice finishes her current turn, she:
   - Checks the queue
   - Finds the queued message
   - Processes it as her next turn
   - Analyzes the PR
   - Writes a skill for any new patterns found
   - Updates `self/state/prs_analyzed`

### A.3 Growth Loop

1. Alice's hourly wake:
   - Calls `goal_get` for her growth goal
   - Fetches recent PRs from GitHub API
   - For each PR:
     - Analyzes the diff
     - Identifies patterns
     - Writes skills for new patterns
     - Updates `self/state/skills_written`
   - Updates `self/capabilities` with new insights
   - Updates `self/state/last_pr_checked`

2. Over time:
   - Alice's skill set grows
   - Her self-model becomes more accurate
   - Her capabilities expand (within her role's tool allowlist)

---

## Appendix B: Self-Model Bootstrap File Grammar

The bootstrap file is a TOML file with the following structure:

```toml
# Top-level sections become self/<section>/ keys
[section_name]
# Keys within a section become self/<section>/<key>
key1 = "value1"
key2 = 123

# Nested tables become nested keys
[section_name.subsection]
key = "value"  # -> self/section_name/subsection/key

# Arrays are serialized as JSON
[section_name]
array_key = ["a", "b", "c"]  # -> self/section_name/array_key = '["a","b","c"]'

# Inline tables are flattened
[section_name]
inline = { key = "value" }  # -> self/section_name/inline = '{"key":"value"}'
```

All values are stored as strings in the KV store. The agent is responsible for parsing them as needed (e.g., JSON arrays for `self/persona/tools`).

---

## Appendix C: Buffered Input Tool Contract

### `input_queue` Tool

**Purpose:** Queue a message for an agent to process.

**Arguments:**
- `agent_id` (string, optional): The agent/session ID to queue for. If empty, queues for the current session.
- `message` (string, required): The message to queue.
- `priority` (integer, optional, default=0): Priority level. 0 = normal, 1 = high.

**Returns:**
- `queue_id` (string): The UUID of the queued message.
- `position` (integer): The position of the message in the queue (0-indexed).

**Errors:**
- `agent_not_found`: The specified agent_id does not exist.
- `queue_full`: The queue for this agent has reached its maximum size.
- `invalid_message`: The message is empty or invalid.

**Example:**
```json
{
  "agent_id": "alice",
  "message": "Review PR #123",
  "priority": 1
}
```

**Response:**
```json
{
  "queue_id": "550e8400-e29b-41d4-a716-446655440000",
  "position": 0
}
```
