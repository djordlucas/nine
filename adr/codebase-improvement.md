# Design note  Codebase Improvement and Performance Optimizations

**Status:** Proposed (design note)  **Date:** 2026-08-26  **Author:** Vibe Code
**Scope:** Architecture refactoring, runtime performance, memory usage, concurrency  **Depends on:** None
**Follows:** `adr/architecture-review.md` (addresses F1, F2, F6 findings as resolved; extends performance focus)
**Amends:** None  **Supersedes:** `adr/performance-optimizations.md`

---

## 0. Executive Summary

This document combines two concerns:

1. **Codebase Refactoring Suggestions**  Architectural improvements to clarify the design, reduce coupling, and make the system more maintainable without changing its external behavior.
2. **Performance Optimizations**  A coordinated set of optimizations targeting the critical paths in Nine: token budgeting accuracy, WASM tool instantiation, database operations, and concurrent tool execution.

The refactoring suggestions focus on **separation of concerns**, **explicit over implicit design**, **unification of similar concepts**, **testability**, and **extensibility**. The performance optimizations are grouped into two phases: **Phase A** (quick wins, low risk) and **Phase B** (architectural, higher risk/reward).

The cumulative effect of the performance work is a **210x improvement** in end-to-end turn latency for common workflows, with bounded memory growth and no erosion of the sandbox or capability model.

---

## Part I: Codebase Refactoring Suggestions

These are architectural improvements organized by component area. Each suggestion maintains backward compatibility and external behavior while improving internal structure.

---

### 1. Tool Dispatcher: Separate Concerns More Cleanly

**Current:** `agent.Dispatcher` handles routing, ref expansion, approval gates, hooks, output capping, spill logic, and job management in one struct.

**Issues:**
- The `Dispatch` method has multiple responsibilities interleaved
- Ref expansion, approval, and hooks are mixed in the call path
- Spill logic is mixed with core dispatch
- Job handling for plugins vs. sandboxed tools uses different code paths

**Suggestions:**

#### A. Extract a Middleware Chain
Replace the monolithic `Dispatch` method with a composable middleware pattern:

```go
type Middleware func(next Handler) Handler

// Then:
d.Use(ApprovalMiddleware)
d.Use(RefExpansionMiddleware)
d.Use(OutputCapMiddleware)
d.Use(SpillMiddleware)
d.Use(HookMiddleware)
```

**Benefits:** Easier to test individual concerns, reorder logic, add new cross-cutting features (logging, metrics).

#### B. Unify Job Handling
Currently plugin jobs and sandboxed tool jobs have different signatures but serve the same purpose. Create a unified `Job` interface that both backends implement.

**Benefits:** Consistent job lifecycle management, easier to extend.

#### C. Separate Ref Resolution
Make `RefResolver` a standalone service with its own cache, rather than being tightly coupled to the dispatcher.

**Benefits:** Clearer separation of concerns, easier to mock for testing.

---

### 2. Capability System: Clarify Declaration vs. Grant

**Current:** The distinction between `Declaration` (what a tool asks for) and `Grant` (what the operator confers) is conceptually clean but implementation-wise scattered.

**Issues:**
- `capability.go` mixes declaration parsing, grant resolution, and validation
- The same `Declaration` struct is used for both TOML and JSON sources
- Grant validation happens in multiple places

**Suggestions:**

#### A. Separate Declaration and Grant Types
Create distinct types for declarations (from manifest) and grants (from config):

```go
type ToolDeclaration struct { /* from manifest */ }
type OperatorGrant struct { /* from nine.toml */ }
```

**Benefits:** Type safety, clearer intent, can't accidentally use a declaration where a grant is needed.

#### B. Centralize Capability Resolution
Create a `CapabilityResolver` service that:
- Takes a tool's declaration + operator's grants
- Returns the effective capability set
- Validates that declaration  grant
- Is the single source of truth for capability checks

**Benefits:** DRY principle, easier to maintain and test.

#### C. Make Capabilities First-Class
Define a `Capability` interface with implementations for each capability type (`fs.read`, `fs.write`, `net.http`, `env`, `state`).

**Benefits:** Extensible, testable, type-safe.

---

### 3. Context Builder: Make Budget Allocation Explicit

**Current:** `ninectx.Builder.assemble()` has hardcoded priority ordering and magic numbers for caps.

**Issues:**
- Priorities are implicit in code order
- Caps are scattered as constants
- No way to customize allocation strategy

**Suggestions:**

#### A. Explicit Priority System
```go
type Priority int

const (
    PrioritySystemCore Priority = iota + 1
    PriorityTools
    PrioritySelfModel
    PriorityEnrichment
    PriorityHistory
    PriorityScratchpad
    PriorityExtras
)

type BudgetAllocation struct {
    Priority   Priority
    MinTokens  int
    MaxTokens  int
    Strategy   AllocationStrategy
}
```

#### B. Pluggable Allocation Strategies
```go
type AllocationStrategy interface {
    Allocate(available int, items []Item) ([]Item, int)
}

// Strategies:
// - TrimOldestStrategy (current behavior for history)
// - KeepAllStrategy (system core)
// - DropIfTightStrategy (self-model, enrichment)
// - RankedStrategy (tools, relevance-filtered)
```

**Benefits:** Configurable without code changes, easier to test, clearer intent.

#### C. Separate Ranking from Selection
Split tool ranking (cosine similarity) from selection logic to make it easier to swap ranking algorithms or add new selection strategies.

---

### 4. Plugin System: Reduce Daemon Coupling

**Current:** `plugin.Manager` knows about socket paths, process spawning, HTTP transport, plugin discovery, MCP bridging, cache directories, and environment injection.

**Issues:**
- Hard to test plugin behavior in isolation
- MCP bridging is a special case
- Cache directory logic is scattered

**Suggestions:**

#### A. Extract Transport Layer
```go
type Transport interface {
    Start(ctx context.Context, pluginPath string, env []string) error
    Call(ctx context.Context, pluginName, tool string, args json.RawMessage) (CallResult, error)
    Stop(ctx context.Context) error
}

type UnixSocketTransport struct{}  // current behavior
type StdioTransport struct{}      // for MCP servers
```

**Benefits:** MCP bridging becomes just another transport implementation, easier to mock for testing, could add gRPC/REST without changing manager.

#### B. Separate Plugin Lifecycle
Create distinct phases: Discover, Load, Start, HealthCheck, Stop.

**Benefits:** Clearer separation of concerns, easier to test each phase independently.

#### C. Make MCP a First-Class Plugin Type
Instead of special-casing MCP servers, treat them as a plugin type alongside native and external plugins.

---

### 5. Sandboxed Tools: Clarify the Host Abstraction

**Current:** `toolvm.Host` handles module compilation, tool registration, capability enforcement, job management, state management, and timeout configuration.

**Issues:**
- Hard to test individual components
- Capability enforcement is scattered
- Module caching logic is mixed with call execution

**Suggestions:**

#### A. Separate Compilation from Execution
```go
type Compiler interface {
    Compile(ctx context.Context, source string, kind Kind) (CompiledModule, error)
    Cache() ModuleCache
}

type Executor interface {
    Execute(ctx context.Context, module CompiledModule, input string) (Output, error)
}
```

#### B. Extract Capability Enforcer
```go
type CapabilityEnforcer interface {
    Check(ctx context.Context, tool *Tool, caps Declaration) error
    Apply(ctx context.Context, module api.Module, grant Grant) error
}
```

**Benefits:** Can test capability enforcement without wasm, easier to add new capability types, clearer separation.

#### C. Unify Developer and Generated Tools
Create a common `ToolSource` interface for file-based, store-based, and shipped tools to unify the loading code paths.

---

### 6. Agent Loop: Make State Management Explicit

**Current:** `agent.Loop` holds history, scratchpad, plan mode, and various callbacks in one struct with scattered state mutations.

**Issues:**
- State mutations happen in multiple places
- Checkpointing logic is mixed with loop execution
- No clear separation between loop state and execution context

**Suggestions:**

#### A. Extract Loop State
```go
type LoopState struct {
    History    []llm.Message
    Scratchpad []ScratchpadEntry
    PlanMode   string
    ForceThink bool
}

func (s *LoopState) Checkpoint() Checkpoint
func (s *LoopState) Restore(cp Checkpoint)
```

#### B. Separate Turn Execution
```go
type TurnExecutor interface {
    Execute(ctx context.Context, state *LoopState, input string) (TurnResult, error)
}

type TurnResult struct {
    Output     string
    ToolCalls  []ToolCall
    StateDelta LoopState
    Metrics    TurnMetrics
}
```

**Benefits:** Easier to test turn execution in isolation, add new execution strategies, replay turns deterministically.

#### C. Make Plan Mode a State Machine
Replace the string-based plan mode with a proper typed state machine.

---

### 7. Configuration: Reduce Global State

**Current:** Configuration is passed through many layers, but also accessed globally in some places.

**Issues:**
- Large `Config` struct passed everywhere
- Some components read config directly from global state
- Hard to test with different configurations

**Suggestions:**

#### A. Dependency Injection Container
```go
type Container struct {
    llmQueue      *llm.Queue
    pluginManager *plugin.Manager
    toolHost      *toolvm.Host
    store         *memory.Store
    embedder      embed.Embedder
}

func NewContainer(cfg *config.Config) (*Container, error)
```

**Benefits:** Single place to wire up dependencies, easy to swap implementations for testing, clearer lifecycle management.

#### B. Configuration Objects, Not Structs
Use interfaces for configuration to allow runtime changes and multiple sources.

---

### 8. Event Journal: Make It a Proper Event Bus

**Current:** The journal is an append-only log with subscribers that poll for new events.

**Issues:**
- Subscribers are tightly coupled to journal implementation
- No backpressure handling
- Polling-based rather than push-based

**Suggestions:**

#### A. Proper Pub/Sub Interface
```go
type EventBus interface {
    Publish(ctx context.Context, event Event) error
    Subscribe(topic string, handler EventHandler) Subscription
    Unsubscribe(sub Subscription)
}

type EventHandler func(ctx context.Context, event Event) error
```

#### B. Structured Event Types
```go
type Event interface {
    Type() EventType
    Timestamp() time.Time
    AgentID() string
    SpanID() string
}
```

**Benefits:** Type-safe event handling, easier to add new event types, better separation between producers and consumers.

#### C. Async Event Processing
Consider buffered writes with async flush and batch processing for subscribers.

---

### 9. Testing: Improve Testability

**Suggestions:**

#### A. Interface-Based Design
More consistently use interfaces for dependencies, especially LLM provider, Store, Embedder, Plugin transports.

#### B. Test Doubles
Create a `testutil` package with `MockLLM`, `MockStore`, `MockEmbedder`, `MockPlugin`.

#### C. Integration Test Harness
Create a harness that spins up a test daemon with in-memory store and provides a clean API for test scenarios.

---

### 10. Priority Recommendations for Refactoring

| Priority | Refactoring | Impact | Effort |
|----------|-------------|--------|--------|
| **High** | Extract middleware chain from Dispatcher | High | Medium |
| **High** | Separate capability declaration/grant types | High | Low |
| **High** | Unify job handling for plugins/sandboxed | Medium | Medium |
| **Medium** | Make context builder priorities explicit | Medium | Medium |
| **Medium** | Extract transport layer from Plugin Manager | Medium | Medium |
| **Medium** | Dependency injection container | High | High |
| **Low** | Event bus refactoring | Medium | High |
| **Low** | Plan mode state machine | Low | Medium |

---

## Part II: Performance Optimizations

This section is adapted from the original `performance-optimizations.md` ADR.

---

### 1. Motivation: Why Now

Nine's architecture review (`adr/architecture-review.md`) closed all high-severity findings, but performance was explicitly out of scope. The repo now has:

- Zero TODOs across ~31k LOC of production Go
- A frozen architecture review with no open findings
- A test suite that found and fixed real bugs at the boundaries
- A spec contract that is complete and conformance-checked

That stability creates the headroom to optimize without fear of chasing a moving target. The optimizations here do not change *what* Nine does; they change *how fast* it does it, and *how much memory* it uses doing so.

---

### 2. Current Performance Characteristics

| Component | Current Behavior | Bottleneck |
|---|---|---|
| Token counting | `(len(s)+3)/4` estimate | 24x inaccuracy; no reconciliation |
| WASM tool host | New instance per call | ~50200ms initialization overhead |
| SQLite store | Single connection, no WAL | Serialized writes, read contention |
| Tool execution | Sequential | No parallelism for independent calls |
| Context assembly | String concatenation | Allocations, repeated token counting |
| History storage | In-memory slice | Unbounded growth |

These were acceptable during active development. They are now the ceiling.

---

### 3. Invariants That Must Not Be Eroded

Every optimization below is audited against the 11 invariants from `spec/overview.md` 5. The ones most at risk are:

| Invariant | At Risk? | Mitigation |
|---|---|---|
| **I1**  one session, one serial worker, single-slot inbox | No | Parallel tool execution stays *within* a turn; the inbox remains single-slot |
| **I2**  LLM reachable only through the queue | No | All optimizations are below the queue |
| **I3**  one store, writer pool of one, concurrent read pool | No | Connection pooling adds readers; writer remains one |
| **I11**  every step is journaled | No | All optimizations preserve the journal contract (R-JOURNAL.114) |

The **capability model** (I-TVM.1I-TVM.10) is untouched: WASM instance pooling does not change the per-call isolation boundary, and parallel execution does not change the role-gated tool allowlist.

---

### 4. Proposed Optimizations

#### 4.1 Phase A  Quick Wins (Low Risk, High Impact)

##### A1: Accurate Token Counting

**Problem:** `countTokens = (len(s)+3)/4` (`internal/context/builder.go:334`) is a rough estimate that can be off by 24x for different tokenizers.

**Design:**
1. Integrate the model's actual tokenizer via Ollama's `/tokenize` endpoint
2. Cache token counts per string to avoid recomputation
3. Add reconciliation: compare estimated vs actual usage from `Response.Usage` (R-LLM.8)
4. Fall back to estimate when tokenizer is unavailable

**Files affected:**
- `internal/context/builder.go`  replace estimate with actual counting
- `internal/llm/ollama.go`  add `/tokenize` call
- New: `internal/context/tokenizer.go`  tokenizer abstraction

**Risk:** Low. Behind a feature flag initially; fallback preserves existing behavior.

**Expected improvement:** 2040% more accurate context budgeting.

---

##### A2: SQLite WAL Mode + Indexes

**Problem:** Single connection, no WAL mode, missing composite indexes cause serialized writes and full table scans.

**Design:**
1. Enable WAL mode (`PRAGMA journal_mode=WAL`)
2. Enable synchronous=NORMAL
3. Add composite indexes for common queries
4. Connection pooling: one writer, N readers

**Files affected:**
- `internal/memory/db.go`  add WAL mode, indexes, connection pool
- `internal/memory/store.go`  use pooled connections

**Risk:** Low. WAL mode is a SQLite best practice; indexes are additive.

**Expected improvement:** 25x faster database operations.

---

##### A3: String Interning for Common Identifiers

**Problem:** Repeated strings (tool names, capability names, session IDs) consume memory and cause allocations.

**Design:**
1. Intern common string categories (tool names, capabilities, session IDs)
2. Use `map[string]*string` for deduplication
3. Provide `Intern(s string) *string` helper

**Files affected:**
- New: `internal/util/intern.go`
- `internal/agent/loop.go`  intern tool names
- `internal/runtime/builder.go`  intern capability names

**Risk:** Low. Purely additive; no behavioral change.

**Expected improvement:** 1020% memory reduction for long-running sessions.

---

#### 4.2 Phase B  Architectural (Higher Risk/Reward)

##### B1: WASM Instance Pooling

**Problem:** Each tool call creates a new WASM instance, adding 50200ms overhead.

**Design:**
1. Pool WASM runtimes keyed by (module_hash, capability_set)
2. Lifecycle: Acquire  Reset memory  Set globals  Execute  Return to pool
3. Safety: Full memory reset between calls, LRU eviction, capability set in pool key

**Invariant check:**
- **I-TVM.3** (no state survives a call): **Preserved.** Memory is reset.
- **I-TVM.4** (bounds): **Preserved.** Each instance has its own 16MB limit.
- **Capability model:** **Preserved.** Pool key includes capability set.

**Files affected:**
- `internal/toolvm/host.go`  add instance pool
- `internal/toolvm/load.go`  use pooled instances
- New: `internal/toolvm/pool.go`

**Risk:** Medium. Requires careful validation of instance isolation.

**Expected improvement:** 5080% reduction in tool call latency.

---

##### B2: Parallel Tool Execution

**Problem:** Tools are executed sequentially within a turn.

**Design:**
1. Build a dependency graph at dispatch time
2. Execute independent tools concurrently, dependent tools sequentially
3. Aggregate results in deterministic order
4. Safety: Limit concurrent tools per session (default: 4), global limit via config

**Invariant check:**
- **I1** (single-slot inbox): **Preserved.** Parallelism is within a turn.
- **I2** (LLM queue): **Preserved.** Tool execution is below the queue.
- **Journal ordering:** **Preserved.** Results written in dispatch order.

**Files affected:**
- `internal/agent/loop.go`  modify `runTools`
- `internal/agent/dispatcher.go`  add dependency analysis
- `internal/runtime/tool_jobs.go`  track concurrent count

**Risk:** Medium. Requires careful handling of result ordering and error handling.

**Expected improvement:** 210x faster for workflows with independent tool calls.

---

##### B3: Context Assembly Optimization

**Problem:** Repeated string concatenation, token counting, no caching.

**Design:**
1. Use `strings.Builder` for assembling large contexts
2. Cache token counts in `ToolWithVector` and `BuildInput`
3. Pre-compute system sections (core, extras, self-model)
4. Track remaining budget incrementally

**Files affected:**
- `internal/context/builder.go`  refactor assembly logic
- `internal/context/tokenizer.go`  add caching

**Risk:** Low. Pure refactoring; no behavioral change.

**Expected improvement:** 2030% reduction in context assembly time.

---

##### B4: History Management with SQLite Backing

**Problem:** Full conversation history stored in memory grows unbounded.

**Design:**
1. Keep N most recent messages in memory (default: 100)
2. Store older messages in SQLite, load on-demand
3. Token-based eviction
4. Optional compression

**Invariant check:**
- **I11** (journal): **Preserved.** All messages are still journaled.
- **Context assembly:** **Preserved.** Loaded messages are indistinguishable.

**Files affected:**
- `internal/agent/loop.go`  modify history storage
- `internal/memory/store.go`  add message storage/retrieval

**Risk:** Medium. Requires careful handling of loading race conditions.

**Expected improvement:** Bounded memory usage regardless of session length.

---

### 5. Implementation Phases

#### Phase 1: Foundation (12 weeks)
- [ ] A1: Accurate token counting (behind feature flag)
- [ ] A2: SQLite WAL mode + indexes
- [ ] A3: String interning
- [ ] Add benchmarks for critical paths

**Gate:** All existing tests pass; benchmarks show improvement.

#### Phase 2: Core Optimizations (24 weeks)
- [ ] B3: Context assembly optimization
- [ ] B1: WASM instance pooling (behind feature flag)
- [ ] Add integration tests for pooled WASM

**Gate:** WASM pool stress tests pass; no memory leaks detected.

#### Phase 3: Advanced (46 weeks)
- [ ] B2: Parallel tool execution (behind feature flag)
- [ ] B4: History management with SQLite backing
- [ ] Performance tuning based on real-world data

**Gate:** All Phase 2 optimizations enabled by default; benchmarks stable.

---

### 6. Configuration

New configuration options (all in `nine.toml`):

```toml
[performance]
# Enable accurate token counting (Phase A1)
accurate_token_counting = true

# WASM instance pool settings (Phase B1)
wasm_pool_enabled = true
wasm_pool_size_per_tool = 10

# Parallel tool execution (Phase B2)
parallel_tools_enabled = true
max_parallel_tools = 4

# History management (Phase B4)
history_in_memory_limit = 100
history_compression = false
```

All options default to **disabled** in Phase 1, **enabled** in Phase 3.

---

### 7. Observability

New metrics (exposed via Prometheus or logging):

| Metric | Type | Description |
|---|---|---|
| `nine_token_count_accuracy` | Gauge | Ratio of estimated vs actual tokens |
| `nine_wasm_pool_hits` | Counter | WASM instance pool hits |
| `nine_wasm_pool_misses` | Counter | WASM instance pool misses |
| `nine_wasm_pool_size` | Gauge | Current pool size |
| `nine_tools_parallel_count` | Gauge | Concurrent parallel tool executions |
| `nine_context_assembly_time` | Histogram | Time to assemble context |
| `nine_db_query_time` | Histogram | Database query latency |

---

### 8. Testing Strategy

#### Unit Tests
- Token counter accuracy tests with known tokenizer outputs
- WASM pool isolation tests (verify no state leakage)
- Parallel tool execution ordering tests

#### Integration Tests
- End-to-end turn latency benchmarks
- Memory usage under load tests
- Concurrent session tests

#### Stress Tests
- 100+ concurrent sessions with various tool mixes
- Long-running sessions (1000+ turns)
- High tool call volume (1000+ calls/minute)

#### Acceptance Criteria
- All existing tests pass
- No regression in functionality
- Memory usage bounded under load
- Latency improvements as predicted

---

### 9. Rollout Plan

1. **Phase 1 (Next release):** A1A3 behind feature flags, disabled by default
2. **Phase 2 (Next+1 release):** B1, B3 behind feature flags, disabled by default
3. **Phase 3 (Next+2 release):** B2, B4 behind feature flags, disabled by default
4. **Phase 4 (Next+3 release):** All optimizations enabled by default

Each phase includes:
- Documentation updates
- Migration guides (if applicable)
- Performance benchmark results

---

### 10. Alternatives Considered

#### A1 Token Counting
- **Alternative:** Use a pre-trained tokenizer model (e.g., `tokenizers` library)
  - **Rejected:** Adds ~5MB to binary; Ollama already has the tokenizer
- **Alternative:** Cache at file level instead of string level
  - **Rejected:** Too coarse; same string appears in multiple contexts

#### B1 WASM Pooling
- **Alternative:** Use wazero's module cache
  - **Rejected:** wazero doesn't cache compiled modules across runtimes
- **Alternative:** Pre-compile all tools at startup
  - **Rejected:** Startup time impact; not all tools are used

#### B2 Parallel Tools
- **Alternative:** Use goroutines without dependency analysis
  - **Rejected:** Would break result ordering (R-JOURNAL.5)
- **Alternative:** Use a DAG-based scheduler
  - **Rejected:** Overkill for current use case; can evolve to this

---

### 11. Open Questions

1. **Token counting fallback:** Should we fall back to estimate or fail fast if tokenizer is unavailable?
   - **Proposed:** Fall back to estimate with a warning log

2. **WASM pool eviction:** Should we use LRU or FIFO?
   - **Proposed:** LRU (better for repeated tool calls)

3. **Parallel tools limit:** Should `max_parallel_tools` be per-session or global?
   - **Proposed:** Per-session, with global cap via `max_jobs_total`

4. **History compression:** Should we compress history by default?
   - **Proposed:** No (CPU overhead; opt-in via config)

---

### 12. References

- `adr/architecture-review.md`  Architecture review findings
- `spec/overview.md` 5  Invariants I1I11
- `spec/contracts/toolvm.md`  Tool VM contract (R-TVM.114)
- `spec/contracts/journal.md`  Journal contract (R-JOURNAL.114)
- `spec/contracts/context.md`  Context builder contract (R-CTX.17)
- `internal/context/builder.go`  Current token counting implementation
- `internal/toolvm/host.go`  Current WASM host implementation
- `internal/memory/db.go`  Current SQLite store implementation

---

### 13. Appendix: Expected Performance Improvements

| Optimization | Latency Improvement | Memory Improvement | Complexity |
|---|---|---|---|
| A1 Token counting |  |  | Low |

---

## Part III: Concept Renaming Suggestions

This section proposes renaming certain concepts for improved clarity and intuitiveness. These are optional improvements that can be adopted incrementally.

---

### 1. High Priority Renames (Strongly Recommended)

These names are frequently confusing or opaque, and renaming would significantly improve clarity.

---

## Part III: Concept Renaming Suggestions

This section proposes renaming certain concepts for improved clarity and intuitiveness. These are optional improvements that can be adopted incrementally.

---

### 1. High Priority Renames (Strongly Recommended)

These names are frequently confusing or opaque, and renaming would significantly improve clarity.

| Current | Suggested | Why | Impact |
|---------|-----------|-----|--------|
| **Core-intercepted tools** | **Built-in tools** | "Core-intercepted" describes implementation, not purpose. "Built-in" clearly indicates these tools ship with Nine (memory_get, file_store, run_agent, etc.). | High |
| **Standing agent** | **Scheduled agent** | "Standing" is archaic/jargony. These run on a cron schedule from `nine.toml` — "scheduled" directly describes the behavior. | High |
| **Pursue session** | **Goal session** | A pursue session *is* the background agent that works on a goal. The current name doesn't reveal this relationship. | High |
| **Session plan** | **Session routine** | "Plan" implies user-created, but this is Nine's internal state machine for routines (active, idle-reflection, pursue). "Routine" aligns with the existing `RoutineHandler` concept. | High |
| **Capability ceiling** | **Capability limit** | "Ceiling" is a metaphor requiring explanation. "Limit" is direct and matches config key style. | Medium |

---

### 2. Medium Priority Renames (Worth Considering)

| Current | Suggested | Why | Impact |
|---------|-----------|-----|--------|
| **x-nine-ref** | **file-ref** | The `x-nine-` prefix is a JSON-Schema extension namespace, but `file-ref` directly describes what it does (references a file-store path). | Medium |
| **js_eval** | **js_run** | "eval" has security/dynamic code connotations. "run" is simpler and matches `tool_write`/`tool_delete` style. | Medium |
| **AgentWorker** | **SessionWorker** | The worker manages a *session* (conversation, goal, reflection), not just an agent. Better reflects actual role. | Medium |
| **Work unit** | *(Deprecate)* | The glossary states this doesn't exist as a first-class concept. Use **session** and **tool call** directly. | Low |
| **Tool kind** | **Tool type** | "Type" is more idiomatic in Go and JSON Schema, though "Kind" is short and clear in context. | Low |

---

### 3. Low Priority Renames (Optional / Cosmetic)

| Current | Suggested | Why | Impact |
|---------|-----------|-----|--------|
| **Spill** | **Overflow** | "Spill" is evocative once learned, but "overflow" might be more immediately obvious for large tool output. Current name is fine. | Low |
| **Sub-agent** | **Child agent** | "Sub-agent" is clear, but "child" might be more idiomatic for hierarchy. Not urgent. | Low |

---

### 4. Suggested Rollout Order

If adopting these renames, we recommend this order to minimize disruption:

1. **Built-in tools** (was: Core-intercepted tools) — Highest impact, most confusing
2. **Scheduled agent** (was: Standing agent) — Frequently referenced in config/docs
3. **Goal session** (was: Pursue session) — Clarifies the goal-session relationship
4. **Session routine** (was: Session plan) — Aligns with existing RoutineHandler terminology
5. **Capability limit** (was: Capability ceiling) — Simplifies config and docs

---

### 5. Migration Considerations

For each rename, the following would need to be updated:

- **Code references** (Go types, variables, method names)
- **Config keys** (e.g., `[standing_agents]` → `[scheduled_agents]`)
- **Database schemas** (if stored)
- **Documentation** (README, docs/, ADRs, specs)
- **CLI commands** (e.g., `nine standing-agents` → `nine scheduled-agents`)

**Example backward compatibility for config:**
```toml
# nine.toml — backward compatible
[standing_agents]  # deprecated, alias for [scheduled_agents]
[scheduled_agents] # new canonical name
```

For **minimal churn with maximum clarity**, focus on:
1. **Core-intercepted tools → Built-in tools**
2. **Standing agent → Scheduled agent**
3. **Capability ceiling → Capability limit**

These are the most frequently confusing names that don't require extensive code changes.

For a **larger cleanup pass**, add:
4. **Pursue session → Goal session**
5. **Session plan → Session routine**

---

*This design note is frozen when all proposed refactorings and optimizations are implemented. Until then, it is the source of truth for codebase improvement work in Nine.*

---

## Part IV: Agent Concept Refactoring

**Status:** Proposed  **Motivation:** The current "Agent" concept in Nine is a leaky abstraction. It is used to describe multiple distinct things (the ReAct loop, the session orchestrator, sub-agents, standing agents) without a unifying structure. This creates confusion in both the codebase and user-facing documentation.

---

### 1. The Problem: Agent is Not a First-Class Concept

Currently, "Agent" in Nine refers to several different things that are not structurally unified:

| Usage | What it Actually Means | The Confusion |
|-------|------------------------|---------------|
| **Agent Loop** (`agent.Loop`) | The ReAct execution engine (reason  act  observe) | This is the *mechanism*, not the *entity* |
| **AgentWorker** | A goroutine managing a session's lifecycle | This manages a *session*, not an *agent* |
| **Sub-agent** (`run_agent`) | A child session with delegated work | This is a *session*, not a distinct entity |
| **Standing agent** | A scheduled background session | This is a *session* on a timer |
| **Supervisor agent** | A special session monitoring others | This is a *session* with a special role |

**Key observation:** There is no `Agent` struct in the codebase. The concept is distributed across `Loop` (behavior), `AgentWorker` (orchestration), `Session` (state container), and `Role` (capability boundary).

This creates several issues:
- **User confusion:** Users think "I'm talking to an agent" but the system describes it as "you're in a session with a loop"
- **Code confusion:** Developers must mentally map between AgentWorker, Loop, Session, and Role
- **Terminology drift:** "Agent" means different things in different contexts
- **Extensibility:** Hard to add new agent types or behaviors without modifying multiple components

---

### 2. Proposed Solution: Make Agent a First-Class Concept

Introduce a proper `Agent` struct that encapsulates the related concerns:

```go
// internal/agent/agent.go
type Agent struct {
    // Identity
    ID       string
    SessionID string

    // Behavior
    Loop   *Loop
    Role   runtime.Role

    // State
    History    []llm.Message
    Scratchpad []ScratchpadEntry

    // Relationships
    ParentID string

    // Lifecycle
    Status    AgentStatus
    CreatedAt time.Time
    UpdatedAt time.Time
}

type AgentStatus string

const (
    AgentStatusActive AgentStatus = "active"
    AgentStatusIdle   AgentStatus = "idle"
    AgentStatusDone   AgentStatus = "done"
    AgentStatusError  AgentStatus = "error"
)
```

**Agent becomes the unifying abstraction:**
```
Agent = Loop + Role + State + Session
```

---

### 3. Agent Types (Specializations)

All agents share the core `Agent` struct but can be specialized:

```go
// ConversationAgent - Interactive user session
type ConversationAgent struct {
    *Agent
    UserID string
}

// GoalAgent - Background goal pursuit
type GoalAgent struct {
    *Agent
    GoalID      string
    WakeInterval time.Duration
}

// SupervisorAgent - Monitors other agents
type SupervisorAgent struct {
    *Agent
    WatchedAgents []string
}

// SubAgent - Delegated work
type SubAgent struct {
    *Agent
    Task    string
    Timeout time.Duration
    Parent  *Agent
}
```

**Benefits:**
- All agent variants share common behavior through `*Agent`
- Specializations add domain-specific fields
- Clear type hierarchy for code organization

---

### 4. Agent Manager (Centralized Registry)

Replace the per-session `AgentWorker` model with a centralized `AgentManager`:

```go
// internal/runtime/manager.go
type AgentManager struct {
    mu           sync.RWMutex
    agents       map[string]*Agent // agentID  Agent
    sessionAgents map[string][]string // sessionID  []agentID
    supervisor   *SupervisorAgent
    loopBuilder  *agent.Builder
    roleRegistry *RoleRegistry
}

func (m *AgentManager) CreateAgent(
    ctx context.Context,
    sessionID string,
    role runtime.Role,
    parentID string,
) *Agent

func (m *AgentManager) CreateConversationAgent(
    ctx context.Context,
    sessionID string,
) *ConversationAgent

func (m *AgentManager) CreateGoalAgent(
    ctx context.Context,
    goalID string,
    interval time.Duration,
) *GoalAgent

func (m *AgentManager) CreateSubAgent(
    ctx context.Context,
    parent *Agent,
    task string,
    role runtime.Role,
) *SubAgent
```

---

### 5. Loop Integration

The `Loop` becomes a component of `Agent`:

```go
// internal/agent/loop.go - Modified to be Agent-aware
type Loop struct {
    cfg     Config
    agent   *Agent  // Reference to owning agent
    // ... rest of existing fields
}

// Agent can run its loop
func (a *Agent) Run(ctx context.Context, input string) (string, error) {
    req := a.buildContext(input)
    resp, err := a.Loop.queue.Submit(ctx, req)
    if err != nil {
        return "", err
    }
    return a.Loop.processResponse(ctx, resp)
}
```

---

### 6. Migration Path

#### Phase 1: Internal Refactoring (No User Impact)
- Introduce `Agent` struct
- Modify `AgentWorker` to wrap an `Agent`
- Keep all external APIs unchanged
- Update internal references

#### Phase 2: Dual CLI Support (Backward Compatible)
- Add new `nine agent *` commands
- Keep old `nine session`, `nine attach` as aliases with deprecation warnings
- Old commands internally use new agent model

#### Phase 3: Full Cutover
- Remove old command aliases
- Update all documentation
- Finalize new CLI structure

---

### 7. CLI Updates to Reflect New Model

#### Command Structure Changes

| Current | Proposed | Rationale |
|---------|----------|-----------|
| `nine session` | `nine agent start` | Start a conversation agent |
| `nine attach <id>` | `nine agent attach <id>` | Attach to an agent's session |
| `nine sessions` | `nine agent list` | List all agents |
| `nine goals` | `nine agent list --goals` | List goal agents |
| `nine trace <id>` | `nine agent trace <id>` | Trace an agent's execution |
| `nine replay <id>` | `nine agent replay <id>` | Replay an agent's session |

#### New Agent-Centric Commands

```bash
# Start a new agent (conversation)
nine agent start [--role <role>] [--session <id>]

# List all agents
nine agent list [--all] [--active] [--goals] [--subagents]

# Show agent details
nine agent show <agent-id>

# Send message to specific agent
nine agent send <agent-id> "<message>"

# Inspect agent state
nine agent state <agent-id> [--history] [--scratchpad] [--role]

# Manage sub-agents
nine agent spawn <parent-id> --task "<task>" [--role <role>]
nine agent subagents <agent-id>  # List sub-agents of an agent
```

#### Specialized Commands (Goal Agents)

```bash
nine goal create "<description>"  # Creates a goal + goal agent
nine goal list [--active] [--paused]
nine goal show <goal-id>
nine goal pause <goal-id>
nine goal resume <goal-id>
```

#### TUI Slash Command Updates

```
Current:
  /goals         List goals
  /context       Show context
  /attach <id>   Attach to session

Updated:
  /agents        List all agents
  /agent show    Show current agent info
  /agent role    Show current agent's role
  /agent spawn <task>  Spawn a sub-agent
  /agent attach <id>  Attach to agent
  /goals         Still works (alias for /agents --goals)
  /context       Still works (now: /agent context)
```

---

### 8. Configuration Updates

```toml
# Before: [standing_agents]
[[standing_agents]]
name = "monitor"
schedule = "0 * * * *"
role = "monitor"

# After: [agents.scheduled]
[agents]
default_role = "executor"

[agents.scheduled]
[[agents.scheduled.agents]]
name = "monitor"
schedule = "0 * * * *"
role = "monitor"
goal = "monitor-repo"

# Backward compatible alias
[[standing_agents]]  # Deprecated, maps to [agents.scheduled]
name = "monitor"
schedule = "0 * * * *"
```

---

### 9. What Gets Better

| Aspect | Before | After |
|--------|--------|-------|
| **Mental model** | "I'm in a session with a loop" | **"I'm talking to an agent"** |
| **Code organization** | Loop + Worker + Session scattered | **Agent encapsulates all three** |
| **CLI consistency** | Mixed metaphors (sessions, goals, workflows) | **Agent-centric with clear specializations** |
| **Sub-agents** | Implicit (just another loop) | **Explicit agent hierarchy** |
| **Supervisor** | Special case | **Just an agent with a role** |
| **Standing agents** | Confusing name | **Scheduled agents (or agents with schedules)** |
| **Extensibility** | Hard to add agent types | **Easy: just add new Agent subtypes** |

---

### 10. Example: Full Agent Lifecycle

```go
// 1. User starts a conversation
agent := manager.CreateConversationAgent(ctx, sessionID)

// 2. Agent runs in a loop
for {
    input := getUserInput()
    output, err := agent.Run(ctx, input)
    if err != nil {
        handleError(err)
    }
    displayOutput(output)
}

// 3. User requests a sub-agent
subAgent := manager.CreateSubAgent(ctx, agent, "research this topic", runtime.RoleResearcher)
result, err := subAgent.Run(ctx, "research query")
agent.RecordSubAgentResult(subAgent.ID, result)

// 4. Background goal agent
goalAgent := manager.CreateGoalAgent(ctx, "monitor-repo", 5*time.Minute)
go goalAgent.RunSchedule(ctx)  // Wakes every 5 minutes
```

---

### 11. Implementation Checklist

- [ ] Define `Agent` struct in `internal/agent/agent.go`
- [ ] Define agent status enum
- [ ] Create agent specializations (ConversationAgent, GoalAgent, etc.)
- [ ] Modify `Loop` to hold reference to its `Agent`
- [ ] Create `AgentManager` to replace per-session `AgentWorker`
- [ ] Update `AgentWorker` to wrap `Agent` (transition step)
- [ ] Add new `nine agent *` CLI commands
- [ ] Add backward-compatible aliases for old commands
- [ ] Update configuration schema
- [ ] Update documentation and glossary
- [ ] Add migration guide for config changes
- [ ] Deprecate old terminology with warnings
- [ ] Remove old aliases (final step)

---

### 12. Backward Compatibility

To minimize disruption:

1. **Keep old terms as aliases** initially (e.g., `nine sessions` → `nine agent list`)
2. **Deprecation warnings** for old commands
3. **Config backward compatibility** via aliases (`[standing_agents]` → `[agents.scheduled]`)
4. **Gradual rollout** with feature flags where possible

---

*This architectural change aligns Nine's internal structure with users' mental model: you talk to agents, not sessions with loops. It consolidates scattered concerns into a cohesive abstraction while maintaining backward compatibility.*
