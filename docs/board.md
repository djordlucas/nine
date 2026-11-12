# The board

The board is Nine's shared work queue: one **orchestrator** session plans and posts **tasks**, and **worker** sessions claim them, do the tool calls under their role, and write results back. It lives in the store (`board_tasks` table), so it survives restarts and every session can read it. Design record: [adr/orchestrator-board.md](../adr/orchestrator-board.md).

## The pieces

- **Task** — one row: `id` (`task_<hex>`), `title`, `spec`, `posted_by`, `claimed_by`, `status` (`queued` → `running` → `done` / `failed` / `cancelled`), `result`, `source` (`post`, `run_agent`, `detached`), `depth`, timestamps. Every transition is a `task_*` event in the journal, so the event stream, `nine trace` and the web UI see the board live.
- **Orchestrator** — the `orchestrator` role ([roles.md](roles.md)): board tools, delegation, role/tool search, memory — no shell, no file tools. New conversations default to it (the web UI picker, `POST /api/v1/conversations` `role`, `nine --role <name> <message>`).
- **Workers** — sub-agent sessions (`run_agent`/`run_agents`) spawned per task, running under a leaf role. `run_agent` records its run on the board; `run_agent wait:false` returns the task id immediately and the result arrives as a restricted turn when the task completes. For background work, a `boardworker` process claims the oldest queued task on its clock (a `[[process]]` block with `tool = "boardworker"` and `every = "2s"`) and finishes it the same way.

## Model tools

| Tool | Who | Effect |
|---|---|---|
| `board_post({title, spec})` | any role granted it (orchestrator by default) | post a task; refused with a reason at the queue cap, on a duplicate, at max depth |
| `board_list({status?})` / `board_get({id})` | any session | read the board |
| `board_claim({id})` | a worker | atomic claim: `queued` → `running` |
| `board_complete({id, result})` / `board_fail({id, reason})` | the claimant | terminal state, result on the board |
| `board_cancel({id})` | the poster | `cancelled` |
| `role_list` / `role_search` | roles that grant them | discover worker roles before dispatching |
| `agent_stop({id})` | orchestrator | force-stop a misbehaving worker's session |

Task content is **data, not instructions**: a delivered result runs as a restricted turn (new-files-only writes) framed with the report label, the same protection piped process reports get ([processes.md](processes.md)).

## Operator surface

Full CRUD on tasks, mirroring processes:

```
nine board list | show <id> | create <title> [spec] | edit <id> [--title T] [--spec S] [--status S] [--result R] [--clear-claim] | delete <id>
```

REST under the same auth as everything else: `GET/POST /api/v1/board`, `GET/PATCH/DELETE /api/v1/board/{id}` ([api.md](api.md)). The web UI has a **Board** section (rail → More): list with a status filter, create form, per-task edit and delete, live on `task_*` events.

## Limits

- One board per store; results are text. Queued tasks are claimed highest-`priority` first (0–10, set on `board_post` or operator edit), FIFO within a priority.
- Delivery to a mid-turn poster is retried at its next `turn_end`; a gone poster gets the human feed. The board is the durable record.
- `[board] max_queued` (default 100) caps one poster's open tasks ([configuration.md](configuration.md)).
