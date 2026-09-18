# AgentWorker

An `AgentWorker` wraps a single `agent.Loop` and serializes user turns for one
conversation or background session. Each worker owns a goroutine that processes
turns one at a time from an inbox channel.

## Component Diagram

```mermaid
flowchart TD
    subgraph Caller["Caller (Daemon)"]
        DA[Daemon.userTurn]
        PROG[progressCh\nbuffered=256]
    end

    subgraph Runner["AgentWorker goroutine"]
        INBOX[inbox chan turnReq\nbuffered=1]
        NOTIF[prependNotifications\nfetch from NotifStore]
        CBS[register callbacks\nOnContextUpdate\nOnToolStart / OnToolEnd\nOnChunk]
        RUN[agent.Loop.Run\nLLM call]
        CLR[clear callbacks]
        STALL[checkStall\nconsecutive no-tool turns]
        CKPT[checkpoint\nSaveCheckpoint → saveCkpt]
        RESP[send turnResp\nto respCh]
        ONC[onComplete callback]

        INBOX --> NOTIF
        NOTIF --> CBS
        CBS --> RUN
        RUN --> CLR
        CLR --> STALL
        STALL -->|"stallN >= Limit"| ONSTALL[OnStall fn]
        STALL --> CKPT
        CKPT --> RESP
        RESP --> ONC
        ONC --> INBOX
    end

    subgraph Stores["External"]
        NS[NotifStore]
        CS[CheckpointStore]
        LOOP[agent.Loop]
    end

    DA -->|"turnAsync(ctx, text)"| INBOX
    DA -->|"setProgress(fn)"| PROG
    RUN -->|"emitEvent via progressFn"| PROG
    PROG -->|"tool_start / tool_end\ncontext_update\nresponse_chunk"| DA

    NOTIF -->|"Fetch(agentID)"| NS
    CKPT -->|"Save(agentID, data)"| CS
    RUN --- LOOP

    RESP -->|"turnResp{text, err}"| DA

    style Runner fill:#3a1a1a,color:#fff
    style Caller fill:#1e3a5f,color:#fff
    style Stores fill:#2a2a1a,color:#fff
```

## Turn Lifecycle

```mermaid
sequenceDiagram
    participant D as Daemon
    participant R as AgentWorker goroutine
    participant N as NotifStore
    participant L as agent.Loop
    participant C as CheckpointStore

    D->>R: turnAsync(ctx, text) → sends turnReq to inbox
    D->>R: setProgress(fn) registers progress callback

    R->>N: Fetch(agentID) — pending notifications
    N-->>R: []string notifications
    R->>R: prepend notifications to text

    R->>L: register OnToolStart / OnToolEnd / OnChunk / OnContextUpdate
    R->>L: Loop.Run(ctx, text)

    loop streaming
        L-->>R: OnToolStart(name, input)
        R-->>D: emitEvent tool_start
        L-->>R: OnChunk(text)
        R-->>D: emitEvent response_chunk
        L-->>R: OnToolEnd(name, input, output)
        R-->>D: emitEvent tool_end
    end

    L-->>R: result text
    R->>R: clear all callbacks
    R->>R: checkStall — track consecutive no-tool turns
    R->>L: SaveCheckpoint()
    L-->>R: []byte state
    R->>C: Save(agentID, data)
    R->>D: turnResp{text, err} → respCh
    R->>R: onComplete(agentID)
```

## Stall Detection

The AgentWorker tracks consecutive turns where `agent.Loop.LastRunToolCount() == 0`. When `stallN` reaches `StallConfig.Limit`, `OnStall` fires and the counter resets.

```
turn completes
  └─ tools used > 0 → stallN = 0
  └─ tools used == 0 → stallN++
       └─ stallN >= Limit → OnStall(agentID), stallN = 0
```

Stall detection is disabled when `Limit == 0` or `OnStall == nil`.

## Concurrency Model

- The inbox channel has capacity 1. The daemon blocks sending a new turn until the current one finishes, making turns strictly sequential per worker.
- `progressFn` is guarded by a mutex so the daemon goroutine can register/clear it while the AgentWorker goroutine calls it.
- `stop()` closes the inbox and blocks on `<-stopped`, ensuring the goroutine drains cleanly before the worker is discarded.

## Key Methods

| Method | Description |
|--------|-------------|
| `newAgentWorker` | Creates the worker and starts the goroutine |
| `turnAsync` | Non-blocking submit; returns a `chan turnResp` |
| `turn` | Blocking submit; wraps `turnAsync` |
| `stop` | Closes inbox, waits for goroutine to exit |
| `setProgress` | Registers/clears the streaming progress callback |
| `emitEvent` | Thread-safe progress event emission |
| `prependNotifications` | Fetches pending notifs and prepends them to the message |
| `checkStall` | Increments/resets stall counter; fires `OnStall` at threshold |
| `checkpoint` | Serializes loop state and persists via `saveCkpt` |
