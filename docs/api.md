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
contradicts on purpose — snake_case properties, action paths such as
`/plugins/reload`, and the fields that are deliberately arbitrary JSON.

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

Without a configured token, the API is open — and that is the default in **both**
published images, which run `nine api serve --host 0.0.0.0`. The API can start
conversations, and a conversation can run shell commands, so an unauthenticated
listener on a routable address is a remote shell.

Nine logs a warning at startup when it binds a non-loopback address with no
token. Either set a token, or publish the port to loopback only:

```bash
docker run -e NINE_API_AUTH_TOKEN=… -p 8080:8080 …   # authenticated
docker run -p 127.0.0.1:8080:8080 …                  # local only
```

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
GET    /api/v1/conversations/{id}/history        — Transcript, every turn
GET    /api/v1/conversations/{id}/trace          — Event journal (nine trace)
POST   /api/v1/conversations/{id}/replay         — One turn reconstructed (nine replay)
DELETE /api/v1/conversations/{id}                — Delete a conversation
POST   /api/v1/conversations/{id}/stop           — Stop a conversation
GET    /api/v1/conversations/{id}/messages/stream — Live events (SSE)
WS     /ws/v1/conversations/{id}/messages          — Live events + send turns (WebSocket)
```

History, trace and replay read the session's event journal through the daemon,
so they work for a stopped session and for one revived after a restart, and
reading them does not attach to the session.

| Endpoint | Returns |
|----------|---------|
| `GET …/history` | The transcript, oldest first: `user_turn`, `tool_start`, `tool_end`, `sub_agent_start`, `sub_agent_end` and `response` entries, each with its `turn_number`. Paged with `limit`/`offset`. |
| `GET …/trace` | Every journal event — `seq`, `turn`, `type`, span ids, timestamp and the payload exactly as journaled. `turn=N` limits it to one turn; `sub_agents=true` adds a `sub_agents` list holding each spawned sub-agent's own trace, recursively, linked to the `seq` of the event that spawned it. |
| `POST …/replay` | Turn `{"turn": N}` reconstructed: trigger and input, each LLM call (request size, token estimate and provider counts, stop reason, requested tools), each tool call with its input, output, timing and outbound HTTP, the sub-agents, and the result. Nothing is re-executed. |

An unknown conversation is `404`; so is a replay of a turn the journal does not
hold.

### Goals, workflows, tools, plugins

```
GET    /api/v1/goals              POST /api/v1/goals
GET    /api/v1/goals/{id}         DELETE /api/v1/goals/{id}

GET    /api/v1/workflows          POST /api/v1/workflows/{id}/stop  POST /api/v1/workflows/{id}/fail

GET    /api/v1/tools              GET /api/v1/tools/{name}       DELETE /api/v1/tools/{name}
POST   /api/v1/tools/{name}/call  POST /api/v1/tools/reload

GET    /api/v1/plugins            POST /api/v1/plugins/reload

GET    /api/v1/capabilities       POST /api/v1/capabilities/{id}/decision
```

`POST /goals` takes `{"description": "…"}` and creates a top-level goal with its
own pursue session ([goal sessions](goal-sessions.md)); the response's
`pursue_session` is `spawned`, or `limit_reached` when the daemon is at its
goal-session cap and the goal is recorded unattended. Adding `"parent_id"`
creates a sub-goal instead, which gets no session (`none`); an unknown parent is
`404`. The API caller creates the goal as the operator, so the role gate on the
agent's `goal_create` tool does not apply — it decides which agents may start
background work, and an authenticated API caller is not an agent.

`DELETE /goals/{id}` deletes the goal and every sub-goal beneath it, and stops the
goal's pursue session; the session's transcript and journal are kept. The
response lists the deleted ids. A goal declared by an `[[agent]]` block in
`nine.toml` is `409 conflict`, because the next boot would re-create it — remove
the block instead.

`GET /capabilities` returns the generated-tool ceiling in force — each grant with its
`source`, one of `default`, `config` or `approved` — together with every request the
agent has made to widen it.

`POST /capabilities/{id}/decision` takes `{"action": "approve"|"deny"|"revoke"}`. An
approval or revocation applies to the running daemon: the ceiling is installed on the
live tool host and the generated catalog re-projected against it, so a tool that
could not load becomes callable on its next turn with no restart. `approve` and
`deny` take a request id; `revoke` takes a grant id, and only an `approved` grant is
revocable — a `config` or `default` grant is rewritten from `nine.toml` at the next
boot. A bad action, an already-settled request and an unrevocable grant are all
`400`. See [sandboxed-tools.md](sandboxed-tools.md) §7.2.

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

## Event stream

`GET /conversations/{id}/messages/stream` follows a session for as long as the
client stays connected. It forwards every turn the session runs — one this client
posted, one another client posted, or a scheduled wake — not only the next one.

```bash
curl -N http://localhost:8080/api/v1/conversations/abc/messages/stream
```

| Event | When |
|-------|------|
| `connected` | Once, after the daemon accepts the watch |
| `tool_start`, `tool_end` | A tool call begins and ends, with its input and output |
| `sub_agent_start`, `sub_agent_end` | A sub-agent is spawned and finishes |
| `response_chunk` | A fragment of the reply, when the model streams |
| `notice` | A session-level notice, such as a capability downgrade |
| `response` | The turn's final reply |
| `error` | The turn failed, or the stream ended on the daemon's side |
| `done` | The turn is over; the stream stays open for the next one |

An unknown conversation is `404` before the stream opens. A session that exists
but is not loaded is revived from its checkpoint. An idle stream carries a
`: keepalive` comment every 15 seconds, and the stream is exempt from the
server's request timeout.

---

## WebSocket

`/ws/v1/conversations/{id}/messages` carries the event stream's events and lets the
client send on the same connection: turns, and answers to an interactive
session's questions. Every message is a JSON text frame with a `type`.

```bash
websocat -H 'Authorization: Bearer secret' ws://localhost:8080/ws/v1/conversations/abc/messages
{"type":"user_turn","text":"What changed in the repo today?"}
```

The server sends each [event stream](#event-stream) event as its fields plus
`type` — `{"type":"tool_start","tool_name":"fetch",…}`, `{"type":"done",…}`,
`{"type":"error","error":{…}}` — and three more:

| Message | Meaning |
|---------|---------|
| `human_input_required` | An interactive session asks a question: `request_id`, `question`, `options`, `timeout_seconds` |
| `queued` | A `user_turn` arrived while the session was mid-turn; it waits for the next turn |
| `human_input_answered` | An answer was delivered |

The client sends:

| Message | Fields |
|---------|--------|
| `user_turn` | `text`, `force_think` |
| `human_input_answer` | `request_id`, `answer` |

A turn sent over the socket reports its events and reply like any other turn —
the socket shows every turn the session runs — so nothing is reported twice.
Bad input is answered with an `error` (`invalid_request`) and the connection
stays open. An unknown conversation is `404` before the upgrade.

Authentication, rate limiting and logging apply to the upgrade request. A
browser's `Origin` must match `cors_origins`, which defaults to `*`. The server
pings every 30 seconds and closes a peer that stops answering; the server's
request timeouts do not apply.

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
| Local transport underneath | The API server is a translation layer over the daemon's Unix socket, so it runs on the same host as the daemon. It is also the only way to reach Nine over a network: the socket itself carries no authentication, and `auth_token` is the API server's. |
| Browser WebSocket clients cannot send the token | A browser's WebSocket API sets no `Authorization` header, so with `auth_token` set a browser reaches the socket only through a proxy that adds it. Non-browser clients send the header. |
| A slow stream reader loses events | The daemon queues up to 1024 events per stream or socket; a client that falls further behind loses the excess rather than stalling the session's turn. A WebSocket write that cannot complete in 10 seconds closes the socket. |
| Stream omits reasoning detail | Reasoning tokens, stage labels and context-usage updates are not forwarded; the TUI renders them, and the API declares no schema for them. |
| `ask_human` is answerable over the WebSocket only | The SSE stream does not carry `human_input_required`, and no REST endpoint answers one. An interactive conversation driven without the WebSocket waits out each question's timeout. |
| Startup races the daemon | The API server polls for the daemon socket on startup and refuses requests until the daemon is ready. |
