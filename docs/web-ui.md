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
| Dashboard | the daemon: status, version, uptime, active sessions, plugin and tool counts |
| Conversations, Processes, Goals & workflows, Tools, Inbox, Journal, System | not built yet (see Limits) |

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
| Most sections are not built | Only the dashboard's daemon status works. The rest arrive in order: conversations; processes, goals and workflows; tools, plugins, skills and the inbox; then the journal and the system section (`adr/web-ui.md` §9). |
| One identity | The API token is the only credential, so every signed-in browser has the same rights. |
| Sessions are in memory | Restarting the API server signs every browser out. |
| Some operations stay in the CLI | `nine restore`, `nine daemon`, `nine api serve`, and the `validate` commands, which check a path on the operator's machine. |
