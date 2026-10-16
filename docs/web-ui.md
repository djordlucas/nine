# Web UI

`nine api serve` serves a web UI at `/`, beside the API. It works on phones and
desktops, and reaches Nine only through the [HTTP API](api.md): the UI can do
nothing the API cannot. It is part of the binary and the container image, so
there is nothing else to install.

```bash
docker run -e NINE_API_AUTH_TOKEN=… -p 8080:8080 ghcr.io/djordlucas/nine
# then open http://localhost:8080
```

| | |
|---|---|
| Address | the API server's: `http://<host>:<port>/` |
| Sign in | the API token (`[api] auth_token`), once per browser |
| Turn off | `[api] ui = false`, or `NINE_API_UI=false` |
| Layout | a rail on desktop; a bottom tab bar under 768px, with More for the rest |
| Theme | follows the system; the theme control switches to light or dark |

## Sections

| Section | Shows |
|---|---|
| Dashboard | what needs you (capability requests and `ask_human` questions), processes by state, the latest activity, recent conversations, active goals and workflows, notifications, and the daemon's status. Every tile updates as things happen. |
| Journal | every event the daemon records, live: turns, model and tool calls, process transitions, and changes to approvals, questions, notifications, goals and workflows. Filter by kind or by session, pause, and open any event for its payload. |
| Conversations | the conversations, newest first, beside the open one; on a phone, the list, then the conversation in its place. Chat live, answer the session's questions, stop a turn, and inspect any turn. |
| Processes | every process, failing and paused first, against `[processes] max_running`; one process's trigger, session, goal, budget and recent activity. Start, stop, retry, and message a running live process. |
| Goals & workflows | goals as a tree beside the workflows. Create goals and sub-goals; pause, resume, finish, archive and delete them; stop or fail a workflow. |
| Tools | the tools by where they came from, each with its capabilities, dependencies and arguments schema, to call directly; a tool Nine wrote with its versions. Also the plugins, with reload, and the skills, each readable. |
| Inbox | capability requests to approve or deny, questions waiting for an answer, the notifications, and the capabilities in force. Its link counts what waits. |
| Data | the workspace's files and the database processes keep their records in, to read and edit. Nine's own state, the store and the workspace's `.nine/`, is not reachable. |
| System | the daemon's status, a backup to download, every session, reflections, and the docs and spec this build embeds. |

On a phone every list opens its item full screen, with ‹ to go back.

## Processes and goals

A process's state decides its controls.

| State | Controls |
|---|---|
| running | Stop (confirmed); Message it, for a live process |
| failing | Retry now, which clears its failures and runs it at once; Stop |
| stopped by the operator, the model or itself | Start |
| paused by its budget | Start, which also resets the day's usage |
| stopped by its goal | Resume goal, which sets the goal active; the process runs again with it |

A goal's status decides whether the processes bound to it run: Pause stops
them, Resume restarts the ones it stopped, Mark done and Archive leave them
stopped ([processes](processes.md#goals-sessions-and-pipes)). A goal that a
`[[process]]` block in `nine.toml` declares cannot be deleted here.

## Tools, plugins and skills

| Control | Does |
|---|---|
| Filter | narrows the roster by name, description or plugin |
| Reload | re-reads the sandboxed tool directories, or, on Plugins, the plugin directories |
| Call | runs the tool now with the JSON arguments given, which start from its schema's required properties. No model is involved. A sandboxed tool runs against throwaway state unless "Use its real state" is ticked. |
| Versions, Restore | a tool Nine wrote: its kept versions, and making one current again |
| Delete | a tool Nine wrote, with the processes it runs |

## Inbox

| Part | Shows |
|---|---|
| Capability requests | what a tool Nine writes asked to use, with its scope and reason. Approve (confirmed) or deny. |
| Questions | `ask_human` questions waiting; each links to the conversation that answers it |
| Notifications | newest first, the new ones marked. Opening the Inbox marks them seen. |
| Capabilities in force | each grant with its source; Revoke for one you approved |

## Data

| View | Does |
|---|---|
| Files | browses the workspace. A text file up to 2 MB opens in an editor (⌘S or Ctrl+S saves); any file downloads. New file takes a path, so `notes/today.md` makes `notes/`. Upload puts files in the directory shown. Delete removes a file, or a directory once it is empty. |
| Database | the tables and views with their row counts; a table's rows 100 at a time. Add row inserts one, a column left empty taking its default; Edit changes or deletes one. NULL stores a NULL. A view, or a table without a rowid, is changed from SQL. |
| SQL | runs one statement: a read shows up to 1000 rows, a write how many rows it changed. ⌘Enter or Ctrl+Enter runs it. |

| Rule | Detail |
|---|---|
| The trash keeps what you replace | Saving, uploading over a file, and deleting move the old file to the workspace trash, where the agent's `trash_list` and `restore_file` reach it until `[workspace]` retention sweeps it. |
| A file changed since you opened it is not overwritten | Save and Delete fail with "the file changed since it was read" if anything wrote it after the page read it. Copy your text, reopen the file, and apply it again. |
| The database's rules apply to you | A statement runs under the policy a process's program has: no `ATTACH`, transactions, temporary objects, or pragmas beyond table introspection. Each write is journaled as `sql_write` under `operator`, kinds and tables only. |
| Files open as downloads | A workspace file is served as bytes that a browser does not run, so an HTML file there cannot act on the UI's origin. |

## System

| View | Shows |
|---|---|
| Overview | the daemon's status; Download the store (`.db`), or the store with the workspace (`.tar.gz`), written while Nine runs; and the operations only the CLI performs |
| Sessions | every session the store holds, process sessions included, with its age, journal size, and whether retention keeps it. Delete erases one and everything recorded for it. |
| Reflections | what a reflecting session concluded, newest first; `self-reflection` by default |
| Docs, Spec | the documentation and specification this build embeds; a link to another topic opens it in place |

## Conversations

A conversation shows its transcript, then every turn it runs as it happens,
whoever started it: this browser, another client, or the session's own schedule.
Replies render as Markdown.

| Control | Does |
|---|---|
| New conversation | starts an interactive session, which may ask you questions |
| Send | sends a turn; Enter sends on a keyboard, Shift+Enter is a new line. A message sent mid-turn waits for the turn to end. |
| Think | asks for the model's reasoning on the next turn (`force_think`) |
| Stop | cancels the running turn; the session and its history stay |
| A question's options, or your own words | answers the session's `ask_human` question |
| Tool calls | open to show their input and output |
| Inspect turn N | the turn as the journal recorded it: its input, each model call and tool call with what went in and came out, its sub-agents, its result, and the raw events. Nothing runs again. |
| Context | what the next model call would be shown, by section, with the system prompt |
| End session | stops the session's worker and deletes its checkpoint, so its next turn starts without the earlier context; the transcript stays |
| Delete | removes the conversation and everything recorded for it |

The conversation follows its turns over a WebSocket
(`/ws/v1/conversations/{id}/messages`). It reconnects by itself and reloads the
transcript when it does.

## Staying live

Each browser tab holds one connection to `GET /api/v1/events`, the journal as a
stream ([HTTP API](api.md#journal-stream)), and every live view uses it. The
header says whether it is up: Live, Connecting, or Offline.

| Behavior | Detail |
|---|---|
| Opening | the stream starts with the 200 most recent events, so the Activity tile and the Journal are not empty on arrival |
| Updating | a dashboard tile loads again when an event it shows arrives; a burst causes one reload |
| Dropping | the browser reconnects and resumes after the last event it had, missing nothing still in the journal. A stream the server refused is retried every 2 to 30 seconds. |
| Missing events | when the journal can no longer resume a tab (scrubbed, or the tab was more than 10,000 events behind), the tiles reload and the Journal says it restarted |
| Keeping | the tab keeps the latest 2000 events. A reload starts again from the latest 200. |

## Signing in

With an `auth_token` configured, the UI asks for it once. The server answers
with a session cookie, and the token is not stored in the browser.

| Property | Value |
|---|---|
| Cookie | `nine_session`: HttpOnly, `SameSite=Strict`, `Secure` over HTTPS, including through a trusted proxy |
| Holds | a random session id, never the token |
| Lifetime | 7 days unused; ends at sign-out, and when the API server restarts |
| Writes | also need the `X-Nine-CSRF` header and this server's own `Origin`, which the UI sends; a cross-site page can send neither |

Without an `auth_token` the UI opens straight to the dashboard, and is as open as
the API is. Nine warns at startup when it listens on a non-loopback address with
no token; set one before exposing the port.

The API's endpoints for this are `GET`, `POST` and `DELETE /api/v1/auth/session`
([HTTP API](api.md#authentication)).

## Security headers

| Header | Value |
|---|---|
| `Content-Security-Policy` | `'self'` for every resource type; no inline script or style; `frame-ancestors 'none'` |
| `X-Frame-Options` | `DENY` |
| `X-Content-Type-Options` | `nosniff` |
| `Referrer-Policy` | `no-referrer` |

The UI loads nothing from another origin.

The app's files are served outside authentication and rate limiting: they hold
no data, and a page load fetches several of them. Every request the app makes
goes through the API, and authentication and rate limits apply there in full.

## Building it

The app's source is `web/` (Preact, TypeScript, Vite). Its build is committed
in `internal/webui/dist`, so `go build` needs no Node.

| Command | Does |
|---|---|
| `make webui` | regenerates the API types from `internal/api/openapi.yaml` and builds, in a pinned Node container |
| `make webui-check` | fails if the committed build or types differ from a fresh build |
| `make webui-test` | unit tests (Vitest) |
| `make webui-e2e` | Playwright against the runtime image, at 390×844 and 1440×900. The daemon's model is `web/e2e/stub-model.mjs`, which answers from a script, so no model runs. |
| `npm run dev` in `web/` | a dev server that proxies `/api` and `/ws` to a daemon on `:8080` |

## Limits

| Limit | Detail |
|---|---|
| Tools reload by hand | No journal event marks a tool written, restored or deleted, so the Tools views reload after your own actions, not when Nine changes a tool. |
| Two tool names are taken | A tool named `plugins` or `skills` cannot be opened from Tools: those paths are its other views. |
| A backup is written beside the API server | The daemon writes the snapshot to a temporary file the API server streams and then removes, so the two must share a filesystem, as their socket already requires. A snapshot left by an interrupted download is removed by the next backup after an hour. |
| A long transcript shows its last 1000 entries | A conversation loads at most 1000 transcript entries, the most recent. Each turn's full record is under Inspect. |
| A failed turn is not in the transcript | The transcript keeps replies, not errors, so a cancelled or failed turn shows its outcome only while the page that saw it stays open. The turn's trace (`GET /api/v1/conversations/{id}/trace?turn=N`) keeps it. |
| Large payloads stay out of the stream | A payload over 16 KB, usually a model call's whole context, reaches the Journal as its size only. The turn's trace (`GET /api/v1/conversations/{id}/trace`) has it, and the Journal links to it. |
| The Journal holds what the tab has seen | It shows the events kept since the tab opened, up to 2000, starting with the 200 before. Older history is in each conversation's trace. |
| One identity | The API token is the only credential, so every signed-in browser has the same rights. |
| Sessions are in memory | Restarting the API server signs every browser out. |
| Files up to 32 MB | A larger workspace file cannot be read or uploaded in Data; `nine backup` with the workspace takes it. |
| `spill/` is reserved | A workspace path under `spill/` cannot be written, as for the file tools: `read_file` reads `spill/…` from the store. |
| Some operations stay in the CLI | `nine restore`, `nine daemon`, `nine api serve`, and the `validate` commands, which check a path on the operator's machine. |
