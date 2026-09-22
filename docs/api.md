# HTTP API

Nine's HTTP API provides REST access to the daemon's functionality with feature parity to the CLI. It runs as a separate process inside the container — managed by s6-overlay alongside the daemon — and communicates with the daemon over the same Unix socket as the CLI.

The formal contract is in [`spec/contracts/api.md`](../spec/contracts/api.md). This doc covers how to use the API and where the OpenAPI spec lives.

---

## Running the API server

### In Docker (default)

The API server starts automatically alongside the daemon in both the runtime and hot-reload containers. Port 8080 is published to the host by `make up` and `make up-hot`:

```bash
make up        # runtime image, port 8080 published
make up-hot    # hot-reload image, port 8080 published
```

The API is then available at `http://localhost:8080`.

### Manually

```bash
nine api serve [--port 8080] [--host 0.0.0.0] [--auth-token secret]
```

The server connects to the daemon's Unix socket (`/tmp/nine.sock` by default). If the socket is not yet available, the API server waits for it to appear before accepting requests.

Other CLI commands:

```bash
nine api status   # check if the API server is running
nine api stop      # stop the API server
```

---

## OpenAPI document

`internal/api/openapi.yaml` is an OpenAPI 3.1 document and the source of truth
for this API. `internal/api/apigen/` — the request/response models and the
server interface — is generated from it, so a handler that disagrees with the
document fails to compile rather than silently serving a different shape.

This inverts the previous arrangement, where the document was generated from
swag annotations on the handlers and could only ever describe whatever the code
already did.

### Regenerating

```bash
make openapi        # regenerate internal/api/apigen from openapi.yaml
make openapi-check  # fail if the committed output is stale (runs in CI)
make openapi-lint   # lint the document, failing on warnings (runs in CI)
```

`internal/api/.vacuum.yaml` holds the lint ruleset, including the rules this API
contradicts on purpose — snake_case properties, operations that return only
`501`, and the fields that are deliberately arbitrary JSON.

The generator lives in `tools/`, its own Go module, so its dependencies stay
out of nine's graph and out of `vendor/`.

### Request validation

Incoming requests are validated against the document before a handler runs
(`internal/api/validate.go`): required body fields, declared types, and the page
bounds `limit` and `offset` carry. A failure is a `400` in the standard error
shape.

Two things stay outside it:

- **Authentication.** nine's bearer token is optional and the document declares
  operations secured unconditionally, so auth is left to the middleware, which
  knows whether a token is configured.
- **The document routes.** `/api/v1/openapi*` are not operations the document
  declares, so validation is scoped to the generated routes.

If the embedded document cannot be loaded, validation is skipped and logged
rather than refusing to start — it is parsed by the generator at build time, so
that cannot happen in a built binary, and the handlers' own checks still run.

### Reading it at runtime

| URL | What |
|-----|------|
| `http://localhost:8080/api/v1/openapi.yaml` | The document, as committed |
| `http://localhost:8080/api/v1/openapi.json` | The same document as JSON |
| `http://localhost:8080/api/v1/openapi/` | Browsable reference (Scalar) |

The document is embedded in the binary, so the two file routes need no network.
The browser page loads its renderer from a CDN and does.

The path is `/api/v1/openapi` rather than `/api/v1/docs`, which is already the
documentation-topic endpoint.

---

## Authentication

Authentication is optional. When `auth_token` is configured (via `--auth-token` flag, `NINE_API_AUTH_TOKEN` env var, or `auth_token` in `nine.toml`'s `[api]` section), all requests must include:

```
Authorization: Bearer <token>
```

Without a configured token, the API is open. This is the default in the dev container.

---

## Base URL and versioning

All endpoints are under `/api/v1`. Content type is `application/json` for both requests and responses.

---

## Key endpoints

### Health and status

```
GET /api/v1/health    — API server health (daemon connectivity, uptime, version)
GET /api/v1/status    — Daemon status (sessions, plugins, tools, memory)
```

### Conversations

```
POST   /api/v1/conversations                    — Create a conversation
GET    /api/v1/conversations                     — List all conversations
GET    /api/v1/conversations/{id}                — Get conversation details
POST   /api/v1/conversations/{id}/messages       — Send a message (execute a turn)
GET    /api/v1/conversations/{id}/context         — Context breakdown
GET    /api/v1/conversations/{id}/history        — Message history
GET    /api/v1/conversations/{id}/trace          — Turn trace (LLM calls + tool I/O)
POST   /api/v1/conversations/{id}/replay         — Replay a turn
DELETE /api/v1/conversations/{id}                — Delete a conversation
POST   /api/v1/conversations/{id}/stop           — Stop a conversation
GET    /api/v1/conversations/{id}/messages/stream — SSE streaming
```

### Goals, workflows, tools, plugins

```
GET    /api/v1/goals              POST /api/v1/goals
GET    /api/v1/goals/{id}         DELETE /api/v1/goals/{id}

GET    /api/v1/workflows          POST /api/v1/workflows/{id}/stop  POST /api/v1/workflows/{id}/fail

GET    /api/v1/tools              GET /api/v1/tools/{name}
POST   /api/v1/tools/{name}/call  POST /api/v1/tools/reload

GET    /api/v1/plugins            POST /api/v1/plugins/reload
```

### Other

```
GET /api/v1/notifications   GET /api/v1/skills   POST /api/v1/sessions/attach
GET /api/v1/docs             GET /api/v1/docs/{topic}
GET /api/v1/spec             GET /api/v1/spec/{topic}
```

---

## Example: a simple conversation

```bash
# 1. Create a conversation
curl -X POST http://localhost:8080/api/v1/conversations \
  -H "Content-Type: application/json" \
  -d '{"interactive": false}'

# Response: {"id":"abc123-...", "role":"orchestrator", ...}

# 2. Send a message
curl -X POST http://localhost:8080/api/v1/conversations/abc123-.../messages \
  -H "Content-Type: application/json" \
  -d '{"text": "Hello! What are you?"}'

# Response: {"agent_id":"abc123-...", "text":"I'm Nine, ...", "completed_at":"..."}
```

The conversation ID from step 1 is used in all subsequent message, history, trace, and context requests.

---

## Configuration

API configuration in `nine.toml`:

```toml
[api]
enabled = true
port = 8080
host = "localhost"
auth_token = ""
timeout_seconds = 30
max_connections = 100
cors_origins = ["*"]
trusted_proxies = []        # proxies whose X-Forwarded-For the API believes
```

Environment variable overrides: `NINE_API_PORT`, `NINE_API_HOST`, `NINE_API_AUTH_TOKEN`, `NINE_API_TRUSTED_PROXIES`, etc.

### Running behind a reverse proxy

`trusted_proxies` is empty by default, so the API keys rate limiting on the
transport peer address and ignores `X-Forwarded-For` and `X-Real-IP`. Both
headers are set by whoever sends the request: believing them unconditionally
lets a client vary the header to get a fresh rate-limit bucket per request.

With a proxy in front, list it as a bare IP or CIDR block so client addresses
survive the hop:

```toml
[api]
trusted_proxies = ["10.0.0.0/8", "192.168.1.7"]
```

```bash
nine api serve --trusted-proxies 10.0.0.0/8,192.168.1.7
```

The API then reads the forwarding chain right to left and keys on the rightmost
address that is not itself a trusted proxy — the last hop a client could not
have forged. List every proxy in the chain; a hop left out is treated as the
client.

---

## Endpoints that return 501

Six endpoints are declared but not backed by the daemon. Each returns `501
not_implemented` with a `details.detail` naming what is missing:

| Endpoint | Missing |
|----------|---------|
| `GET /conversations/{id}/history` | no journal query on the wire protocol |
| `GET /conversations/{id}/trace` | no per-turn trace on the wire protocol |
| `POST /conversations/{id}/replay` | no replay message on the wire protocol |
| `POST /goals` | `goal_create` is role-gated; an HTTP caller has no role |
| `DELETE /goals/{id}` | no goal deletion exists to call |
| `GET /skills` | no skills query on the wire protocol |

They previously returned `200` with invented data — an empty list, an echo of
the request, or a fabricated id for a goal that was never created. Use the CLI
for these until the wire protocol carries them: `nine trace`, `nine replay` and
`nine skills` read the memory store directly, which the API process must not do
(spec API-A-1).

---

## Pagination

List endpoints take `limit` and `offset` in the query string and return a
`pagination` block alongside `data`:

```bash
curl 'http://localhost:8080/api/v1/conversations?limit=10&offset=20'
```

```json
{
  "data": [ ... ],
  "pagination": { "limit": 10, "offset": 20, "total": 57, "has_more": true }
}
```

| Parameter | Default | Constraint |
|-----------|---------|------------|
| `limit` | 50 | 1–1000 |
| `offset` | 0 | ≥ 0 |

A malformed or out-of-range value is a `400 invalid_request`, not a silent
fallback to the default. An `offset` past the end returns an empty page.

Paging is offset based; there is no cursor. The daemon returns a full result
set per call, so a cursor would only re-encode the offset.

---

## Query parameters, not request bodies

Four operations take arguments in the query string. They previously took a JSON
body on `GET`/`DELETE`, which OpenAPI 3.x leaves undefined — generated clients
drop it and Swagger UI will not send it.

| Operation | Parameters |
|-----------|------------|
| `GET /conversations/{id}/context` | `verbose` |
| `GET /conversations/{id}/trace` | `turn`, `sub_agents` |
| `GET /notifications` | `all`, `limit`, `offset` |
| `DELETE /conversations/{id}` | `force` |

```bash
curl 'http://localhost:8080/api/v1/conversations/abc/trace?turn=3&sub_agents=true'
curl -X DELETE 'http://localhost:8080/api/v1/conversations/abc?force=true'
```

A body sent to these operations is ignored.

---

## Error format

All errors follow a consistent shape:

```json
{
  "error": {
    "code": "not_found",
    "message": "conversation not found",
    "details": { "id": "abc123" }
  }
}
```

Standard error codes: `invalid_request` (400), `unauthorized` (401), `forbidden` (403), `not_found` (404), `conflict` (409), `timeout` (408), `too_many_requests` (429), `server_error` (500), `not_implemented` (501), `service_unavailable` (503).

---

## Hot-reload behavior

In the dev container, the API server is an s6 longrun service. When the hot-reload loop detects a `.go` change, it rebuilds the binary, restarts the daemon, and restarts the API server so it picks up the new binary and reconnects to the fresh daemon socket. The API server polls for `/tmp/nine.sock` on startup, so it waits for the daemon to be ready before accepting requests.

## Limits

| Limit | Detail |
|-------|--------|
| Local transport underneath | The API server is a translation layer over the daemon's Unix socket, so it runs on the same host as the daemon. |
| No WebSocket support | Streaming a turn's progress events over HTTP is not implemented. A conversation turn is request/response. |
| Spec is generated, not hand-checked | The OpenAPI document is regenerated from annotations. An endpoint whose annotation drifts from its handler produces a spec that is wrong in the same way. |
| Startup races the daemon | The API server polls for the daemon socket on startup and refuses requests until the daemon is ready. |
