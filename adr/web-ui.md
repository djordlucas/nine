# Design note — The embedded web UI

**Status:** Phases 1–3 implemented (2026-10-14); phases 4–6 proposed · **Related:** `docs/api.md`, `docs/hitl.md`,
`docs/processes.md`, `adr/agent-boundary.md`

Nine gets a web UI that is part of the binary: a Preact app, built once and embedded,
served by `nine api serve` at `/`, on by default. It works on phones and desktops and
covers everything the running daemon does. It reaches the daemon only through `/api/v1`
and `/ws/v1`, so the UI can do nothing the API cannot. The features only the CLI has
today get API endpoints first. Home is a dashboard that stays live through a new
daemon-wide event stream.

| Decision | Choice | Rejected |
|---|---|---|
| Delivery | Static app embedded with `go:embed`, served by the API server | Server-rendered Go with htmx; a separate web service |
| Stack | Preact, TypeScript, Vite; types generated from `internal/api/openapi.yaml` | Svelte; vanilla TypeScript; React |
| Bundle | Committed; CI rebuilds it in a container and fails on a difference | Built only by `make` and the Dockerfile |
| Authentication | Token login sets an HttpOnly session cookie; CSRF header on writes | Token kept in `localStorage` |
| Scope | Everything the running daemon does; missing endpoints added to the API | Restore by upload; today's API only |
| Layout | Dashboard-first; rail on desktop, bottom tabs on mobile | Chat-first |
| Live data | New `GET /api/v1/events`: the journal over SSE, resumable | Polling |
| Styling | Own CSS on design tokens; light and dark | Tailwind; a component library |
| Gate | Vitest, Playwright end to end at phone and desktop sizes, and an API coverage check | Unit tests only |
| Serving | On at `/`; `[api] ui = false` turns it off | Off by default |

---

## 1. Problem

Nine has two operator surfaces, the CLI and the TUI. Both need a terminal on the machine
running the daemon, or on one that can reach its socket. A deployment in Docker on another
machine is reachable only over HTTP, and the HTTP API has no client but `curl`.

The CLI also does things the API cannot, so an HTTP client is not a full operator:

| CLI | API today |
|---|---|
| `nine reflections [agent]` | none |
| `nine sessions`, `nine session show\|delete` (every session, process sessions included) | conversations only |
| `nine backup <path>` | none |
| `nine tools deps` | `deps` is in each tool's detail; there is no daemon-wide list |
| a journal to read | per-conversation streams only; nothing daemon-wide |

---

## 2. Delivery

The app's source is `web/`. Its build output is `internal/webui/dist/`, embedded by
`internal/webui` with `//go:embed dist` (embed cannot reach outside its package
directory), and committed.

The bundle is committed for the reason `vendor/` and the generated API code are: `go build`
works anywhere, with no Node. `make webui` builds it in a Node container. CI runs the same
build and fails if the result differs from what is committed, the way `make openapi-check`
fails on generated-code drift. Vite's output is deterministic for the same lockfile and
source, which is what makes a byte comparison usable.

`internal/api` serves it:

| Path | Serves |
|---|---|
| `/api/v1/…`, `/ws/v1/…` | the API, unchanged |
| `/assets/…` | hashed files, `Cache-Control: immutable` |
| any other `GET` | `index.html`, `no-cache`, so client-side routes survive a reload |

`[api] ui` (default `true`, `NINE_API_UI`) turns serving off; the API is unaffected.
Every UI response carries:
- a Content-Security-Policy with `default-src 'self'`, no inline script, no third-party
  origin and `frame-ancestors 'none'`;
- `X-Content-Type-Options: nosniff`;
- `Referrer-Policy: no-referrer`.

The UI is exactly as open as the API. With no `auth_token` it is open, and the startup
warning for a non-loopback address with no token now names the UI too.

---

## 3. Authentication

A browser cannot set an `Authorization` header on a WebSocket, so the bearer token alone
does not carry the chat. The token is exchanged for a cookie instead:

| Step | What happens |
|---|---|
| Login | `POST /api/v1/session` with `{token}`, compared in constant time. On success the daemon sets `nine_session`: HttpOnly, `SameSite=Strict`, `Path=/`, `Secure` when the request is HTTPS or arrives through a trusted proxy as HTTPS. |
| Requests | The auth middleware accepts the cookie wherever it accepts the bearer header. |
| Writes | A cookie-authenticated `POST`, `DELETE` or WebSocket upgrade must carry `X-Nine-CSRF` matching the token the login response returned, and its `Origin` must be the daemon's own. `cors_origins` still governs bearer-authenticated callers. |
| Logout | `DELETE /api/v1/session` clears the cookie and forgets the session. |
| Lifetime | Sessions live in memory, expire after 7 days idle, and end when the daemon restarts or the token changes. |

The cookie holds a random session id, not the token, so a leaked cookie dies with the
session. When no token is configured, `POST /api/v1/session` succeeds with no body, and
the UI skips the login page.

---

## 4. Scope and the endpoints added

"Every feature" means everything the running daemon does. Each CLI-only feature in §1
gets an endpoint, and `openapi.yaml` gains it first:

| Endpoint | For |
|---|---|
| `POST`, `DELETE /api/v1/session` | §3 |
| `GET /api/v1/events` | §6 |
| `GET /api/v1/reflections[?agent=]` | `nine reflections` |
| `GET /api/v1/sessions`, `GET`, `DELETE /api/v1/sessions/{id}` | `nine sessions`, `session show\|delete` |
| `GET /api/v1/backup?with=workspace` | `nine backup`, streamed as a download: `.db`, or `.tar.gz` with the workspace |

Out of scope, with the UI naming the CLI command where it would be:

| Left to the CLI | Why |
|---|---|
| `nine restore` | It replaces the database the daemon serving the UI has open. The daemon must be stopped first. |
| `nine daemon`, `nine api serve` | They start the process that serves the UI. |
| `nine skills\|plugin\|tool validate <path>` | They check a path on the operator's machine, before it is installed. |

---

## 5. Layout

Home is a **dashboard**. It shows:
- daemon status and the model;
- processes running against `max_running`, with any `paused` or `failing` first;
- pending approvals and unanswered `ask_human` questions;
- recent notifications;
- active goals and workflows;
- recent conversations.

Each tile links to its section.

| Section | Covers |
|---|---|
| Dashboard | the above |
| Conversations | list, create, delete; chat over the WebSocket, with streamed tool calls, queued turns, stop, `ask_human` answers and `force_think`; history, context, trace and replay per turn |
| Processes | the roster, one process with its session, budget and health; start, stop, send |
| Goals and workflows | list, create and delete goals; list workflows, stop or fail one |
| Tools | the roster, provenance and deps; show, call, versions, restore, delete, reload |
| Inbox | notifications; capability requests (approve, deny, revoke) |
| Journal | the live event stream, filtered by type, agent and session |
| System | status, plugins (reload), skills, sessions, reflections, docs and spec topics, backup, and the CLI-only operations from §4 |

Desktop has a left rail and, in Conversations, a list beside the chat. Under 768px wide,
a bottom tab bar holds Dashboard, Conversations, Processes and Inbox, plus a "More" sheet
for the rest. Lists become full-screen views that push detail views. Pending approvals
badge the Inbox tab in both layouts.

---

## 6. The event stream

`GET /api/v1/events` serves the journal over SSE:
- **Filters:** `type`, `agent` and `session`, each repeatable.
- **Resume:** each event's `id` is its journal sequence, so `Last-Event-ID` (or
  `?after=`) replays what a dropped connection missed, up to a retention window. Past
  it, the server sends `reset` and the client reloads its lists.
- **Content:** the stream carries what the journal stores, so it is as sensitive as
  `nine trace`, and the same authentication guards it.

The dashboard, Inbox and Processes load lists with the existing `GET`s and update from
the stream. The Journal section renders the stream itself. One connection per tab serves
them all.

---

## 7. Front end

| Concern | Choice |
|---|---|
| Types | `openapi-typescript` generates them from `openapi.yaml`; a drift check like the Go one |
| Client | a typed `fetch` wrapper that sends `X-Nine-CSRF` and handles `401` by returning to login |
| State | Preact signals; no global store library |
| Routing | `preact-iso`, path-based, so a link to a conversation or a process can be shared |
| Markdown | `marked` with HTML disabled; no `innerHTML` from model output |
| Long lists | windowed rendering for the journal and long histories |
| Styling | CSS modules on tokens in `web/src/tokens.css`. Light and dark from `prefers-color-scheme`, with a stored override. Touch targets 44px or larger; layouts tested at 360px. |
| Budget | under 150 KB gzipped for the whole app |

---

## 8. Testing

| Layer | Tool | Checks |
|---|---|---|
| Go | `go test` | serving, caching and security headers; cookie login, logout, expiry, CSRF and `Origin`; each new endpoint |
| Components and client | Vitest | rendering, the API client, stream reconnect and resume |
| End to end | Playwright in a container | the embedded UI against a real daemon with a scripted model server, at 390×844 and 1440×900. Covers login, each section, a chat turn with a tool call, an `ask_human` answer, an approval, and a process start and stop. |
| Coverage | a script in CI | every `openapi.yaml` operation is used by the UI or listed as deliberately unused with a reason |

No model runs in CI. The daemon's provider points at a stub OpenAI-compatible server in
the Playwright container, which answers each request from a script. Nothing in the tree
does this for a real daemon yet, so phase 1 builds the stub.

---

## 9. Phases

| Phase | Delivers |
|---|---|
| 1 | `internal/webui`, serving and headers, `[api] ui`; cookie sessions and CSRF; `web/` scaffold with generated types, tokens, the app shell (rail and tabs) and login; `make webui`, drift checks, the Playwright harness |
| 2 | `GET /api/v1/events`; the dashboard; the Journal section |
| 3 | Conversations: list, chat over the WebSocket, `ask_human`, stop, history, context, trace and replay |
| 4 | Processes, goals and workflows |
| 5 | Tools, plugins, skills and the Inbox with capability decisions |
| 6 | Reflections, sessions and backup endpoints; the System section; the coverage check required; `docs/web-ui.md` |

Each phase ships with its docs, contract changes and tests, and the API changes land in
`openapi.yaml` in the same PR.

---

## Phase 1 as built

| Planned | Built | Why |
|---|---|---|
| `POST`, `DELETE /api/v1/session` | `GET`, `POST`, `DELETE /api/v1/auth/session` | `/sessions` is the session roster (§4); one letter apart was a trap. `GET` tells a page whether to ask for the token, and hands a reloaded page its CSRF token. |
| `X-Nine-CSRF` on a cookie WebSocket upgrade | Same-origin `Origin` only | A browser cannot set headers on a WebSocket handshake, and always sends `Origin` on one. |
| Stub model server in phase 1 | Deferred to phase 3 | Nothing in phase 1 sends a turn; the first chat test needs it. |
| `make webui` and friends | Plus `.dockerignore` for `web/node_modules`, and the `cookieAuth` scheme on every operation in `openapi.yaml` | The build context was copying the Node toolchain into the image build. |

The UI's files are served before the rate limiter: with the default 10-request
burst, a page load and its first API calls would otherwise spend the budget.
The bundle is 15 KB gzipped. Two fresh builds in the pinned container produce
identical bytes, which the drift check relies on. The Playwright tests run
against the runtime image a release publishes, not a dev build.

## Phase 2 as built

| Planned | Built | Why |
|---|---|---|
| `GET /api/v1/events`: the journal | The journal now also records approvals, questions, notifications, goals and workflows (`human_input_*`, `capability_*`, `notification`, `goal_*`, `workflow_*`), written by the store beside each change | None of them reached the journal, so no stream of it could keep the dashboard's attention, notification and goal tiles live. The store is the one place every path that changes them passes. |
| Filters `type`, `agent`, `session` | `type` (exact or `prefix*`) and `agent` | A session's id is the journal's `agent_id`; two names for one filter. |
| Payloads as journaled | A payload over 16 KB is sent as `{"omitted": true, "bytes": N}` | An `llm_request` carries the whole context window. The trace has it whole, and the Journal links there. |
| A retention window | Reset when the position was scrubbed, never assigned, or more than 10,000 events behind | The journal is bounded by turns, not time, so "window" is the events still kept; the backlog cap bounds a replay nobody would read. |
| — | `?tail=N` (up to 1000) | A tab opening with an empty Activity tile and Journal says nothing happened; the stream starts with the last 200. |
| — | `GET /api/v1/questions` and the daemon's `list_human_requests` | The dashboard lists unanswered `ask_human` questions (§5), and nothing listed them. Answering stays on the WebSocket (phase 3). |
| Dashboard: processes against `max_running` | Processes by state, failing and paused first | The API does not expose `max_running`; it joins with the Processes section (phase 4). |
| Windowed rendering of the journal | 200 rows at a time, "Show more", 2000 kept | Enough for the volume a tab sees; real windowing can come with long histories in phase 3. |

The daemon's side is `journal_follow`, a socket message like `watch`, which reads
the store forward from a `seq` and is woken by a hook on every journal write. The
new event types are not process triggers: adding them to `config.EventTypes` is a
behavior change for `on` that wants its own evals. The bundle is 20 KB gzipped.

## Phase 3 as built

| Planned | Built | Why |
|---|---|---|
| Conversations: stop | Stop cancels the running turn, through a new daemon `turn_cancel`, `POST /conversations/{id}/cancel` and a WebSocket `cancel`. Ending the session (`POST …/stop`) is a separate, confirmed action. | The only stop the daemon had was `session_stop`, which deletes the checkpoint: there was no way to interrupt a runaway turn and keep the session. |
| A stub model server in the Playwright container | `web/e2e/stub-model.mjs`, its own `model` service in `e2e/compose.yaml`, speaking the streaming chat API the `mistral` provider calls | The daemon reaches it as it would a provider, by `NINE_LLM_ENDPOINT`, with no change to nine. It answers from the last message: an echo with Markdown, a tool call, an `ask_human` question, or a slow reply to cancel. |
| History, context, trace and replay per turn | The chat shows the transcript; "Inspect turn N" shows replay and the raw trace; Context is a panel | One place per question: the chat for what was said, the inspector for how. |
| `marked` with HTML disabled | `marked`'s lexer only; its tokens become Preact elements | No HTML string is built, so no `innerHTML`: raw HTML in a reply is text, links are http(s) or mailto, images are links. |
| — | The chat reloads the transcript when a turn ends, and keeps its live view until the transcript has the turn | The journal is written off the turn's path; a transcript read the moment a turn ends can lack its reply. |
| Windowed rendering of long histories | The last 1000 transcript entries | The transcript endpoint pages at 1000; a turn's full record is one click away. |

The bundle is 41 KB gzipped, `marked` included.

---

## Limits

| Limit | Detail |
|---|---|
| Restore stays in the CLI | §4. The System section shows the command. |
| One operator identity | The token is the only identity, so every browser session has the same rights. There are no per-user accounts or roles. |
| Sessions do not survive a restart | They are held in memory. After a daemon restart, every browser logs in again. |
| Journal resume is bounded | A client away longer than the retention window reloads instead of replaying. |
| Committed bundle | A PR that changes `web/` carries a built-asset diff. |
