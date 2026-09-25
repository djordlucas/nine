# AgentWorker

An `AgentWorker` wraps a single `agent.Loop` and serializes user turns for one
conversation or background session. Each worker owns a goroutine that processes
turns one at a time from an inbox channel.

## Component diagram

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

## Turn lifecycle

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

## End-of-turn hooks

After `agent.Loop.Run` returns, the worker runs four hooks in order, and the
order matters: a stall has to be seen before the routines are notified, because
`OnTurnEnd` is how a pursue routine learns to pause its goal.

1. **Stall detection** — see below.
2. **Routine `OnTurnEnd`** — every active routine on the session plan, which is
   where a goal's status is synced ([session-plans.md](session-plans.md)).
3. **Checkpoint** — serialize loop state and persist it.
4. **Re-arm the idle scheduler** — compute the next wake across the session's
   idle-capable routines.

A turn also carries two things on its context rather than taking them from boot
configuration, because the sandboxed-tool host is daemon-wide: the journal
destination for a tool's outbound HTTP, and the conversation a
`scope = "conversation"` state grant resolves against.

Queued messages are drained here too — a message sent while the worker was busy
is folded into the next turn rather than starting one
([queued-messages.md](queued-messages.md)).

## Stall detection

The AgentWorker tracks consecutive turns where `agent.Loop.LastRunToolCount() == 0`. When `stallN` reaches `StallConfig.Limit`, `OnStall` fires and the counter resets.

```
turn completes
  └─ tools used > 0 → stallN = 0
  └─ tools used == 0 → stallN++
       └─ stallN >= Limit → OnStall(agentID), stallN = 0
```

Stall detection is disabled when `Limit == 0` or `OnStall == nil`.

## Concurrency model

- The inbox channel has capacity 1. The daemon blocks sending a new turn until the current one finishes, making turns strictly sequential per worker.
- `progressFn` is guarded by a mutex so the daemon goroutine can register/clear it while the AgentWorker goroutine calls it.
- `stop()` closes the inbox and blocks on `<-stopped`, ensuring the goroutine drains cleanly before the worker is discarded.

## Key methods

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
| `setDrainQueued` | Installs the hook that folds queued messages into the next turn |

## Limits

| Limit | Detail |
|-------|--------|
| Stall detection is heuristic | A stall is counted as consecutive turns using no tools. A session legitimately reasoning without tools across several turns looks identical to a wedged one. |
| Stall detection is off by default in tests | `Limit == 0` or `OnStall == nil` disables it entirely. |
| Checkpoint granularity is one turn | State is serialized after a turn completes. A daemon killed mid-turn resumes from the previous turn and loses that turn's scratchpad. |
| Progress buffer can drop | `progressCh` is buffered at 256 events. A turn emitting faster than the client consumes can overflow it. |
| The replay ring is bounded | Each worker keeps the last 200 tool and sub-agent events for a reattaching client and sends at most 50. A longer disconnection loses the earliest of them, and `response_chunk` events are never buffered, so the text stream itself is not replayed. |
| No per-turn timeout | The worker blocks on `agent.Loop.Run` for as long as the loop takes. Bounding a turn is the loop's and the LLM queue's job, not the worker's. |
