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

## OpenAPI / Swagger spec

The API spec is generated at build time from swag annotations on the handler functions in `internal/api/handlers.go`. The generated files live in `internal/api/docs/` and are committed to the repo:

| File | Content |
|------|---------|
| `internal/api/docs/swagger.json` | OpenAPI 2.0 JSON spec |
| `internal/api/docs/swagger.yaml` | OpenAPI 2.0 YAML spec |
| `internal/api/docs/docs.go` | Generated Go code that registers the spec at runtime |

### Regenerating the spec

After adding or changing handler annotations:

```bash
swag init -d internal/api -o internal/api/docs -g docs.go --parseDependency --parseInternal
```

The annotations are Go comments directly above each handler function — `@Summary`, `@Description`, `@Tags`, `@Param`, `@Success`, `@Failure`, `@Router`. Package-level metadata (title, version, base path, security scheme) is in `internal/api/docs.go`.

### Accessing the spec at runtime

| URL | What |
|-----|------|
| `http://localhost:8080/api/v1/swagger/` | Swagger UI — interactive browser for all endpoints |
| `http://localhost:8080/api/v1/swagger/doc.json` | Raw OpenAPI JSON |

The Swagger UI lets you browse all 29 endpoints, see their request/response schemas, and execute requests directly from the browser.

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

Standard error codes: `invalid_request` (400), `unauthorized` (401), `forbidden` (403), `not_found` (404), `conflict` (409), `timeout` (408), `too_many_requests` (429), `server_error` (500), `service_unavailable` (503).

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
