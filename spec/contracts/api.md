# Contract — API layer

**Status:** Design · **Depends on:** wire-protocol, plugin, toolvm · **Used by:** API server, CLI, daemon

This document specifies the Nine API layer, which provides HTTP/REST access to Nine's functionality with feature parity to the CLI. The API runs as a separate process that communicates with the daemon via the Unix socket, similar to how the MCP plugin works.

---

## Overview

The API layer is designed to:

1. **Run as a separate process** - Nine forks itself and runs the API server as a distinct process
2. **Communicate via Unix socket** - The API process talks to the daemon using the same socket protocol as the CLI
3. **Provide feature parity** - Every CLI capability must be available through the API
4. **Maintain isolation** - API process has no direct access to daemon internals, only the socket interface
5. **Support multiple transports** - HTTP/REST primarily, with potential for WebSocket streaming

---

## Architecture

```
┌─────────────────────────────────────────────────────────────┐
│                         API Process                              │
│  ┌─────────────────────────────────────────────────────────┐ │
│  │                    HTTP Server                             │ │
│  │  ┌─────────────┐  ┌─────────────┐  ┌─────────────┐     │ │
│  │  │  REST        │  │  Streaming   │  │  WebSocket    │     │ │
│  │  │  Handlers    │  │  Handlers    │  │  Handlers     │     │ │
│  │  └─────────────┘  └─────────────┘  └─────────────┘     │ │
│  └─────────────────────────────────────────────────────────┘ │
│  ┌─────────────────────────────────────────────────────────┐ │
│  │                  Socket Client                             │ │
│  │  Communicates with daemon via Unix socket using wire       │ │
│  │  protocol (newline-delimited JSON)                         │ │
│  └─────────────────────────────────────────────────────────┘ │
└─────────────────────────────────────────────────────────────┘
                    │
                    ▼
┌─────────────────────────────────────────────────────────────┐
│                      Daemon Process                             │
│  (existing, unchanged - uses same socket interface)            │
└─────────────────────────────────────────────────────────────┘
```

---

## Process model

### API-A-1: process lifecycle

The API server process:

1. **MUST** be started via `nine api serve` command
2. **MUST** run as a separate process from the daemon
3. **MUST** connect to the daemon's Unix socket on startup
4. **MUST** exit cleanly when the daemon stops or on SIGTERM/SIGINT
5. **MUST NOT** load daemon configuration directly (only through socket)
6. **MUST NOT** access the database directly (only through daemon)

### API-A-2: startup sequence

```
1. Parse API-specific configuration (port, host, auth, etc.)
2. Connect to daemon socket
3. Verify daemon is running and responsive
4. Start HTTP server
5. Register health check endpoint
6. Begin accepting requests
```

### API-A-3: shutdown sequence

```
1. Receive SIGTERM/SIGINT or daemon disconnect
2. Stop accepting new requests
3. Allow in-flight requests to complete (with timeout)
4. Close socket connection
5. Exit process
```

---

## Configuration

### API-C-1: configuration sources

API configuration is separate from daemon configuration:

| Source | Priority | Description |
|--------|----------|-------------|
| Command-line flags | Highest | `--port`, `--host`, `--auth-token`, etc. |
| Environment variables | Medium | `NINE_API_PORT`, `NINE_API_HOST`, `NINE_API_AUTH_TOKEN` |
| Config file | Lowest | `api` section in `nine.toml` |

### API-C-2: configuration options

```toml
[api]
enabled = true              # Enable API server (default: false)
port = 8080                 # HTTP port (default: 8080)
host = "localhost"          # Bind address (default: "localhost")
auth_token = ""            # Optional bearer token for authentication
timeout_seconds = 30       # Request timeout (default: 30)
max_connections = 100      # Maximum concurrent connections (default: 100)
cors_origins = ["*"]        # CORS allowed origins
trusted_proxies = []        # Proxies whose forwarding headers are believed (default: none)
```

Command-line overrides:
```bash
nine api serve --port 3000 --host 0.0.0.0 --auth-token secret
```

---

## HTTP API specification

### API-HTTP-1: base URL and versioning

- **Base path:** `/api/v1` (version 1)
- **Content-Type:** `application/json` for all requests and responses
- **Charset:** UTF-8

### API-HTTP-2: authentication

When `auth_token` is configured:

- **MUST** include `Authorization: Bearer <token>` header
- **MUST** return `401 Unauthorized` for missing or invalid token
- **MUST** return `403 Forbidden` for valid but insufficient token

### API-HTTP-3: error format

All errors follow this format:

```json
{
  "error": {
    "code": "string",
    "message": "string",
    "details": {}
  }
}
```

Standard error codes:

| Code | HTTP Status | Description |
|------|-------------|-------------|
| `invalid_request` | 400 | Malformed request body or parameters |
| `unauthorized` | 401 | Missing or invalid authentication |
| `forbidden` | 403 | Insufficient permissions |
| `not_found` | 404 | Resource does not exist |
| `conflict` | 409 | Resource already exists |
| `timeout` | 408 | Request timeout |
| `server_error` | 500 | Internal server error |
| `not_implemented` | 501 | Endpoint declared but not backed by the daemon |
| `service_unavailable` | 503 | Daemon not running or unreachable |

### API-HTTP-4: request argument placement

Operations **MUST NOT** carry arguments in a request body on `GET` or `DELETE`.
OpenAPI 3.x leaves a request body on those methods undefined, so generated
clients drop it and interactive documentation will not send it. Such arguments
belong in the query string.

A body sent to one of these operations is ignored, not an error.

### API-HTTP-5: unimplemented endpoints

An endpoint the daemon cannot serve **MUST** return `501 not_implemented` with
a `details.detail` naming what is missing. It **MUST NOT** return `200` with an
empty list, an echo of the request, or a synthesised identifier, and **MUST
NOT** declare a success schema it cannot produce.

A caller cannot distinguish invented data from a real answer, so a stub that
returns `200` is indistinguishable from a working endpoint until something
downstream depends on it.

No declared endpoint currently returns `501`; the rule governs any added
before the daemon can serve it.

### API-HTTP-6: pagination

Paging is offset based. The daemon materialises a full result set per call, so
there is no server-side stream for an opaque cursor to point into; a cursor
could only re-encode the offset while implying a stability the slice does not
have.

**Request parameters (query string):**

| Parameter | Type | Default | Constraint |
|-----------|------|---------|------------|
| `limit` | int | 50 | 1–1000 |
| `offset` | int | 0 | ≥ 0 |

A list endpoint **MUST** reject a malformed or out-of-range value with `400
invalid_request` rather than substituting the default, and **MUST** validate
before dialling the daemon. An `offset` past the end of the set returns an
empty page, not an error.

**Response envelope:**
```json
{
  "data": [...],
  "pagination": {
    "limit": 50,
    "offset": 0,
    "total": 100,
    "has_more": true
  }
}
```

`data` **MUST** serialise as `[]` rather than `null` for an empty page.

---

## Endpoint specification

### API-END-1: health and status

#### GET `/api/v1/health`

Check API server health.

**Request:** None

**Response:**
```json
{
  "status": "healthy" | "degraded" | "unhealthy",
  "daemon_connected": true,
  "daemon_health": "healthy" | "degraded" | "unhealthy",
  "uptime_seconds": 12345,
  "version": "1.0.0"
}
```

#### GET `/api/v1/status`

Get daemon status.

**Request:** None

**Response:** Same as CLI `nine status` output, formatted as JSON.

---

### API-END-2: conversations (sessions)

#### POST `/api/v1/conversations`

Create a new conversation.

**Request:**
```json
{
  "interactive": false
}
```

**Response:**
```json
{
  "id": "agent-id-string",
  "role": "default",
  "instance_name": "nine-instance",
  "created_at": "2024-01-01T00:00:00Z"
}
```

#### GET `/api/v1/conversations`

List all conversations.

**Request:** Supports pagination parameters.

**Response:**
```json
{
  "data": [
    {
      "id": "agent-id",
      "name": "conversation-name",
      "status": "active" | "idle" | "stopped",
      "age_seconds": 123,
      "events": 456,
      "protected": false,
      "attached": false
    }
  ],
  "pagination": { ... }
}
```

#### GET `/api/v1/conversations/{id}`

Get conversation details.

**Response:**
```json
{
  "id": "agent-id",
  "name": "conversation-name",
  "role": "resolved-role",
  "plan_mode": "off" | "plan-only" | "always",
  "status": "active" | "idle" | "stopped",
  "created_at": "2024-01-01T00:00:00Z",
  "updated_at": "2024-01-01T00:00:00Z",
  "events_count": 123,
  "protected": false
}
```

#### POST `/api/v1/conversations/{id}/messages`

Send a message to a conversation (execute a turn).

**Request:**
```json
{
  "text": "user message here",
  "force_think": false
}
```

**Response (streaming capable):**
```json
{
  "id": "turn-id",
  "agent_id": "agent-id",
  "text": "assistant response",
  "completed_at": "2024-01-01T00:00:00Z"
}
```

#### GET `/api/v1/conversations/{id}/context`

Get conversation context breakdown.

**Query parameters:** `verbose` (bool, default false)

**Response:** Same as CLI `nine context` output, formatted as JSON.

#### DELETE `/api/v1/conversations/{id}`

Delete a conversation and all its data.

**Query parameters:** `force` (bool, default false)

**Response:**
```json
{
  "message": "conversation deleted",
  "deleted_events": 123
}
```

#### POST `/api/v1/conversations/{id}/stop`

Stop a conversation (end session but keep history).

**Response:**
```json
{
  "message": "conversation stopped",
  "status": "stopped"
}
```

---

### API-END-3: messages and turns

#### GET `/api/v1/conversations/{id}/history`

Get a conversation's transcript, every turn, oldest first. Read from the event
journal through the daemon's `session_history` message; it does not attach.

**Request:** Supports pagination.

**Response:**
```json
{
  "data": [
    {
      "type": "user_turn" | "response" | "tool_start" | "tool_end" | "sub_agent_start" | "sub_agent_end",
      "agent_id": "agent-id",
      "text": "prompt, reply, or sub-agent task",
      "tool_name": "tool-name",
      "tool_input": {},
      "tool_output": "tool result",
      "sub_agent_id": "sub-agent-id",
      "role": "sub-agent role",
      "status": "sub-agent outcome",
      "timestamp": "2024-01-01T00:00:00Z",
      "turn_number": 1
    }
  ],
  "pagination": { ... }
}
```

`404` when the daemon knows no such session.

#### GET `/api/v1/conversations/{id}/trace`

Get the conversation's event journal, as `nine trace` shows it. Read through the
daemon's `session_events` message.

**Query parameters:** `turn` (int ≥ 0, default 0 — every turn, as `nine trace`
with no `--turn`), `sub_agents` (bool, default false)

**Response:**
```json
{
  "agent_id": "agent-id",
  "turns": 1,
  "events": [
    {"seq": 1, "turn": 1, "type": "turn_start", "span_id": "t1", "parent_span_id": "",
     "timestamp": "2024-01-01T00:00:00Z", "payload": {}}
  ],
  "sub_agents": [
    {"agent_id": "sub-id", "parent_agent_id": "agent-id", "spawned_at_seq": 7,
     "events": [ ... ], "error": "present only when the trace could not be read"}
  ]
}
```

`payload` is the event body exactly as journaled (spec/contracts/event-journal.md).
`sub_agents` is present only with `sub_agents=true`: every sub-agent spawned in
`events`, and in theirs in turn, flat and in spawn order. `404` when the daemon
knows no such session.

#### POST `/api/v1/conversations/{id}/replay`

Reconstruct one turn from the journal, as `nine replay` does. Nothing is
re-executed: no model call, no tool run.

**Request:**
```json
{
  "turn": 1
}
```

**Response:**
```json
{
  "agent_id": "agent-id",
  "turn": 1,
  "trigger": "user",
  "input": "the turn's input",
  "llm_calls": [
    {"n": 1, "system": "...", "message_count": 4, "tool_names": [], "tokens_used": 1200,
     "budget": 8000, "input_tokens": 1250, "output_tokens": 80, "stop_reason": "tool_use",
     "text": "", "tool_calls": [{"name": "tool", "input": {}}]}
  ],
  "tool_calls": [
    {"name": "tool", "input": {}, "output": "...", "error": "", "duration_ms": 12, "attempts": 1,
     "truncated": false, "output_chars": 0, "spill_path": "",
     "http": [{"method": "GET", "url": "...", "host": "...", "status": 200, "bytes": 512,
               "truncated": false, "duration_ms": 30, "error": ""}]}
  ],
  "sub_agents": [{"id": "sub-id", "task": "...", "role": "...", "status": "done"}],
  "result": {"text": "...", "error": "", "tool_count": 1, "duration_ms": 900}
}
```

An LLM call's `llm_request` and `llm_response` share a span id and are joined on
it, as are a tool call's `tool_start` and `tool_end`; a `tool_http` event joins the
tool call named by its parent span. `400` for a missing or non-positive `turn`;
`404` for an unknown session or a turn the journal does not hold.

---

### API-END-4: goals

#### GET `/api/v1/goals`

List all goals.

**Response:**
```json
{
  "data": [
    {
      "id": "goal-id",
      "name": "goal-name",
      "description": "goal description",
      "status": "active" | "completed" | "failed" | "paused",
      "priority": 0,
      "created_at": "2024-01-01T00:00:00Z",
      "updated_at": "2024-01-01T00:00:00Z",
      "session_id": "agent-id"
    }
  ],
  "pagination": { ... }
}
```

#### POST `/api/v1/goals`

Create a goal as the operator, through the daemon's `goal_create` message.

**Request:**
```json
{
  "description": "goal description",
  "parent_id": "optional parent goal id"
}
```

**Response:** `201`
```json
{
  "goal": { "id": "goal-id", "description": "...", "status": "active", "parent_type": "operator", ... },
  "pursue_session": "spawned" | "limit_reached" | "none",
  "session_id": "goal-id"
}
```

A top-level goal (no `parent_id`) gets `parent_type` `operator` and a pursue
session whose id is the goal's (docs/goal-sessions.md): `spawned`, or
`limit_reached` when the daemon is at `[processes] max_running` and the goal is
recorded unattended. A sub-goal gets `parent_type` `goal`, no session, `none`,
and no `session_id`. `400` for an empty description; `404` for an unknown parent.

The caller is the operator, not an agent, so the role gate on the agent tool
`goal_create` (a role's `Delegates` flag) does not apply — the same standing an
`[[process]]` block in nine.toml has. If the pursue session fails to start, the
goal is removed and the request fails rather than leaving an unattended goal
that reads as a cap outcome.

#### GET `/api/v1/goals/{id}`

Get goal details.

**Response:**
```json
{
  "id": "goal-id",
  "name": "goal-name",
  "description": "goal description",
  "status": "active",
  "priority": 0,
  "created_at": "2024-01-01T00:00:00Z",
  "updated_at": "2024-01-01T00:00:00Z",
  "session_id": "agent-id",
  "progress": "current progress description",
  "events_count": 123
}
```

#### DELETE `/api/v1/goals/{id}`

Delete a goal and every sub-goal beneath it, and stop the goal's pursue session,
through the daemon's `goal_delete` message. The session's transcript and journal
are kept; the goal's processes are removed so nothing starts it again.

**Response:**
```json
{
  "id": "goal-id",
  "message": "deleted 2 goal(s); its session was stopped and its history kept",
  "deleted": ["goal-id", "sub-goal-id"],
  "session_stopped": true
}
```

`404` for an unknown goal. `409 conflict` for a goal with `parent_type` `config`:
nine.toml declares it, and the next boot would re-create it.

---

### API-END-5: workflows

#### GET `/api/v1/workflows`

List all workflows.

**Response:**
```json
{
  "data": [
    {
      "id": "workflow-id",
      "name": "workflow-name",
      "status": "running" | "completed" | "failed" | "cancelled",
      "steps": 5,
      "current_step": 2,
      "created_at": "2024-01-01T00:00:00Z"
    }
  ],
  "pagination": { ... }
}
```

#### POST `/api/v1/workflows/{id}/stop`

Stop/cancel a workflow.

**Response:**
```json
{
  "message": "workflow cancelled",
  "id": "workflow-id",
  "status": "cancelled"
}
```

#### POST `/api/v1/workflows/{id}/fail`

Mark a workflow as failed.

**Response:**
```json
{
  "message": "workflow marked as failed",
  "id": "workflow-id",
  "status": "failed"
}
```

---

### API-END-6: tools

#### GET `/api/v1/tools`

List all available tools (plugins + sandboxed).

**Response:**
```json
{
  "data": [
    {
      "name": "tool-name",
      "description": "tool description",
      "plugin": "plugin-name",
      "kind": "plugin" | "sandboxed" | "generated",
      "loaded": true,
      "capabilities": ["fs.read", "net.http"],
      "error": ""
    }
  ],
  "pagination": { ... }
}
```

#### POST `/api/v1/tools/{name}/call`

Call a tool directly (bypassing LLM).

**Request:**
```json
{
  "args": {},
  "live_state": false
}
```

**Response:**
```json
{
  "tool_name": "tool-name",
  "output": "tool result",
  "duration_ms": 123,
  "success": true
}
```

#### GET `/api/v1/tools/{name}`

Get tool details.

**Response:**
```json
{
  "name": "tool-name",
  "description": "tool description",
  "plugin": "plugin-name",
  "kind": "plugin",
  "loaded": true,
  "input_schema": {},
  "capabilities": {},
  "manifest_path": "/path/to/manifest",
  "generated": false
}
```

#### DELETE `/api/v1/tools/{name}`

Delete a tool Nine wrote, and its standing run if it has one.

| Status | When |
|---|---|
| 200 | deleted: `{"name": "…", "message": "…"}` |
| 400 | missing name, or tool writing (`[tools.agent]`) is off |
| 403 | a shipped or operator-installed tool, which cannot be deleted |
| 404 | no tool Nine wrote by that name |

---

### API-END-7: plugins

#### GET `/api/v1/plugins`

List all plugins.

**Response:**
```json
{
  "data": [
    {
      "name": "plugin-name",
      "source": "builtin" | "user",
      "loaded": true,
      "tools": ["tool1", "tool2"],
      "error": "",
      "disabled": false
    }
  ],
  "pagination": { ... }
}
```

#### POST `/api/v1/plugins/reload`

Reload user plugins.

**Response:**
```json
{
  "message": "plugins reloaded",
  "loaded_count": 5,
  "failed_count": 0,
  "plugins": [...]
}
```

---

### API-END-8: notifications

#### GET `/api/v1/notifications`

Get user notifications.

**Query parameters:** `all` (bool, default false), plus `limit` and `offset`
(API-HTTP-6)

**Response:**
```json
{
  "data": [
    {
      "id": "notification-id",
      "title": "notification title",
      "message": "notification content",
      "severity": "info" | "warning" | "error",
      "created_at": "2024-01-01T00:00:00Z",
      "seen": false,
      "source": "agent-id",
      "type": "notification-type"
    }
  ],
  "pagination": { ... }
}
```

---

### API-END-9: skills

#### GET `/api/v1/skills`

List all skills.

**Response:**
```json
{
  "data": [
    {
      "name": "skill-name",
      "description": "skill description",
      "tags": ["tag1", "tag2"],
      "source": "builtin" | "user" | "agent"
    }
  ],
  "pagination": { ... }
}
```

---

### API-END-10: memory and search

#### GET `/api/v1/memory`

List memory entries.

**Request:**
```json
{
  "query": "search term",
  "limit": 10
}
```

**Response:**
```json
{
  "data": [
    {
      "key": "memory-key",
      "value": "memory content",
      "created_at": "2024-01-01T00:00:00Z",
      "updated_at": "2024-01-01T00:00:00Z",
      "metadata": {}
    }
  ],
  "pagination": { ... }
}
```

#### POST `/api/v1/memory`

Create a memory entry.

**Request:**
```json
{
  "key": "memory-key",
  "value": "memory content",
  "metadata": {}
}
```

**Response:**
```json
{
  "key": "memory-key",
  "created": true
}
```

---

### API-END-11: files

#### GET `/api/v1/files`

List stored files.

**Request:**
```json
{
  "path": "/workspace",
  "recursive": false
}
```

**Response:**
```json
{
  "data": [
    {
      "name": "filename",
      "path": "/full/path",
      "size": 1234,
      "created_at": "2024-01-01T00:00:00Z",
      "is_dir": false
    }
  ],
  "pagination": { ... }
}
```

#### POST `/api/v1/files`

Store a file.

**Request:**
```json
{
  "path": "/workspace/file.txt",
  "content": "base64-encoded-content"
}
```

**Response:**
```json
{
  "path": "/workspace/file.txt",
  "stored": true,
  "size": 1234
}
```

#### GET `/api/v1/files/{path}`

Get file content.

**Response:**
```json
{
  "path": "/workspace/file.txt",
  "content": "base64-encoded-content",
  "size": 1234,
  "mime_type": "text/plain"
}
```

---

### API-END-12: sessions (Attach/Detach)

#### POST `/api/v1/sessions/attach`

Attach to an existing session for streaming.

**Request:**
```json
{
  "agent_id": "agent-id"
}
```

**Response:**
```json
{
  "agent_id": "agent-id",
  "name": "session-name",
  "role": "session-role",
  "instance_name": "daemon-instance",
  "replay_events": [...],
  "pending_response": "",
  "history": [...]
}
```

---

### API-END-13: system

#### GET `/api/v1/docs`

Get documentation topics.

**Response:**
```json
{
  "topics": ["topic1", "topic2", ...]
}
```

#### GET `/api/v1/docs/{topic}`

Get specific documentation.

**Response:**
```json
{
  "topic": "topic-name",
  "content": "markdown content"
}
```

#### GET `/api/v1/spec`

Get specification topics.

**Response:**
```json
{
  "topics": ["topic1", "topic2", ...]
}
```

#### GET `/api/v1/spec/{topic}`

Get specific specification.

**Response:**
```json
{
  "topic": "topic-name",
  "content": "markdown content"
}
```

---

## Streaming endpoints

### API-STREAM-1: streaming responses

For endpoints that produce streaming output (turn execution, tool calls), the API supports both:

1. **SSE (Server-Sent Events):** `/api/v1/conversations/{id}/messages/stream`
2. **WebSocket:** `/ws/v1/conversations/{id}/messages` (API-STREAM-3). It is
   outside the OpenAPI document, which cannot describe a WebSocket, and outside
   `/api/v1`; authentication, rate limiting and logging apply to its upgrade
   request as to any route.

The SSE stream is backed by the daemon's `watch` message. It **MUST** forward
every turn the session runs while the client is connected, whoever started it,
and **MUST** answer `404` before opening when the daemon knows no such session.
It is exempt from the server's write timeout and carries a `: keepalive` comment
every 15 seconds while idle.

### API-STREAM-2: SSE format

```
event: connected
data: {"agent_id": "id", "message": "stream connected"}

event: tool_start
data: {"tool_name": "name", "tool_input": {}, "timestamp": 1234567890}

event: tool_end
data: {"tool_name": "name", "tool_output": "result", "timestamp": 1234567890}

event: sub_agent_start
data: {"sub_agent_id": "id", "task": "...", "role": "...", "timestamp": 1234567890}

event: sub_agent_end
data: {"sub_agent_id": "id", "task": "...", "role": "...", "status": "done", "timestamp": 1234567890}

event: response_chunk
data: {"text": "chunk", "timestamp": 1234567890}

event: notice
data: {"text": "...", "timestamp": 1234567890}

event: response
data: {"agent_id": "id", "text": "the reply", "timestamp": 1234567890}

event: error
data: {"error": {"code": "server_error", "message": "..."}}

event: done
data: {"agent_id": "id", "timestamp": 1234567890}
```

A turn ends with `response` or `error`, then `done`; the stream stays open for
the next turn. Reasoning tokens, stage labels and context updates are not
forwarded.

### API-STREAM-3: WebSocket messages

The WebSocket carries the SSE stream's events and adds the client's direction.
Every message, both ways, is one JSON text frame with a `type` field.

**Server → client.** Each SSE event (API-STREAM-2) as its payload's fields plus
`type`, with the same names, the same backing `watch`, and the same per-turn
rule; plus three the WebSocket alone can act on:

```json
{"type": "connected", "agent_id": "id", "message": "stream connected"}
{"type": "tool_start", "tool_name": "name", "tool_input": {}, "timestamp": 1234567890}
{"type": "tool_end", "tool_name": "name", "tool_output": "result", "timestamp": 1234567890}
{"type": "response_chunk", "text": "chunk", "timestamp": 1234567890}
{"type": "response", "agent_id": "id", "text": "the reply", "timestamp": 1234567890}
{"type": "done", "agent_id": "id", "timestamp": 1234567890}
{"type": "error", "error": {"code": "...", "message": "..."}}
{"type": "human_input_required", "request_id": "r1", "question": "...", "options": ["yes", "no"], "timeout_seconds": 300, "origin": ""}
{"type": "queued", "text": "Your message has been queued ..."}
{"type": "human_input_answered", "request_id": "r1"}
```

**Client → server:**

```json
{"type": "user_turn", "text": "...", "force_think": false}
{"type": "human_input_answer", "request_id": "r1", "answer": "yes"}
```

| Rule | Detail |
|------|--------|
| Opening | The daemon accepts the watch before the upgrade; an unknown conversation **MUST** be a `404` response, never an opened socket |
| A turn's events | A `user_turn` runs on its own daemon connection. Its events and outcome arrive through the watch like any turn's; the server **MUST NOT** also report them, or they would arrive twice |
| Queued | A `user_turn` sent while the session is mid-turn is queued, answered by `queued` |
| Refused | A `user_turn` with empty text, an unknown type, a non-text frame or malformed JSON is answered `error` with `invalid_request`; a turn the daemon refuses before running is `error` with its code. The connection stays open |
| Answers | `human_input_answer` is delivered on the conversation's id (hitl.md R-HITL.6), answered by `human_input_answered` or `error` |
| Origin | A browser `Origin` is checked against `cors_origins`; the `*` default skips the check. A request without `Origin` is not checked |
| Liveness | The server pings every 30 seconds and closes a peer that does not answer; a write that cannot complete in 10 seconds closes the connection. The server's request timeouts do not apply |
| Size | A client message is at most 1 MiB |
| End | When the daemon ends the watch, the server sends `error` (`service_unavailable`) and closes with status 1001 |

---

## Socket communication

### API-SOCKET-1: connection management

The API server:

1. **MUST** connect to the daemon socket on startup
2. **MUST** maintain a connection pool for concurrent requests
3. **MUST** handle connection failures gracefully
4. **MUST** reconnect automatically when the daemon restarts

### API-SOCKET-2: message routing

Each API request:

1. **MUST** be translated to the appropriate wire protocol message
2. **MUST** include proper request IDs for correlation
3. **MUST** handle responses and map them back to API format
4. **MUST** propagate errors appropriately

### API-SOCKET-3: request context

The API server:

1. **MUST** propagate request timeouts to socket operations
2. **MUST** handle request cancellation
3. **MUST** manage connection lifecycle per request

---

## Security considerations

### API-SEC-1: authentication

- **MUST** support bearer token authentication
- **MUST** support API key authentication
- **SHOULD** support JWT authentication (future)
- **MUST** return appropriate error codes for auth failures

### API-SEC-2: authorization

- **MUST** validate that authenticated user has access to requested resource
- **MUST** implement resource-level access control
- **MUST** log all access attempts

### API-SEC-3: client address attribution

The API server:

1. **MUST NOT** derive a client address from `X-Forwarded-For` or `X-Real-IP`
   unless the transport peer is a configured trusted proxy
2. **MUST** default to an empty trusted-proxy set, keying on the transport peer
3. **MUST** resolve a trusted forwarding chain right to left, selecting the
   rightmost address that is not itself a trusted proxy
4. **MUST** apply rate limiting before authentication, so requests that fail
   authentication consume rate-limit budget

Both forwarding headers are set by the sender. Trusting them from an arbitrary
peer lets a client obtain a fresh rate-limit bucket per request by varying the
header, and rate limiting applied after authentication leaves credential
guessing unthrottled.

### API-SEC-4: input validation

Requests **MUST** be validated against the OpenAPI document before a handler
runs: required body fields, declared types, and declared parameter ranges. The
API server **MUST** report a validation failure in the standard error shape
(API-HTTP-3), not the transport's default.

Authentication is out of scope for that validation: bearer auth is optional
(API-HTTP-2) while the document declares operations secured unconditionally, so
the middleware enforces it.


- **MUST** validate all request parameters
- **MUST** sanitize user input
- **MUST** enforce size limits on request bodies
- **MUST** enforce rate limiting (configurable)

### API-SEC-5: transport security

- **SHOULD** support HTTPS/TLS
- **SHOULD** support mutual TLS (mTLS)
- **MUST** handle sensitive data appropriately

### API-SEC-6: CORS

- **MUST** respect configured CORS origins
- **MUST** include appropriate CORS headers
- **MUST** support preflight requests

---

## Rate limiting

### API-RATE-1: configuration

```toml
[api.rate_limit]
enabled = true
requests_per_minute = 60
burst_size = 10
excluded_paths = ["/health", "/status"]
```

### API-RATE-2: headers

Rate limit responses **MUST** include:

```
X-RateLimit-Limit: 60
X-RateLimit-Remaining: 55
X-RateLimit-Reset: 30
Retry-After: 30
```

---

## Observability

### API-OBS-1: logging

The API server **MUST** log:

- All requests (method, path, status, duration)
- All errors
- Authentication attempts
- Rate limit events

### API-OBS-2: metrics

The API server **SHOULD** expose metrics:

- Request counts by endpoint
- Request durations
- Error rates
- Connection pool stats
- Active connections

### API-OBS-3: tracing

The API server **SHOULD** support distributed tracing:

- Propagate trace context from incoming requests
- Add trace context to socket messages
- Support OpenTelemetry

---

## CLI integration

### API-CLI-1: API command

```bash
# Start the API server
nine api serve [--port 8080] [--host 0.0.0.0] [--auth-token secret]

# Check API status
nine api status

# Stop the API server
nine api stop
```

### API-CLI-2: configuration via CLI

All API configuration options **MUST** be available via CLI flags.

---

## Feature parity matrix

| CLI Command | API Endpoint | Status |
|-------------|--------------|--------|
| `nine send` | POST `/api/v1/conversations/{id}/messages` | Implemented |
| `nine status` | GET `/api/v1/status` | Implemented |
| `nine goals` | GET `/api/v1/goals` | Implemented |
| `nine workflows` | GET `/api/v1/workflows` | Implemented |
| `nine tools` | GET `/api/v1/tools` | Implemented |
| `nine plugins` | GET `/api/v1/plugins` | Implemented |
| `nine sessions` | GET `/api/v1/conversations` | Implemented |
| `nine context` | GET `/api/v1/conversations/{id}/context` | Implemented |
| `nine trace` | GET `/api/v1/conversations/{id}/trace` | Implemented |
| `nine replay` | POST `/api/v1/conversations/{id}/replay` | Implemented |
| `nine stop` | POST `/api/v1/conversations/{id}/stop` | Implemented |
| `nine session delete` | DELETE `/api/v1/conversations/{id}` | Implemented |
| `nine workflow stop` | POST `/api/v1/workflows/{id}/stop` | Implemented |
| `nine workflow fail` | POST `/api/v1/workflows/{id}/fail` | Implemented |
| `nine plugins reload` | POST `/api/v1/plugins/reload` | Implemented |
| `nine tools reload` | POST `/api/v1/tools/reload` | Implemented |
| `nine tools delete` | DELETE `/api/v1/tools/{name}` | Implemented |
| `nine notifications` | GET `/api/v1/notifications` | Implemented |
| `nine docs` | GET `/api/v1/docs` | Implemented |
| `nine spec` | GET `/api/v1/spec` | Implemented |
| `nine tool call` | POST `/api/v1/tools/{name}/call` | Implemented |
| — | GET `/api/v1/conversations/{id}/history` | Implemented |
| — | POST `/api/v1/goals` | Implemented — as the operator; not role-gated |
| — | DELETE `/api/v1/goals/{id}` | Implemented |
| — | GET `/api/v1/skills` | Implemented |
| — | GET `/api/v1/conversations/{id}/messages/stream` | Implemented |
| — | WS `/ws/v1/conversations/{id}/messages` | Implemented |

---

## Implementation notes

### API-IMPL-1: process architecture

The API server runs as a separate Go process:

```go
package main

func serveAPI() {
    cfg := config.LoadAPIConfig()
    
    // Connect to daemon
    daemonClient := protocol.Connect(cfg.SocketPath)
    
    // Create API server
    apiServer := api.NewServer(cfg, daemonClient)
    
    // Start HTTP server
    apiServer.ListenAndServe()
}
```

### API-IMPL-2: request flow

```
HTTP Request → recovery → logging → CORS → rate limit → auth → Handler →
Socket Client → Daemon → Socket Response →
Handler → HTTP Response
```

Rate limiting precedes authentication (API-SEC-3.4). CORS precedes both so
preflight requests answer without credentials.

### API-IMPL-3: error handling

```go
func handleRequest(w http.ResponseWriter, r *http.Request) {
    // Parse request
    req, err := parseRequest(r)
    if err != nil {
        writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
        return
    }
    
    // Call daemon via socket
    resp, err := socketClient.Send(req)
    if err != nil {
        if errors.Is(err, protocol.ErrDaemonUnavailable) {
            writeError(w, http.StatusServiceUnavailable, "service_unavailable", err.Error())
        } else {
            writeError(w, http.StatusInternalServerError, "server_error", err.Error())
        }
        return
    }
    
    // Format response
    writeJSON(w, http.StatusOK, formatResponse(resp))
}
```

---

## Testing requirements

### API-TEST-1: unit tests

- All handlers **MUST** have unit tests
- All middleware **MUST** have unit tests
- Socket client **MUST** have mock tests

### API-TEST-2: integration tests

- **MUST** test with real daemon
- **MUST** test all endpoints
- **MUST** test error cases
- **MUST** test authentication
- **MUST** test rate limiting

### API-TEST-3: contract tests

- **MUST** verify response formats
- **MUST** verify error formats
- **MUST** verify wire protocol compatibility

---

## Future enhancements

| Feature | Priority | Status |
|---------|----------|--------|
| WebSocket support | High | Planned |
| JWT authentication | Medium | Future |
| OpenAPI 3.1 document | — | Implemented — `internal/api/openapi.yaml` is the source of truth; models and the server interface are generated from it, and it is served at `/api/v1/openapi.yaml` (see docs/api.md) |
| GraphQL interface | Low | Future |
| gRPC interface | Low | Future |
| Rate limiting by IP | Medium | Future |
| API key rotation | Medium | Future |

---

## Appendix A: configuration reference

### Full configuration schema

```toml
[api]
enabled = true
port = 8080
host = "localhost"
auth_token = ""
timeout_seconds = 30
max_connections = 100
cors_origins = ["*"]
trusted_proxies = []

[api.rate_limit]
enabled = true
requests_per_minute = 60
burst_size = 10
excluded_paths = ["/health", "/status"]

[api.tls]
enabled = false
cert_path = ""
key_path = ""
```

### Environment variables

```
NINE_API_ENABLED=true
NINE_API_PORT=8080
NINE_API_HOST=localhost
NINE_API_AUTH_TOKEN=secret
NINE_API_TIMEOUT_SECONDS=30
NINE_API_MAX_CONNECTIONS=100
NINE_API_CORS_ORIGINS=*
NINE_API_TRUSTED_PROXIES=10.0.0.0/8,192.168.1.7
```

### Command-line flags

```
nine api serve --port 8080 --host 0.0.0.0 --auth-token secret --timeout 30
```

---

## Appendix B: error code reference

| Code | HTTP | Description | Retryable |
|------|------|-------------|-----------|
| `invalid_request` | 400 | Bad request format | No |
| `unauthorized` | 401 | Missing/invalid auth | No |
| `forbidden` | 403 | Insufficient permissions | No |
| `not_found` | 404 | Resource doesn't exist | No |
| `conflict` | 409 | Resource exists | No |
| `timeout` | 408 | Request timeout | Yes |
| `too_many_requests` | 429 | Rate limited | Yes |
| `server_error` | 500 | Internal error | Yes |
| `not_implemented` | 501 | Not backed by the daemon | No |
| `service_unavailable` | 503 | Daemon not available | Yes |

---

## Appendix C: response examples

### Successful response

```json
{
  "data": {...},
  "meta": {
    "request_id": "req-12345",
    "timestamp": "2024-01-01T00:00:00Z",
    "duration_ms": 123
  }
}
```

### Error response

```json
{
  "error": {
    "code": "not_found",
    "message": "conversation not found",
    "details": {
      "resource": "conversation",
      "id": "abc123"
    }
  },
  "meta": {
    "request_id": "req-12345",
    "timestamp": "2024-01-01T00:00:00Z"
  }
}
```

---

## Revision history

| Version | Date | Author | Changes |
|---------|------|--------|---------|
| 1.0 | 2025-01-XX | - | Initial design |
| 1.1 | 2026-10-04 | - | History, trace, replay, goal create/delete and skills served (no `501` remains); SSE stream forwards events (API-STREAM-1/2) |
| 1.2 | 2026-10-04 | - | WebSocket transport served, with turns and `ask_human` answers from the client (API-STREAM-3) |
