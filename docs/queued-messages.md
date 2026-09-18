# Queued messages

**Status:** Implemented  **Feature:** Per-session message buffering for user replies during active turns

Queued messages allow users to send additional messages to Nine while a session is actively processing a turn. These messages are **not automatically included** in the model's context, preventing them from interfering with the current turn's reasoning. Instead, they are held in a separate queue and the model is notified of their existence via a system message.

## Overview

When a user sends a message while Nine is busy (e.g., waiting for an LLM response or tool execution), that message is added to a queue rather than being appended to the conversation history immediately. This design ensures:

1. **No context pollution** - Queued messages don't appear in the model's context until explicitly consumed
2. **No message duplication** - Messages exist in either the queue OR the conversation history, never both
3. **Model control** - The model decides when and which messages to process
4. **Hybrid consumption** - The model can process some messages and leave others in the queue

## How it works

### Message flow

```mermaid
flowchart TD
    subgraph User["User Input"]
        U1[Send message during idle]
        U2[Send message during busy turn]
    end

    subgraph Queue["Queue System"]
        Q1[queued_messages column\nin conversations table]
        Q2[Store as []QueuedMessage\nwith consumed status]
    end

    subgraph Model["Model Context"]
        M1[History: previous messages]
        M2[System message:\n"There are N queued messages"]
        M3[queued_messages_get tool]
        M4[queued_message_mark_consumed tool]
    end

    subgraph History["Conversation History"]
        H1[Consumed messages\nmoved here]
    end

    U1 --> M1
    U2 --> Q1
    Q1 --> Q2
    Q2 --> M2
    M2 --> M3
    M3 --> M4
    M4 --> H1
    H1 --> M1

    style User fill:#3a1a1a,color:#fff
    style Queue fill:#1e3a5f,color:#fff
    style Model fill:#1e3a1e,color:#fff
    style History fill:#3a1a3a,color:#fff
```

### Turn lifecycle with queued messages

1. **User sends message during busy turn** > Message is added to the queue (not to history)
2. **Next turn starts** > System message includes: `"[System: There are N queued messages from the user. Use queued_messages_get to read them.]"`
3. **Model calls `queued_messages_get`** > Returns all queued messages with their consumption status
4. **Model processes messages** > Can read, analyze, and decide which to consume
5. **Model marks messages as consumed** > Using `queued_message_mark_consumed` for individual messages or `queued_messages_mark_all_consumed` for all
6. **Consumed messages moved to history** > Messages are appended to conversation history as user messages
7. **Next turn** > Only unconsumed messages remain in queue; consumed messages now appear in context

## Tools

### `queued_messages_get`

Read all messages currently queued for this session.

**Returns:** JSON object with `messages` array, where each message has:
- `text` - The message content
- `consumed` - Boolean indicating if the message has been processed

**Example output:**
```json
{
  "messages": [
    {"text": "Please also check the logs", "consumed": false},
    {"text": "Focus on the API endpoint", "consumed": false},
    {"text": "Ignore the frontend", "consumed": true}
  ]
}
```

**Behavior:**
- Non-destructive - messages remain in queue
- Can be called multiple times
- Returns the current consumption state of each message

### `queued_message_mark_consumed`

Mark a specific queued message as consumed by its index.

**Arguments:**
- `index` (required, integer) - 0-based index of the message to mark as consumed

**Returns:** Confirmation string with the consumed message summary

**Example:**
```json
{"index": 0}
```

**Behavior:**
- Marks the message at the given index as consumed
- Moves the message to conversation history as a user message
- The message will appear in future context
- The message is removed from the queue
- Indexes shift after consumption - calling this may change the indices of remaining messages

### `queued_messages_mark_all_consumed`

Mark all queued messages as consumed.

**Returns:** String indicating the number of messages consumed

**Behavior:**
- Marks all unconsumed messages as consumed
- Moves all messages to conversation history
- All messages will appear in future context
- Queue is cleared

### `queued_messages_count`

Get the total number of messages currently queued (both consumed and unconsumed).

**Returns:** Integer count

### `queued_messages_unconsumed_count`

Get the number of unconsumed messages currently queued.

**Returns:** Integer count

## Usage patterns

### Pattern 1: process all messages

```
User: Do the analysis
Nine: [thinking...]
User: Also check the database
User: And verify the cache
Nine: [finishes thinking, starts turn]
      [System: There are 2 queued messages from the user. Use queued_messages_get to read them.]
      queued_messages_get
      queued_messages_mark_all_consumed
      [Now processes all messages together]
```

### Pattern 2: selective processing

```
User: Do the analysis
Nine: [thinking...]
User: Also check the database
User: And verify the cache
User: Ignore the frontend issues
Nine: [finishes thinking, starts turn]
      [System: There are 3 queued messages from the user. Use queued_messages_get to read them.]
      queued_messages_get
      [Sees 3 messages, decides to process only the first two]
      queued_message_mark_consumed(index: 0)
      queued_message_mark_consumed(index: 1)
      [Processes database and cache requests, ignores frontend]
```

### Pattern 3: deferred processing

```
User: Do the analysis
Nine: [thinking...]
User: Also check the database
User: And verify the cache
Nine: [finishes thinking, starts turn]
      [System: There are 2 queued messages from the user. Use queued_messages_get to read them.]
      queued_messages_get
      [Sees messages but decides to finish current task first]
      [Completes current analysis without consuming queued messages]
      
      [Next turn starts]
      [System: There are 2 queued messages from the user. Use queued_messages_get to read them.]
      queued_messages_get
      [Now processes the queued messages]
```

## Data structure

### Conversation table

The `conversations` table has a `queued_messages` column (TEXT, default `[]`) that stores an array of queued messages:

```json
[
  {"text": "message 1", "consumed": false},
  {"text": "message 2", "consumed": false},
  {"text": "message 3", "consumed": true}
]
```

### QueuedMessage type

```go
type QueuedMessage struct {
    Text      string `json:"text"`
    Consumed bool   `json:"consumed"`
}
```

## Implementation details

### Database schema

The `queued_messages` column was added in schema version 10 (migration step 9  10).

**Migration:** `conversations_queued_messages` - adds the column with default `[]`

### Context builder integration

When `QueuedMessagesCount > 0`, the context builder appends to the system prompt:

```
[System: There are N queued messages from the user. Use queued_messages_get to read them.]
```

This notification appears in **every turn** where there are unconsumed queued messages.

### Message consumption flow

1. Model calls `queued_message_mark_consumed` with a specific index
2. Store retrieves the queued messages
3. Store marks the message at that index as consumed
4. Store saves the updated queue
5. Store retrieves the current conversation history
6. Store appends the consumed message to history as a user message
7. Store updates the conversation history
8. Return confirmation to model

### Cleanup

The system does not automatically clean up consumed messages from the queue. This is intentional:

- Consumed messages remain in the queue with `consumed: true` until explicitly removed
- This allows the model to reference them if needed
- The `queued_messages_get` tool returns the full state including consumed status
- Future cleanup can be added as a maintenance operation

## Configuration

No configuration is required. The queued messages feature is always available when the daemon is running.

## Limits

1. **No automatic consumption** - Messages are never automatically moved to history
2. **No TUI/CLI/API integration yet** - Currently only the model can access queued messages through tools
3. **No size limits** - There is currently no limit on the number or size of queued messages
4. **No expiration** - Queued messages do not expire automatically

## Future enhancements

- CLI command to view queued messages for a session
- TUI indicator showing queued message count
- API endpoint to manage queued messages
- Configuration options for queued message behavior
- Automatic cleanup of fully-consumed queues

## See also

- [Agent loop](agent-loop.md) - How turns are processed
- [Context builder](context-builder.md) - How context is assembled
- [Memory store contract](../spec/contracts/memory-store.md) - Database schema and operations
