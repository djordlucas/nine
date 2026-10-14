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
| Conversations, Processes, Goals & workflows, Tools, Inbox, System | not built yet (see Limits) |

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
| `make webui-e2e` | Playwright against the runtime image, at 390×844 and 1440×900 |
| `npm run dev` in `web/` | a dev server that proxies `/api` and `/ws` to a daemon on `:8080` |

## Limits

| Limit | Detail |
|---|---|
| Most sections are not built | The dashboard and the Journal work. The rest arrive in order: conversations; processes, goals and workflows; tools, plugins, skills and the inbox; then the system section (`adr/web-ui.md` §9). Until then a tile's Open link leads to a placeholder. |
| Large payloads stay out of the stream | A payload over 16 KB, usually a model call's whole context, reaches the Journal as its size only. The turn's trace (`GET /api/v1/conversations/{id}/trace`) has it, and the Journal links to it. |
| The Journal holds what the tab has seen | It shows the events kept since the tab opened, up to 2000, starting with the 200 before. Older history is in each conversation's trace. |
| One identity | The API token is the only credential, so every signed-in browser has the same rights. |
| Sessions are in memory | Restarting the API server signs every browser out. |
| Some operations stay in the CLI | `nine restore`, `nine daemon`, `nine api serve`, and the `validate` commands, which check a path on the operator's machine. |
