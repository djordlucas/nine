# CLI usage

## Commands

```
nine                             Launch the interactive TUI (no arguments)
nine <message>                   Send a message; auto-starts the daemon if needed
nine help                        Show this usage reference (also --help, -h)
nine docs [topic]                Show bundled documentation (no topic lists them)
nine spec [topic]                Show a bundled specification (no topic lists them)
nine version                     Print the version and exit
nine daemon                      Start the daemon in the foreground

nine send [--id <id>] <message>  Non-interactive one-shot turn (scripting);
                                 prints the agent id to stderr, the reply to stdout
nine status                      Show daemon info, active agents, loaded plugins
nine context <agent-id> [-v]     Show the session's assembled-context token
                                 breakdown (no LLM call; -v dumps the full prompt)
nine attach <agent-id>           Reconnect to an existing conversation/session
nine stop <agent-id>             Terminate a session (stops the worker and
                                 deletes its saved state)
nine stop --all                  Terminate every active session
nine tools standing              List standing tools (run indefinitely)
nine tool status <id>            One standing tool, with recent activity
nine tool logs <id> [-n N]       A standing tool's recent calls
nine tool stop|start <id>        Stop or start a standing tool
nine tool call <name> ['<json>'] Run one tool once, for testing
                                 (--live-state uses its real store)
nine sessions                    List sessions: age, journal size, retention
nine session show <agent-id>     One session in full
nine session delete <agent-id>   Erase a session and everything keyed to it
                                 (asks first; --force skips the prompt)

nine goals                       List active goals
nine reflections [agent-id]      Reflection history from the journal (default: self-reflection)
nine notifications [--all]       Show the human-facing notification feed
                                 (--all includes already-seen entries)
nine workflows                   List workflows (active and recent)
nine workflow stop <id>          Cancel an ongoing workflow
nine workflow fail <id>          Mark a stale workflow as failed
nine workflow fail --all         Mark all stale workflows as failed

nine skills validate [path]      Check your own skill/role files against the
                                 format Nine seeds from (defaults to
                                 [skills].user_dir; works with the daemon down)

nine plugins                     Show the plugin roster (built-in + user, with
                                 any skipped user plugins and the reason)
nine plugins reload              Re-scan [plugins].user_dir and reload user
                                 plugins live (built-ins untouched)
nine plugin validate [path]      Check a user plugin with the load-time handshake
                                 (defaults to [plugins].user_dir; a path may be a
                                 manifest or a binary; works with the daemon down)

nine tools                       Show the sandboxed-tool roster: each tool with
                                 the capabilities it actually runs with, plus any
                                 skipped at load and the reason
nine tools show <name>           Print one sandboxed tool in full: kind, status,
                                 resolved grant, manifest path
nine tools reload                Re-scan [tools].user_dir and reload sandboxed
                                 tools live
nine tools deps                  List the external npm packages generated tools
                                 resolved, with the tool that asked for each
nine tool validate [path]        Check a sandboxed tool's manifest, entrypoint,
                                 schema, and ABI exports (defaults to
                                 [tools].user_dir; works with the daemon down)

nine backup <destination.db>     Write a consistent snapshot of the store; no
                                 downtime, one file, refuses to overwrite
nine trace <agent-id> [--turn N] [--sub-agents]
                                 Print a session's event journal (or one turn);
                                 --sub-agents nests delegated sub-agent traces
                                 inline; works even with the daemon down
nine replay <agent-id> --turn N  Reconstruct one recorded turn in full detail —
                                 every LLM call and tool I/O; works with the
                                 daemon down

nine api serve [--port] [--host] [--auth-token]
                                 Start the HTTP API server (see [api.md](api.md))
nine api status                  Check if the API server is running
nine api stop                    Stop the API server
```

---

## TUI slash commands

When running the interactive TUI (`nine` with no arguments), you can type slash commands directly in the input box.

Typing `/` opens a picker above the input listing every command with a short
description. It narrows as you keep typing (`/to` leaves just `/tools`), and it
closes as soon as the leading `/` is deleted, an argument is started, nothing
matches, or you press Esc.

| Key | While the picker is open |
|-----|--------------------------|
| `↑` / `↓` | Move the highlight (`ctrl+p` / `ctrl+n` also work) |
| `Tab` | Complete to the highlighted command without running it |
| `Enter` | Run the highlighted command — or, if it takes an argument, complete it and wait for you to type one |
| `Esc` | Dismiss the picker (Esc quits the TUI when it is closed) |

The picker stays out of the way while Nine is waiting on an answer to an
`ask_human` question, where the input box is free text.

| Command | Description | Example |
|---------|-------------|---------|
| `/help` | List all slash commands | `/help` |
| `/sessions` | List running sessions (copy an ID to reattach with `nine attach`) | `/sessions` |
| `/status` | Daemon uptime, active agents, loaded plugins | `/status` |
| `/config` | Show running configuration (the embeddings API key, the only one left, is masked) | `/config` |
| `/context [id]` | Assembled-context token breakdown for the current (or given) session | `/context` |
| `/tools [filter]` | List all available tools, grouped by provider (optional name filter) | `/tools shell` |
| `/skills [name]` | List skills, or show the full content of a specific skill | `/skills git-workflow` |
| `/memory [key]` | List all KV memory keys, or show the value at a specific key | `/memory self/identity` |
| `/goals` | List active goals | `/goals` |
| `/workflows` | List active and recent workflows | `/workflows` |
| `/standing [id]` | List standing tools, or show one with its recent activity | `/standing corpus` |
| `/plan-mode <mode>` | Change the session's reasoning mode live: `off`, `plan-only`, or `always` | `/plan-mode always` |
| `/think <message>` | Send a message with reasoning forced on for this one turn | `/think reconcile these two specs` |
| `/new` | Start a fresh conversation | `/new` |
| `/clear` | Clear the screen | `/clear` |

Most slash commands are handled locally and consume no LLM tokens; type `/help`
in the TUI for the same list. `/think` is the exception — it sends a normal turn
(and so runs the model) with native thinking / the analysis pass forced on,
regardless of the session's plan mode.

---

## Sending messages

The simplest use is passing a message directly:

```bash
./nine "What's the current date and time?"
```

Nine starts the daemon automatically if it isn't already running, sends the message, waits for the response, and prints it. The daemon stays running in the background for subsequent calls.

### Continuing a conversation

Each call to `nine <message>` continues the same conversation thread. The agent remembers prior messages in the session.

```bash
./nine "Read the file /etc/hosts"
./nine "Now count how many lines contain '127'"
./nine "Store that count in memory under the key 'loopback_count'"
```

### Attaching to a specific agent

If you want to connect to a different session (e.g., a background goal-pursue session):

```bash
./nine status
# output includes active agent IDs

./nine attach a1b2c3d4-...
```

---

## Command reference

### `nine daemon`

Starts the daemon in the foreground. Useful for debugging (logs go to stderr):

```bash
NINE_LOG_LEVEL=debug ./nine daemon
```

The daemon listens on the Unix socket defined in `nine.toml` (default: `/tmp/nine.sock`).

### `nine status`

Shows a snapshot of the running daemon:

```
Daemon Status
  Uptime:   14m22s
  Agents:   2 active

Loaded Plugins
  shell     1 tool
  files     2 tools
  http      4 tools
  time      1 tool
```

An MCP server appears here too, under `mcp:<name>` (e.g. `mcp:playwright  24 tools`),
as does any plugin from `plugins.d/`. A plugin withheld by `[plugins].disabled` is
shown as `off` rather than omitted.

(Memory, file, vector, and skill tools are core-intercepted, not plugins.)

### `nine goals`

Lists top-level goals and their sub-goals:

```
Active Goals
  ID    Status    Description
  1     active    Keep the deploy docs in sync with the codebase
    ↳ done        Audit current docs against the code
    ↳ active      Draft updates for the three stale sections
```

### `nine reflections`

Prints a session's reflection turns, oldest first — the self-reflection session by
default, or any agent id:

```bash
./nine reflections                 # the dedicated self-reflection session
./nine reflections sec-watch       # a standing agent that carries a reflect routine
```

It reads the **journal**, not a dedicated table, so it needs no running daemon (like
`nine trace`) and works for every session that reflects. Reflection used to be a session
*kind* writing to a `reflections` table with no agent id; now it is a routine kind any
session can carry as a routine (`[[agent.routine]]`), so the history has to be per-agent.

For the full trace of a session — tool calls, LLM requests, timings — use
`nine trace <agent-id>`; this is the digest view of the same events.

### `nine notifications`

Prints the human-facing notification feed that background agents post to (e.g. a
standing agent reporting a finding). By default it drains unseen entries; `--all`
includes ones already seen:

```bash
./nine notifications          # unseen entries (marks them seen)
./nine notifications --all     # full history
```

This reads directly from the store, so it works even when the daemon is down.

### `nine workflows` / `nine workflow`

List multi-step workflows, or act on one:

```bash
./nine workflows                       # active and recent workflows
./nine workflow stop  wf-abc12345       # cancel an in-flight workflow
./nine workflow fail  wf-abc12345       # mark one stalled workflow failed
./nine workflow fail  --all             # mark all stalled workflows failed
```

`stop`/`fail` work whether the daemon is up or down (when down, the CLI opens the
store directly).

### `nine send`

A non-interactive one-shot turn, for scripting and automation. It prints the reply
to **stdout** and the session id (as `id=<agent-id>`) to **stderr**, so you can
reuse the id to drive more turns on the same session:

```bash
# New conversation — reply on stdout, "id=<agent-id>" on stderr.
./nine send "summarize the README"

# Continue that same session by passing its id.
./nine send --id a1b2c3d4-... "now list the open questions"
```

Unlike `nine <message>` (which reuses the CLI's default conversation), `nine send`
hands you an explicit session id per conversation.

### `nine attach`

Reconnect the TUI to a specific session — for example a background goal-pursue
session — using an id from `nine status` or the TUI `/sessions` command:

```bash
./nine status                 # lists active agent IDs
./nine attach a1b2c3d4-...
```

On reconnect the full conversation transcript is restored — prior prompts, tool
activity, and responses — so the session looks as it did before you detached,
not like a fresh conversation. The history is reconstructed from the durable
event journal, so it survives daemon restarts too.

### `nine stop` — terminate a session

Stops a session and deletes its saved state, so it is not revived on the next
turn or attach, nor resumed after a daemon restart. Use it to clean up a
misbehaving background session or one created by accident — for example a
mistyped command:

```bash
./nine status                 # find the session's id
./nine stop a1b2c3d4-...      # terminate that one session
./nine stop --all             # terminate every active session
```

`stop` acts on running state, so it needs the daemon up; with none running there
is nothing to stop.

### `nine tools standing` — watching what runs on its own

A **standing tool** is a sandboxed tool the daemon runs indefinitely on its own
cadence, declared in `nine.toml`. These are the commands for seeing what they are
doing and stopping them.

```bash
./nine tools standing            # the roster: state, cycles, trigger
./nine tool status corpus        # one of them in full
./nine tool logs corpus -n 50    # its recent calls
./nine tool stop corpus          # exact — no further call is scheduled
./nine tool start corpus         # clears the failure history and calls now
```

A failing tool says so in the roster, with its consecutive-failure count and last
error, so an old quiet tool explains itself rather than needing a log dive.

`nine tool logs` reads an **in-memory** ring buffer, not the journal, and empties
on restart. That is deliberate: a tool on a ten-second cadence makes thousands of
calls a day and none of them are journal events. What *is* journaled is its
transitions and its output — `nine trace <id>` reads those, and survives.

### `nine tool call` — running one tool once

```bash
./nine tool call csv_stats '{"path":"/data/x.csv"}'
```

Prints the whole envelope, including the `continue` a resumable tool returns —
cursor, progress and requested delay — because that is what you are debugging.

**It runs against scratch state.** If the tool has durable state, a test call gets
its own throwaway namespace, discarded afterwards. Without that, testing a tool
could overwrite the cursor of a live standing run and you would have broken
production by checking something. `--live-state` opts in when reproducing a bug
needs the real store. Capabilities are never sandboxed — a test that cannot make
the tool's real calls tests nothing.

### `nine sessions` / `nine session delete` — the roster, and erasing one

`stop` ends a session and keeps its history. **`session delete` destroys it** —
the transcript, the journal, the notifications, the session's tool state, and
its background jobs all go, in one transaction. It is the only command in Nine
that removes history rather than bounding it, so it asks you to type the id back
before it acts (`--force` skips that).

```bash
./nine sessions                       # the roster, with age and journal size
./nine session show a1b2c3d4-...      # one session in full
./nine session delete a1b2c3d4-...    # erase it, after confirming
```

Sessions are also deleted automatically once they have gone
`[daemon] session_retention_days` without activity — ten days by default, `0` to
switch it off. Two things are never taken, whatever their age:

- a session whose id matches an **active goal** (a goal's pursue session), and
- a session carrying an **active session plan** (a standing agent, for example).

Both are idle *by design*: a standing agent that wakes weekly looks abandoned
after ten days precisely because it is working correctly. `nine sessions` marks
them `kept`, so an old session that is not being reaped explains itself.

Age is measured from **last activity**, not creation — a conversation you have
had running for a year and used this morning is never stale.

> **Typos don't start sessions.** A mistyped command that closely matches a real
> one (e.g. `nine staus`) is reported as an error with a suggestion and the usage
> reference, instead of being sent as a message. Genuine one-word messages that
> don't resemble any command still go through as normal.

### `nine context` — inspect a session's assembled context

`nine context` shows how a session's token budget is currently spent: a per-priority
breakdown (system core, tool definitions, self-model, enrichment, history,
scratchpad, extras), each with its token cost and whether it was included or dropped
for budget. It snapshots the **same deterministic assembly** the agent runs before
every turn, so it performs **no LLM call** (it does run local embedding / memory
retrieval, as a real turn would, to rank tools and surface the self-model). Add
`--verbose` (`-v`) to also dump the fully-assembled system prompt and message list:

```bash
./nine context a1b2c3d4-...             # token breakdown
./nine context a1b2c3d4-... --verbose   # + full assembled prompt and messages
```

In the TUI, `/context` targets the current session (or `/context <id>` another one).

### `nine backup` — snapshot the store

```bash
nine backup /data/backups/nine-$(date -u +%Y%m%dT%H%M%SZ).db
```

Writes one self-contained SQLite file. The daemon can be running: the store is
opened read-only and the snapshot comes from `VACUUM INTO`, so it never blocks
the writer and never produces the torn copy that `cp` of a live WAL database
does. The destination must not already exist.

Take one before every upgrade — a schema migration is forward-only, and an
older binary refuses a database a newer one has migrated. See
[Operations](operations.md).

### `nine trace` — inspect a session's journal

Every session's execution is recorded to a durable event journal
([event log](event-journal.md)). `nine trace` prints that timeline — turn boundaries,
exact LLM requests/responses, tool calls with timing, and the final answer. It
reads the journal directly, so it **works with the daemon down**:

```bash
./nine trace a1b2c3d4-...              # the whole session
./nine trace a1b2c3d4-... --turn 3     # just turn 3
./nine trace a1b2c3d4-... --sub-agents # nest delegated sub-agent traces inline
```

A delegated sub-agent runs under its own ID and journals its own trajectory. By
default `nine trace` shows only the parent's `sub_agent_start`/`sub_agent_end`
markers; `--sub-agents` expands each marker into that sub-agent's full trace,
indented beneath it, recursing to any delegation depth.

### `nine replay` — reconstruct one turn in full detail

`nine replay` reprints a single recorded turn from the journal, in the detail
`nine trace` has no room for: each inner LLM call with its token budget, message
window and tool names; each response with its stop reason, text and tool calls;
each tool's output with timing, attempt count and errors. Like `trace` it reads
the journal directly, so it works with the daemon down.

It is **observational** — it renders the record, it does not re-execute. No LLM
or tool call is made:

```bash
./nine replay a1b2c3d4-... --turn 3
```

The pair works zoomed out to zoomed in: `nine trace` to find the turn that went
wrong, `nine replay --turn N` to read it.

Deterministic **re-execution** — rebuilding the agent loop on a *recorded*
provider and dispatcher so a recorded session runs again with no live LLM or tool
calls and reproduces its answers — is a separate, programmatic surface
(`replay`). It backs the record-then-replay test gate and the eval
suite's replay track ([evals](evals.md)); no CLI command exposes it.

### `nine docs` / `nine spec` — read the bundled docs

The full documentation and specification are embedded in the binary, so they are
always available and always match the version you are running — no source tree
needed. Run either command with no argument to list the topics, or name a topic
to print it:

```bash
./nine docs                       # list every documentation topic
./nine docs workflows             # print workflows.md
./nine docs hitl                  # print hitl.md

./nine spec                       # list every specification topic
./nine spec event-journal         # print spec/contracts/event-journal.md
```

Output is styled Markdown on a terminal and raw Markdown when piped or redirected,
so `./nine spec wire-protocol | less` and `./nine docs usage > usage.txt` both work.

---

## Use cases with examples

### 1. File operations

```bash
./nine "Read /etc/hosts and tell me how many entries are in the 127.0.0.1 block"
```

```bash
./nine "Write a Python script to /tmp/hello.py that prints 'Hello, World!' and run it"
```

```bash
./nine "Find all .log files under /var/log modified in the last 24 hours and list their sizes"
```

### 2. Shell commands

```bash
./nine "Run 'df -h' and tell me which partition is most full"
```

```bash
./nine "What processes are listening on port 8080?"
```

```bash
./nine "Run 'git log --oneline -20' in /path/to/repo and summarize what changed"
```

### 3. Web research

Out of the box, web research goes through `web_search` and `web_page_read` — plain
HTTP, no JavaScript.

```bash
./nine "Search the web for the latest release of Go and tell me what changed"
```

```bash
./nine "Fetch https://example.com and extract all links from the page"
```

```bash
./nine "Search for 'best practices for SQLite indexing' and give me a summary"
```

`web_search` uses DuckDuckGo by default; set `SEARCH_PROVIDER=brave|serpapi` with
`SEARCH_API_KEY` for another backend.

For pages that need JavaScript, a login, or interaction, add a real browser by declaring
Playwright's MCP server in `nine.toml` — see [Browser Automation](browser.md). Nine ships
no browser of its own.

### 4. Memory and state

```bash
./nine "Remember that my production database host is db.prod.example.com"
./nine "What's my production database host?"
```

```bash
./nine "Store the following JSON as 'project_config': {\"env\": \"staging\", \"replicas\": 3}"
./nine "What's the replica count in my project config?"
```

```bash
./nine "List everything you have in memory"
```

### 5. Background goals

Ask Nine to work on something open-ended in the background — Nine records a goal, spawns a background pursue session for it, and returns immediately:

```bash
./nine "Set a goal to watch /var/log/syslog for lines containing ERROR and notify me every 5 minutes with a count"
```

Check in later:

```bash
./nine goals
./nine "Any notifications for me?"
```

Background agents continue running even when you're not connected. The next time you send a message, any pending notifications are prepended to the response.

### 6. Multi-Step workflows

Nine handles complex, multi-step tasks without needing step-by-step prompting:

```bash
./nine "Download the latest release tarball for jq from GitHub, extract it to /tmp/jq-test, and verify the binary works by running 'jq --version'"
```

```bash
./nine "Read all .go files in ./internal/agent, identify any TODO comments, and write a summary to /tmp/todos.md"
```

```bash
./nine "Set up a cron-style task that runs 'uptime' every minute and stores the result in memory under 'uptime_log'. Keep the last 10 entries."
```

### 7. Self-Improvement

Nine improves itself by writing skills. It does not generate plugins or change its own
configuration at runtime — see [Self-Improvement & Boundaries](self-modification.md).

**Add a skill:**
```bash
./nine "Create a skill for writing idiomatic Go error handling"
```

**Refine an existing skill:**
```bash
./nine "Update the go-development skill with notes on table-driven tests"
```

---

## Output and logging

Nine writes the agent's final response to stdout. All internal logs go to stderr.

```bash
# Quiet: only the response
./nine "Hello"

# Verbose daemon logs
NINE_LOG_LEVEL=debug ./nine daemon
```

For JSON-structured logs (e.g., for ingestion into a log aggregator):

```bash
NINE_LOG_FORMAT=json NINE_LOG_LEVEL=info ./nine daemon
```

---

## Tips

- **Longer context**: If the agent seems to "forget" earlier parts of a conversation, increase `context_budget` in `nine.toml`.
- **Parallel work**: Nine can run multiple background sessions and sub-agents concurrently. The LLM queue prioritizes active (user-facing) conversations over background ones.
- **Stuck agent**: If an agent appears to loop without making progress, the supervisor detects the stall and intervenes automatically. You can also ask: `./nine "Are any agents stalled?"`.
- **Reset**: To start a completely fresh conversation (no history), restart the daemon: `pkill nine && ./nine "Hello"`.

---

## Limits

| Limit | Detail |
|-------|--------|
| Local clients only | The CLI and TUI reach the daemon over a Unix socket on the same host. |
| No conversation reset command | Starting genuinely fresh means restarting the daemon. `/new` starts a new conversation but leaves the daemon's other state in place. |
| TUI views are read-only | Slash commands surface goals, workflows, tools, skills, memory and the context breakdown, but do not let you edit them. |
| The journal has no TUI view | `nine trace` and `nine replay` are CLI-only; nothing surfaces the journal inside the TUI. The context breakdown is the exception — `/context [id]` is the same view as `nine context`. |
| Config changes need a restart | Editing `nine.toml` takes effect on daemon restart. |
