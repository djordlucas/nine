# Personalities

A **personality** is a complete, autonomous Nine deployment packaged as a specialized agent with its own identity, knowledge, tools, and growth loop. A personality takes the **entire Nine instance**—there is no composition, no namespacing, and no multi-personality support within a single instance.

Each personality is a **separate repository** that builds on the Nine Docker image, providing a turnkey deployable artifact for a specific use case (code review assistant, security monitor, research agent, etc.).

---

## Overview

Nine provides all the primitives needed to build an autonomous agent:

- **Roles** define what a session can do and how it approaches tasks
- **Skills** provide on-demand procedural knowledge
- **Sandboxed tools** extend capabilities safely
- **Standing agents** run autonomously on schedules
- **Goals** provide open-ended, persistent objectives
- **The self-model** is a live, evolving description of the agent

The personality pattern **packages these primitives** into a deployable artifact that:

1. **Starts with a predefined identity** (a self-model file seeded under `self/`)
2. **Has custom knowledge** (via skills directory)
3. **Has custom capabilities** (via sandboxed tools)
4. **Runs autonomously** (via standing agents and goals)
5. **Grows over time** (via memory and skill accumulation)

---

## When to use a personality

Build a personality when you want:

- A **specialized Nine instance** for a specific domain (e.g., code review, security monitoring, research)
- **Predefined knowledge and capabilities** that don't require manual setup
- **Autonomous operation** with scheduled tasks and growth loops
- **Easy deployment** as a Docker image that builds on Nine

Do **not** use a personality when you want:

- Multiple personalities in one Nine instance (not supported)
- Shared state between personalities (each has its own database)
- Lightweight customization (use roles and skills instead)

---

## Personality structure

A personality repository has the following structure:

```
personality-<name>/
├── Dockerfile                  # Builds on Nine runtime image
├── README.md                   # Personality documentation
├── nine.toml                   # Nine configuration
├── self-model.toml             # Identity seeded on first boot (optional)
├── skills/                     # Personality-specific skills (optional)
│   ├── <role-name>.md         # Role definition(s)
│   └── <skill-name>.md        # Knowledge skills
├── tools.d/                    # Personality-specific tools (optional)
│   ├── <tool-name>.js         # Tool code
│   ├── <tool-name>.toml       # Tool manifest
│   └── <tool-name>.schema.json # Tool schema
└── plugins.d/                  # Native plugins (optional)
    └── <plugin-name>           # Plugin binary + manifest
```

---

## Creating a personality

### Step 1: create the repository

```bash
mkdir personality-alice
cd personality-alice
git init
```

### Step 2: create the Dockerfile

```dockerfile
# personality-alice/Dockerfile
FROM ghcr.io/djordlucas/nine:latest

# Copy personality artifact
COPY . /etc/nine-personality

# Set configuration path
ENV NINE_CONFIG=/etc/nine-personality/nine.toml

# Set data path (persistent volume)
ENV NINE_DB_PATH=/data/nine.db

# Optional: Set workspace root
ENV NINE_WORKSPACE_ROOT=/workspace

# Expose the Unix socket (optional, for host access)
VOLUME /tmp/nine.sock

# Persistent data volume
VOLUME /data
VOLUME /workspace

# Start the daemon
CMD ["nine", "daemon"]
```

### Step 3: create the configuration (`nine.toml`)

```toml
# personality-alice/nine.toml
[llm]
provider = "ollama"
model = "qwen3.5:9b"
num_ctx = 49152
max_concurrent = 1

[daemon]
socket_path = "/tmp/nine.sock"
max_goal_sessions = 5

# The personality as a standing agent
[[agent]]
id = "alice"
description = "Alice: autonomous code review assistant"
role = "code-reviewer"
schedule = "0 * * * *"  # Wake hourly

# Tool configuration
[tools]
enabled = true
user_dir = "/etc/nine-personality/tools.d"

[tools.agent]
enabled = true
max_tools = 32
require_approval = "on_capability"

# The ceiling a generated tool may be granted — never an automatic grant.
[tools.agent.capabilities.net.http]
allow_hosts = ["api.github.com", "raw.githubusercontent.com"]
methods     = ["GET"]          # required; there is no implicit default

# Skills configuration
[skills]
user_dir = "/etc/nine-personality/skills"
```

Every table appears once: TOML rejects a repeated `[daemon]`, so a second block
for it is a parse error rather than a merge.

### Step 4: seed the self-model (optional)

A personality starts knowing who it is by shipping a **self-model file**: a TOML
file whose sections become `self/*` entries on the instance's first boot.

```toml
# personality-alice/self-model.toml
[identity]
name = "Alice"
purpose = "Autonomous Go code review assistant"

[persona]
description = "Alice is a senior Go engineer. She reviews PRs, identifies patterns, writes skills, and maintains a knowledge base of best practices."
tone = "Direct. States the problem, then the fix."

[capabilities]
initial = "Can analyze Go code, identify patterns, write skills, store insights in memory, search files"
```

Point Nine at it:

```toml
[bootstrap]
self_model_path = "/etc/nine-personality/self-model.toml"
```

or `NINE_BOOTSTRAP_SELF_MODEL=/etc/nine-personality/self-model.toml`, which is
how an image names a file it mounts without rewriting the config it inherited.

**Each section becomes one key**, holding its fields as `field: value` lines:
`[identity]` becomes `self/identity`, `[persona]` becomes `self/persona`. Three
of those keys — `self/identity`, `self/persona`, `self/capabilities` — are read
into every turn's self-model block, along with `self/learned`, which the file
should leave alone: it is where reflection writes. Any other section is stored
and reachable with `memory_get`, but does not ride in the context.

A field's value can be a string, a number, a boolean, or a list (rendered
comma-separated). A nested table is refused with a named error rather than
flattened.

| Behavior | Detail |
|---|---|
| Runs once per database | A `self/_bootstrapped` sentinel is written last. An instance that has since revised its own self-model is not reset to the packaged text on the next restart. |
| Beats the built-in defaults | It runs before them, so `self/identity` is the file's, not the generic description of Nine. |
| A missing file warns | A configured path that is not there logs and boots with the defaults — a typo should not take the daemon down. |
| A malformed file stops the boot | An instance that believes it is generic Nine while its operator believes it is Alice is the failure this prevents. |

After first boot the self-model is the agent's: reflection updates
`self/capabilities` and `self/learned` on its own cadence
([session-plans.md](session-plans.md)). The file is the starting point, not a
description maintained from outside.

### Step 5: create skills

Skills provide the personality's knowledge base. They are stored in the `skills/` directory and seeded into the database on every boot.

```markdown
# personality-alice/skills/code-reviewer.md
---
name: code-reviewer
description: Code review specialist with autonomous learning
tags: [role, code-review, learning]
role:
  tools: [read_file, file_search_text, list_files, memory_get, memory_set, memory_list, skill_write, skill_list, goal_create, goal_get, goal_list, goal_update_status, queued_messages_get]
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

2. On new insight:
   - Write a skill documenting the pattern
   - Increment `self/state/skills_written`
   - Update `self/capabilities` with the new capability
```

### Step 6: create sandboxed tools (optional)

Tools extend the personality's capabilities. They live in the `tools.d/` directory.

```javascript
// personality-alice/tools.d/analyze_pr.js
export default ({ diff, pr_number }) => {
  const lines = diff.split('\n');
  const additions = lines.filter(line => line.startsWith('+') && !line.startsWith('+++'));
  const deletions = lines.filter(line => line.startsWith('-') && !line.startsWith('---'));
  
  return {
    pr_number,
    additions: additions.length,
    deletions: deletions.length,
    files_changed: new Set(lines.filter(line => line.startsWith('diff --git')).map(line => line.split(' ')[2])).size,
    summary: `PR #${pr_number}: ${additions.length} additions, ${deletions.length} deletions across ${files_changed} files`
  };
};
```

```toml
# personality-alice/tools.d/analyze_pr.toml
name = "analyze_pr"
kind = "js"
entrypoint = "./analyze_pr.js"
description = "Analyze a PR diff and return summary statistics"

[capabilities]
# No capabilities needed for this pure computation tool
```

```json
// personality-alice/tools.d/analyze_pr.schema.json
{
  "type": "object",
  "properties": {
    "diff": {"type": "string", "description": "The PR diff text"},
    "pr_number": {"type": "integer", "description": "The PR number"}
  },
  "required": ["diff", "pr_number"]
}
```

### Step 7: build and run

```bash
# Build the personality image
docker build -t personality-alice .

# Run the personality
docker run -d \
  -v /path/to/data:/data \
  -v /path/to/workspace:/workspace \
  -p 11434:11434 \  # Ollama port (if using host Ollama)
  --name alice \
  personality-alice

# Check logs
docker logs alice

# Interact with the personality
docker exec -it alice nine "Alice, analyze this PR"
```

---

## Input while the agent is busy

A message sent to a session that is mid-turn is **queued**, not dropped, and the
model is told it is there. After the turn ends, anything the model left
unconsumed is drained one message at a time, each starting a further turn.

There is no priority, no queue-management CLI, and no configurable size — the
queue is a column on the conversation row and a set of model-facing tools. See
[queued-messages.md](queued-messages.md) for the tools and the drain, and use
`nine send --id <id> <message>` to deliver input to a named session from a
script.

## Personality growth loop

A personality's **growth loop** improves its knowledge and capabilities over time without supervision. Five parts:

1. **Standing agents** that wake on a schedule
2. **Goals** that define open-ended objectives
3. **Memory** that stores insights and state
4. **Skills** that capture procedural knowledge
5. **Tools** that extend capabilities

### Example growth loop

```
# Hourly wake
1. Standing agent "alice" wakes on schedule
2. Reads its growth goal from self/persona/growth_goal
3. Checks for new PRs since self/state/last_pr_checked
4. For each PR:
   a. Analyzes the diff
   b. Identifies patterns
   c. Writes skills for new patterns (skill_write)
   d. Stores insights in memory (memory_set)
   e. Updates state (self/state/prs_analyzed++, self/state/skills_written++)
5. Updates self/state/last_pr_checked to now
```

### Growth mechanisms

| Mechanism | Tool | Purpose | Persistence |
|-----------|------|---------|-------------|
| Self-model | `memory_set` | Update identity, capabilities, state | KV store |
| Skills | `skill_write` | Add new procedural knowledge | Skills table |
| Memory | `memory_set` | Store insights and data | KV store |
| Goals | `goal_create` | Define new objectives | Goals table |
| State | `memory_set` | Track progress | KV store |

---

## Best practices

### 1. Start simple

Begin with a minimal personality:
- A `nine.toml` with a standing agent
- A basic role skill
- No custom tools, and the default self-model seeds

Test that it starts and runs, then add complexity.

### 2. Use Self-Model for configuration

Store personality configuration in the self-model, not in code:

```toml
[persona]
growth_goal = "Analyze 5 PRs/week"
max_concurrent_analyses = 3
```

This allows the personality to **adapt its behavior** based on its own state.

### 3. Design for idempotency

Personalities should be **idempotent**—running the same operation twice should have the same result:

- Use `memory_get` before `memory_set` to check if a value exists
- Check if a skill exists before writing it
- Use unique IDs for goals and sub-goals

### 4. Handle errors gracefully

- Catch tool errors and continue with fallback behavior
- Log errors to memory for later inspection
- Use `goal_update_status` to mark goals as `paused` on repeated failures

### 5. Document the personality

Include in your `README.md`:
- What the personality does
- How to configure it
- What tools and capabilities it needs
- How to interact with it
- Examples of its growth loop

### 6. Version the personality

Use semantic versioning for your personality:
- Update `self/identity/version` on releases
- Document breaking changes
- Maintain a changelog

### 7. Test the personality

Test your personality with:
- Manual interaction via TUI
- Automated tests via CLI
- Long-running tests to verify growth loop
- Edge cases (empty input, malformed input, etc.)

---

## Example personalities

### 1. Code review assistant (Alice)

- **Purpose**: Review PRs and identify patterns
- **Tools**: GitHub API (via sandboxed tool), file analysis
- **Skills**: Code review best practices, pattern recognition
- **Growth**: Writes skills for new patterns found in PRs
- **Schedule**: Hourly wake to check for new PRs

### 2. Security monitor (Bob)

- **Purpose**: Monitor repositories for security issues
- **Tools**: CVE database access, dependency scanning
- **Skills**: Security best practices, vulnerability patterns
- **Growth**: Writes skills for new vulnerability types
- **Schedule**: Daily wake to scan dependencies

### 3. Research assistant (Charlie)

- **Purpose**: Research topics and write reports
- **Tools**: Web search, page reading, file storage
- **Skills**: Research methodologies, report writing
- **Growth**: Writes skills for new research techniques
- **Schedule**: On-demand (triggered by queued messages)

### 4. Documentation generator (Dana)

- **Purpose**: Generate documentation from code
- **Tools**: File reading, code analysis
- **Skills**: Documentation standards, code understanding
- **Growth**: Writes skills for new code patterns
- **Schedule**: On file changes (via standing tool or external trigger)

---

## Deployment patterns

### Local development

```bash
# Clone the personality
git clone https://github.com/your-org/personality-alice
cd personality-alice

# Build the image
docker build -t personality-alice .

# Run with local Ollama
docker run -it \
  -v /path/to/data:/data \
  -v /path/to/workspace:/workspace \
  -e OLLAMA_HOST=host.docker.internal \
  --network host \
  personality-alice
```

### Production deployment

```bash
# Pull the personality image
docker pull ghcr.io/your-org/personality-alice:latest

# Run with persistent volumes
docker run -d \
  -v /var/lib/nine/alice:/data \
  -v /var/lib/nine/workspace:/workspace \
  -e OLLAMA_HOST=http://ollama-server:11434 \
  --restart unless-stopped \
  --name alice \
  ghcr.io/your-org/personality-alice:latest
```

### Kubernetes deployment

```yaml
# personality-alice-deployment.yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: personality-alice
spec:
  replicas: 1
  selector:
    matchLabels:
      app: personality-alice
  template:
    metadata:
      labels:
        app: personality-alice
    spec:
      containers:
      - name: alice
        image: ghcr.io/your-org/personality-alice:latest
        volumeMounts:
        - name: data
          mountPath: /data
        - name: workspace
          mountPath: /workspace
        env:
        - name: OLLAMA_HOST
          value: http://ollama-service:11434
        - name: NINE_WORKSPACE_ROOT
          value: /workspace
      volumes:
      - name: data
        persistentVolumeClaim:
          claimName: alice-data
      - name: workspace
        persistentVolumeClaim:
          claimName: alice-workspace
```

---

## Troubleshooting

### Personality won't start

1. **Check the config was found**:
   ```bash
   docker logs alice | grep -i "config"
   ```

2. **Check the config parses**. An unknown key is ignored silently, but a
   malformed file or a repeated table is a load error and appears in the log:
   ```bash
   docker logs alice | grep -i "toml\|parse"
   ```

3. **Check for missing dependencies**:
   ```bash
   docker logs alice | grep "error"
   ```

### Messages appear to be ignored

A message sent while the session is mid-turn is queued rather than dropped, and
the model is told it is waiting. Nothing lists a queue from the CLI, so the
checks are indirect: the model reads it with `queued_messages_get`, and an
unconsumed message starts its own turn once the current one ends
([queued-messages.md](queued-messages.md)).

1. **Check agent status**:
   ```bash
   nine status
   ```

### Personality doesn't grow

1. **Check the standing agent**:
   ```bash
   nine goals
   ```

2. **Check the self-model**:
   ```bash
   nine "memory_get self/persona/growth_goal"
   ```

3. **Check for errors in the growth loop**:
   ```bash
   nine trace alice
   ```

---

## Reference

- [Pre-defined agents](predefined-agents.md) – Standing agents and their lifecycle
- [Roles](roles.md) – Worker kinds as data
- [Skills](skills.md) – Procedural knowledge
- [Sandboxed tools](sandboxed-tools.md) – Safe capability extensions
- [Session plans](session-plans.md) – Autonomous behavior
- [Configuration](configuration.md) – All configuration options
- [CLI Usage](usage.md) – Command-line interface

---

## See also

- [ADR: Personality Pattern](../adr/personality-pattern.md) – The design document for this feature

## Limits

| Limit | Detail |
|-------|--------|
| A personality is configuration, not a feature | It is a `nine.toml`, a set of skills, and optionally some sandboxed tools, packaged in an image. Nothing in the daemon knows what a personality is. |
| Each needs its own image and container | There is no way to run two personalities in one daemon. They are separate deployments with separate databases. |
| The self-model is seeded once, then owned by the agent | With no file configured, `self/identity` and `self/capabilities` get Nine's generic defaults on first start. Either way, what the instance believes about itself after that is whatever reflection wrote. |
| Skills are copied, not shared | Two personalities that need the same skill each carry their own copy. There is no shared skill registry. |
| Still the design under `adr/` | The pattern is documented and usable, but it remains a convention rather than a supported product surface — see [`adr/personality-pattern.md`](../adr/personality-pattern.md). |
| Only the surfaced keys reach a turn | `self/identity`, `self/persona`, `self/capabilities` and `self/learned` ride in the self-model block. A file section named anything else is stored under `self/<section>` and reachable with `memory_get`, but nothing puts it in front of the model. |
| The self-model is seeded, never re-synced | Editing the file after first boot changes nothing: the sentinel has been written. Re-seeding means clearing `self/_bootstrapped` and the keys by hand, or starting from a fresh database. |
| The input-queue goal is met differently | "Accept input while the agent is busy" is covered by [queued messages](queued-messages.md): a column on the conversation row, model-facing tools, and a post-turn drain. What the ADR wanted and this does not provide is priority ordering, a durable per-message row with error state, and any CLI to inspect or flush a queue. |
