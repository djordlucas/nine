# Contract — API Layer

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

## Process Model

### API-A-1: Process Lifecycle

The API server process:

1. **MUST** be started via `nine api serve` command
2. **MUST** run as a separate process from the daemon
3. **MUST** connect to the daemon's Unix socket on startup
4. **MUST** exit cleanly when the daemon stops or on SIGTERM/SIGINT
5. **MUST NOT** load daemon configuration directly (only through socket)
6. **MUST NOT** access the database directly (only through daemon)

### API-A-2: Startup Sequence

```
1. Parse API-specific configuration (port, host, auth, etc.)
2. Connect to daemon socket
3. Verify daemon is running and responsive
4. Start HTTP server
5. Register health check endpoint
6. Begin accepting requests
```

### API-A-3: Shutdown Sequence

```
1. Receive SIGTERM/SIGINT or daemon disconnect
2. Stop accepting new requests
3. Allow in-flight requests to complete (with timeout)
4. Close socket connection
5. Exit process
```

---

## Configuration

### API-C-1: Configuration Sources

API configuration is separate from daemon configuration:

| Source | Priority | Description |
|--------|----------|-------------|
| Command-line flags | Highest | `--port`, `--host`, `--auth-token`, etc. |
| Environment variables | Medium | `NINE_API_PORT`, `NINE_API_HOST`, `NINE_API_AUTH_TOKEN` |
| Config file | Lowest | `api` section in `nine.toml` |

### API-C-2: Configuration Options

```toml
[api]
enabled = true              # Enable API server (default: false)
port = 8080                 # HTTP port (default: 8080)
host = "localhost"          # Bind address (default: "localhost")
auth_token = ""            # Optional bearer token for authentication
timeout_seconds = 30       # Request timeout (default: 30)
max_connections = 100      # Maximum concurrent connections (default: 100)
cors_origins = ["*"]        # CORS allowed origins
```

Command-line overrides:
```bash
nine api serve --port 3000 --host 0.0.0.0 --auth-token secret
```

---

## HTTP API Specification

### API-HTTP-1: Base URL and Versioning

- **Base path:** `/api/v1` (version 1)
- **Content-Type:** `application/json` for all requests and responses
- **Charset:** UTF-8

### API-HTTP-2: Authentication

When `auth_token` is configured:

- **MUST** include `Authorization: Bearer <token>` header
- **MUST** return `401 Unauthorized` for missing or invalid token
- **MUST** return `403 Forbidden` for valid but insufficient token

### API-HTTP-3: Error Format

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
| `service_unavailable` | 503 | Daemon not running or unreachable |

### API-HTTP-4: Pagination

For list endpoints that support pagination:

**Request parameters:**
- `limit`: Maximum items per page (default: 50, max: 1000)
- `offset`: Number of items to skip (default: 0)
- `cursor`: Opaque cursor for next page (alternative to offset)

**Response envelope:**
```json
{
  "data": [...],
  "pagination": {
    "limit": 50,
    "offset": 0,
    "total": 100,
    "next_cursor": "...",
    "has_more": true
  }
}
```

---

## Endpoint Specification

### API-END-1: Health and Status

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

### API-END-2: Conversations (Sessions)

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

**Request:**
```json
{
  "verbose": false
}
```

**Response:** Same as CLI `nine context` output, formatted as JSON.

#### DELETE `/api/v1/conversations/{id}`

Delete a conversation and all its data.

**Request:**
```json
{
  "force": false
}
```

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

### API-END-3: Messages and Turns

#### GET `/api/v1/conversations/{id}/history`

Get conversation message history.

**Request:** Supports pagination.

**Response:**
```json
{
  "data": [
    {
      "type": "user_turn" | "response" | "tool_start" | "tool_end",
      "agent_id": "agent-id",
      "text": "message content",
      "tool_name": "tool-name",
      "tool_input": {},
      "tool_output": "tool result",
      "timestamp": "2024-01-01T00:00:00Z",
      "turn_number": 1
    }
  ],
  "pagination": { ... }
}
```

#### GET `/api/v1/conversations/{id}/trace`

Get detailed trace of a specific turn.

**Request:**
```json
{
  "turn": 1,
  "sub_agents": false
}
```

**Response:** Same as CLI `nine trace` output, formatted as JSON.

#### POST `/api/v1/conversations/{id}/replay`

Replay a specific turn.

**Request:**
```json
{
  "turn": 1
}
```

**Response:** Same as CLI `nine replay` output, formatted as JSON.

---

### API-END-4: Goals

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

Create a new goal.

**Request:**
```json
{
  "name": "goal-name",
  "description": "goal description",
  "priority": 0
}
```

**Response:**
```json
{
  "id": "goal-id",
  "status": "created",
  "session_id": "agent-id"
}
```

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

Delete a goal.

**Response:**
```json
{
  "message": "goal deleted",
  "id": "goal-id"
}
```

---

### API-END-5: Workflows

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

### API-END-6: Tools

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

---

### API-END-7: Plugins

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

### API-END-8: Notifications

#### GET `/api/v1/notifications`

Get user notifications.

**Request:**
```json
{
  "all": false
}
```

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

### API-END-9: Skills

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
      "source": "builtin" | "user" | "generated",
      "created_at": "2024-01-01T00:00:00Z"
    }
  ],
  "pagination": { ... }
}
```

---

### API-END-10: Memory and Search

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

### API-END-11: Files

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

### API-END-12: Sessions (Attach/Detach)

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

### API-END-13: System

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

## Streaming Endpoints

### API-STREAM-1: Streaming Responses

For endpoints that produce streaming output (turn execution, tool calls), the API supports both:

1. **SSE (Server-Sent Events):** `/api/v1/conversations/{id}/messages/stream`
2. **WebSocket:** `/ws/v1/conversations/{id}/messages`

### API-STREAM-2: SSE Format

```
event: tool_start
data: {"tool_name": "name", "tool_input": {}, "timestamp": 1234567890}

event: tool_end
data: {"tool_name": "name", "tool_output": "result", "timestamp": 1234567890}

event: response_chunk
data: {"text": "chunk", "timestamp": 1234567890}

event: done
data: {"agent_id": "id", "timestamp": 1234567890}
```

### API-STREAM-3: WebSocket Messages

All messages are JSON objects with a `type` field:

```json
{"type": "tool_start", "tool_name": "name", "tool_input": {}, "timestamp": 1234567890}
{"type": "tool_end", "tool_name": "name", "tool_output": "result", "timestamp": 1234567890}
{"type": "response_chunk", "text": "chunk", "timestamp": 1234567890}
{"type": "done", "agent_id": "id", "timestamp": 1234567890}
{"type": "error", "error": {"code": "...", "message": "..."}}
```

---

## Socket Communication

### API-SOCKET-1: Connection Management

The API server:

1. **MUST** connect to the daemon socket on startup
2. **MUST** maintain a connection pool for concurrent requests
3. **MUST** handle connection failures gracefully
4. **MUST** reconnect automatically when the daemon restarts

### API-SOCKET-2: Message Routing

Each API request:

1. **MUST** be translated to the appropriate wire protocol message
2. **MUST** include proper request IDs for correlation
3. **MUST** handle responses and map them back to API format
4. **MUST** propagate errors appropriately

### API-SOCKET-3: Request Context

The API server:

1. **MUST** propagate request timeouts to socket operations
2. **MUST** handle request cancellation
3. **MUST** manage connection lifecycle per request

---

## Security Considerations

### API-SEC-1: Authentication

- **MUST** support bearer token authentication
- **MUST** support API key authentication
- **SHOULD** support JWT authentication (future)
- **MUST** return appropriate error codes for auth failures

### API-SEC-2: Authorization

- **MUST** validate that authenticated user has access to requested resource
- **MUST** implement resource-level access control
- **MUST** log all access attempts

### API-SEC-3: Input Validation

- **MUST** validate all request parameters
- **MUST** sanitize user input
- **MUST** enforce size limits on request bodies
- **MUST** enforce rate limiting (configurable)

### API-SEC-4: Transport Security

- **SHOULD** support HTTPS/TLS
- **SHOULD** support mutual TLS (mTLS)
- **MUST** handle sensitive data appropriately

### API-SEC-5: CORS

- **MUST** respect configured CORS origins
- **MUST** include appropriate CORS headers
- **MUST** support preflight requests

---

## Rate Limiting

### API-RATE-1: Configuration

```toml
[api.rate_limit]
enabled = true
requests_per_minute = 60
burst_size = 10
excluded_paths = ["/health", "/status"]
```

### API-RATE-2: Headers

Rate limit responses **MUST** include:

```
X-RateLimit-Limit: 60
X-RateLimit-Remaining: 55
X-RateLimit-Reset: 30
Retry-After: 30
```

---

## Observability

### API-OBS-1: Logging

The API server **MUST** log:

- All requests (method, path, status, duration)
- All errors
- Authentication attempts
- Rate limit events

### API-OBS-2: Metrics

The API server **SHOULD** expose metrics:

- Request counts by endpoint
- Request durations
- Error rates
- Connection pool stats
- Active connections

### API-OBS-3: Tracing

The API server **SHOULD** support distributed tracing:

- Propagate trace context from incoming requests
- Add trace context to socket messages
- Support OpenTelemetry

---

## CLI Integration

### API-CLI-1: API Command

```bash
# Start the API server
nine api serve [--port 8080] [--host 0.0.0.0] [--auth-token secret]

# Check API status
nine api status

# Stop the API server
nine api stop
```

### API-CLI-2: Configuration via CLI

All API configuration options **MUST** be available via CLI flags.

---

## Feature Parity Matrix

| CLI Command | API Endpoint | Status |
|-------------|--------------|--------|
| `nine send` | POST `/api/v1/conversations/{id}/messages` | Required |
| `nine status` | GET `/api/v1/status` | Required |
| `nine goals` | GET `/api/v1/goals` | Required |
| `nine workflows` | GET `/api/v1/workflows` | Required |
| `nine tools` | GET `/api/v1/tools` | Required |
| `nine plugins` | GET `/api/v1/plugins` | Required |
| `nine sessions` | GET `/api/v1/conversations` | Required |
| `nine context` | GET `/api/v1/conversations/{id}/context` | Required |
| `nine trace` | GET `/api/v1/conversations/{id}/trace` | Required |
| `nine replay` | POST `/api/v1/conversations/{id}/replay` | Required |
| `nine stop` | POST `/api/v1/conversations/{id}/stop` | Required |
| `nine session delete` | DELETE `/api/v1/conversations/{id}` | Required |
| `nine workflow stop` | POST `/api/v1/workflows/{id}/stop` | Required |
| `nine workflow fail` | POST `/api/v1/workflows/{id}/fail` | Required |
| `nine plugins reload` | POST `/api/v1/plugins/reload` | Required |
| `nine tools reload` | POST `/api/v1/tools/reload` | Required |
| `nine notifications` | GET `/api/v1/notifications` | Required |
| `nine docs` | GET `/api/v1/docs` | Required |
| `nine spec` | GET `/api/v1/spec` | Required |
| `nine tool call` | POST `/api/v1/tools/{name}/call` | Required |

---

## Implementation Notes

### API-IMPL-1: Process Architecture

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

### API-IMPL-2: Request Flow

```
HTTP Request → Middleware (auth, rate limit) → Handler → 
Socket Client → Daemon → Socket Response → 
Handler → HTTP Response
```

### API-IMPL-3: Error Handling

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

## Testing Requirements

### API-TEST-1: Unit Tests

- All handlers **MUST** have unit tests
- All middleware **MUST** have unit tests
- Socket client **MUST** have mock tests

### API-TEST-2: Integration Tests

- **MUST** test with real daemon
- **MUST** test all endpoints
- **MUST** test error cases
- **MUST** test authentication
- **MUST** test rate limiting

### API-TEST-3: Contract Tests

- **MUST** verify response formats
- **MUST** verify error formats
- **MUST** verify wire protocol compatibility

---

## Future Enhancements

| Feature | Priority | Status |
|---------|----------|--------|
| WebSocket support | High | Planned |
| JWT authentication | Medium | Future |
| OpenAPI/Swagger docs | Medium | Future |
| GraphQL interface | Low | Future |
| gRPC interface | Low | Future |
| Rate limiting by IP | Medium | Future |
| API key rotation | Medium | Future |

---

## Appendix A: Configuration Reference

### Full Configuration Schema

```toml
[api]
enabled = true
port = 8080
host = "localhost"
auth_token = ""
timeout_seconds = 30
max_connections = 100
cors_origins = ["*"]

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

### Environment Variables

```
NINE_API_ENABLED=true
NINE_API_PORT=8080
NINE_API_HOST=localhost
NINE_API_AUTH_TOKEN=secret
NINE_API_TIMEOUT_SECONDS=30
NINE_API_MAX_CONNECTIONS=100
NINE_API_CORS_ORIGINS=*
```

### Command-line Flags

```
nine api serve --port 8080 --host 0.0.0.0 --auth-token secret --timeout 30
```

---

## Appendix B: Error Code Reference

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
| `service_unavailable` | 503 | Daemon not available | Yes |

---

## Appendix C: Response Examples

### Successful Response

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

### Error Response

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

## Revision History

| Version | Date | Author | Changes |
|---------|------|--------|---------|
| 1.0 | 2025-01-XX | - | Initial design |
