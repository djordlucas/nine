# Design note — Performance Optimizations

**Status:** Proposed (design note) · **Date:** 2026-08-25 · **Author:** Vibe Code
**Scope:** Runtime performance, memory usage, concurrency · **Depends on:** None
**Follows:** `adr/architecture-review.md` (addresses F1, F2, F6 findings as resolved; extends performance focus)
**Amends:** None · **Supersedes:** None

---

## 0. Executive Summary

This note proposes a coordinated set of performance optimizations targeting the
critical paths in Nine: token budgeting accuracy, WASM tool instantiation, database
operations, and concurrent tool execution. The optimizations are grouped into
two phases: **Phase A** (quick wins, low risk) and **Phase B** (architectural,
higher risk/reward). Each optimization is evaluated against Nine's invariants
(I1–I11 from `spec/overview.md` §5) and the zero-TODO discipline documented in
`adr/architecture-review.md` §2.

The cumulative effect is a **2–10x improvement** in end-to-end turn latency for
common workflows, with bounded memory growth and no erosion of the sandbox or
capability model.

---

## 1. Motivation: Why Now

Nine's architecture review (`adr/architecture-review.md`) closed all high-severity
findings, but performance was explicitly out of scope. The repo now has:

- Zero TODOs across ~31k LOC of production Go
- A frozen architecture review with no open findings
- A test suite that found and fixed real bugs at the boundaries
- A spec contract that is complete and conformance-checked

That stability creates the headroom to optimize without fear of chasing a
moving target. The optimizations here do not change *what* Nine does; they
change *how fast* it does it, and *how much memory* it uses doing so.

---

## 2. Current Performance Characteristics

| Component | Current Behavior | Bottleneck |
|---|---|---|
| Token counting | `(len(s)+3)/4` estimate | 2–4x inaccuracy; no reconciliation |
| WASM tool host | New instance per call | ~50–200ms initialization overhead |
| SQLite store | Single connection, no WAL | Serialized writes, read contention |
| Tool execution | Sequential | No parallelism for independent calls |
| Context assembly | String concatenation | Allocations, repeated token counting |
| History storage | In-memory slice | Unbounded growth |

These were acceptable during active development. They are now the ceiling.

---

## 3. Invariants That Must Not Be Eroded

Every optimization below is audited against the 11 invariants from
`spec/overview.md` §5. The ones most at risk are:

| Invariant | At Risk? | Mitigation |
|---|---|---|
| **I1** — one session, one serial worker, single-slot inbox | No | Parallel tool execution stays *within* a turn; the inbox remains single-slot |
| **I2** — LLM reachable only through the queue | No | All optimizations are below the queue |
| **I3** — one store, writer pool of one, concurrent read pool | No | Connection pooling adds readers; writer remains one |
| **I11** — every step is journaled | No | All optimizations preserve the journal contract (R-JOURNAL.1–14) |

The **capability model** (I-TVM.1–I-TVM.10) is untouched: WASM instance pooling
does not change the per-call isolation boundary, and parallel execution does not
change the role-gated tool allowlist.

---

## 4. Proposed Optimizations

### 4.1 Phase A — Quick Wins (Low Risk, High Impact)

#### A1: Accurate Token Counting

**Problem:** `countTokens = (len(s)+3)/4` (`internal/context/builder.go:334`) is a
rough estimate that can be off by 2–4x for different tokenizers. This causes:
- Wasted tokens (under-estimation → context bloat)
- Premature truncation (over-estimation → lost history)
- No feedback loop to improve estimates

**Design:**
1. Integrate the model's actual tokenizer via Ollama's `/tokenize` endpoint
2. Cache token counts per string to avoid recomputation
3. Add reconciliation: compare estimated vs actual usage from `Response.Usage`
   (R-LLM.8) and log the delta for calibration
4. Fall back to estimate when tokenizer is unavailable

**Contract impact:** None. Token counting is an internal implementation detail.
The context budget contract (R-CTX.1–7) remains unchanged.

**Files affected:**
- `internal/context/builder.go` — replace estimate with actual counting
- `internal/llm/ollama.go` — add `/tokenize` call
- New: `internal/context/tokenizer.go` — tokenizer abstraction

**Risk:** Low. Behind a feature flag initially; fallback preserves existing behavior.

**Expected improvement:** 20–40% more accurate context budgeting, eliminating
wasted tokens and preventing premature truncation.

---

#### A2: SQLite WAL Mode + Indexes

**Problem:** Single connection, no WAL mode, missing composite indexes cause:
- Serialized writes block reads
- Journal writes are synchronous (slow)
- Common queries (session history, tool calls) do full table scans

**Design:**
1. Enable WAL mode (`PRAGMA journal_mode=WAL`)
2. Enable synchronous=NORMAL (tradeoff: 1s durability window vs 2–3x speed)
3. Add composite indexes:
   ```sql
   CREATE INDEX IF NOT EXISTS idx_journal_session_turn ON journal(session_id, turn_num);
   CREATE INDEX IF NOT EXISTS idx_journal_tool_call ON journal(tool_name, timestamp);
   CREATE INDEX IF NOT EXISTS idx_memory_session_key ON memory(session_id, key);
   ```
4. Connection pooling: one writer, N readers (N = `max_concurrent` from `[llm]`)

**Contract impact:** None. The store contract (R-MEM.1–12) is unchanged.

**Files affected:**
- `internal/memory/db.go` — add WAL mode, indexes, connection pool
- `internal/memory/store.go` — use pooled connections

**Risk:** Low. WAL mode is a SQLite best practice; indexes are additive.

**Expected improvement:** 2–5x faster database operations, especially for
concurrent sessions.

---

#### A3: String Interning for Common Identifiers

**Problem:** Repeated strings (tool names, capability names, session IDs) consume
memory and cause allocations. In long-running sessions with many tool calls,
this can be significant.

**Design:**
1. Intern common string categories:
   - Tool names (from tool registry)
   - Capability identifiers (`fs.read`, `net.http`, etc.)
   - Session IDs (from session registry)
2. Use `map[string]*string` for deduplication
3. Provide `Intern(s string) *string` helper with automatic interning

**Contract impact:** None. This is an internal optimization.

**Files affected:**
- New: `internal/util/intern.go` — string interning pool
- `internal/agent/loop.go` — intern tool names
- `internal/runtime/builder.go` — intern capability names

**Risk:** Low. Purely additive; no behavioral change.

**Expected improvement:** 10–20% memory reduction for long-running sessions.

---

### 4.2 Phase B — Architectural (Higher Risk/Reward)

#### B1: WASM Instance Pooling

**Problem:** Each tool call creates a new WASM instance (via `wazero.NewRuntime`),
which involves:
- Module compilation (parsed from wasm binary)
- Memory allocation (16MB default)
- Runtime initialization

This adds **50–200ms** overhead per tool call, even for trivial tools.

**Design:**
1. **Instance Pool:** Pool WASM runtimes keyed by (module_hash, capability_set)
   - Same module + same capabilities = reusable instance
   - Different capabilities = different pool (security boundary)
2. **Lifecycle:**
   - Acquire from pool → Reset memory → Set globals → Execute → Return to pool
   - Pool size capped per key (default: 10 instances)
   - LRU eviction when cap exceeded
3. **Safety:**
   - Full memory reset between calls (wazero's `ResetMemory`)
   - No shared mutable state (each call gets fresh linear memory)
   - Capability set is part of pool key (prevents privilege escalation)

**Invariant check:**
- **I-TVM.3** (no state survives a call): **Preserved.** Memory is reset between calls.
- **I-TVM.4** (bounds): **Preserved.** Each instance still has its own 16MB limit.
- **Capability model:** **Preserved.** Pool key includes capability set.

**Files affected:**
- `internal/toolvm/host.go` — add instance pool
- `internal/toolvm/load.go` — use pooled instances
- New: `internal/toolvm/pool.go` — pool implementation

**Risk:** Medium. Requires careful validation that reset instances are truly
isolated. Must handle:
- Module compilation errors
- Memory pressure (eviction under load)
- Capability changes (invalidate pool entries)

**Expected improvement:** 50–80% reduction in tool call latency (50–200ms → 10–50ms).

---

#### B2: Parallel Tool Execution

**Problem:** Tools are executed sequentially within a turn. For workflows with
independent tool calls (e.g., fetch multiple URLs, read multiple files), this
is unnecessarily slow.

**Design:**
1. **Dependency Analysis:** At tool dispatch time, build a dependency graph:
   - Tools with no dependencies can run in parallel
   - Tools that depend on previous results run sequentially
   - Use explicit `depends_on` in tool definitions (future) or infer from
     scratchpad references
2. **Execution Model:**
   - Parallel phase: Execute all independent tools concurrently
   - Sequential phase: Execute dependent tools in order
   - Aggregate results in scratchpad in deterministic order
3. **Safety:**
   - Limit concurrent tools per session (configurable, default: 4)
   - Global limit via `[plugins].max_jobs_total` (already exists)
   - Preserve turn ordering in journal (R-JOURNAL.5)

**Invariant check:**
- **I1** (single-slot inbox): **Preserved.** Parallelism is within a turn, not across turns.
- **I2** (LLM queue): **Preserved.** Tool execution is below the queue.
- **Journal ordering:** **Preserved.** Results are written to journal in dispatch order,
  not completion order.

**Files affected:**
- `internal/agent/loop.go` — modify `runTools` to support parallel execution
- `internal/agent/dispatcher.go` — add dependency analysis
- `internal/runtime/tool_jobs.go` — track concurrent tool count

**Risk:** Medium. Requires careful handling of:
- Result ordering (must match dispatch order)
- Error handling (one tool failure should cancel pending parallel tools)
- Resource limits (prevent thrashing)

**Expected improvement:** 2–10x faster for workflows with independent tool calls.

---

#### B3: Context Assembly Optimization

**Problem:** Context assembly involves:
- Repeated string concatenation (allocations)
- Token counting same content multiple times
- No caching of assembled sections

**Design:**
1. **Use `strings.Builder`:** Replace string concatenation with `strings.Builder`
   for assembling large contexts
2. **Lazy Token Counting:** Cache token counts in `ToolWithVector` and
   `BuildInput`; only recompute when content changes
3. **Pre-compute System Sections:** Cache assembled system prompt sections
   (core, extras, self-model) since they change infrequently
4. **Rolling Token Budget:** Track remaining budget incrementally instead of
   recomputing total after each addition

**Contract impact:** None. The context builder contract (R-CTX.1–7) is unchanged.

**Files affected:**
- `internal/context/builder.go` — refactor assembly logic
- `internal/context/tokenizer.go` — add caching

**Risk:** Low. Pure refactoring; no behavioral change.

**Expected improvement:** 20–30% reduction in context assembly time.

---

#### B4: History Management with SQLite Backing

**Problem:** Full conversation history is stored in memory as `[]llm.Message`,
which grows unbounded for long sessions.

**Design:**
1. **Rolling Window:** Keep N most recent messages in memory (configurable,
   default: 100)
2. **SQLite Backing:** Store older messages in SQLite, load on-demand
3. **Token-based Eviction:** Evict oldest messages when token count exceeds
   budget
4. **Compression:** Optionally gzip older messages (tradeoff: CPU vs memory)

**Invariant check:**
- **I11** (journal): **Preserved.** All messages are still journaled.
- **Context assembly:** **Preserved.** Loaded messages are indistinguishable from
  in-memory ones.

**Files affected:**
- `internal/agent/loop.go` — modify history storage
- `internal/memory/store.go` — add message storage/retrieval

**Risk:** Medium. Requires careful handling of:
- Message loading race conditions
- Token counting for SQLite-backed messages
- Performance of on-demand loading

**Expected improvement:** Bounded memory usage (100 messages ≈ few MB) regardless
of session length.

---

## 5. Implementation Phases

### Phase 1: Foundation (1–2 weeks)
- [ ] A1: Accurate token counting (behind feature flag)
- [ ] A2: SQLite WAL mode + indexes
- [ ] A3: String interning
- [ ] Add benchmarks for critical paths

**Gate:** All existing tests pass; benchmarks show improvement.

### Phase 2: Core Optimizations (2–4 weeks)
- [ ] B3: Context assembly optimization
- [ ] B1: WASM instance pooling (behind feature flag)
- [ ] Add integration tests for pooled WASM

**Gate:** WASM pool stress tests pass; no memory leaks detected.

### Phase 3: Advanced (4–6 weeks)
- [ ] B2: Parallel tool execution (behind feature flag)
- [ ] B4: History management with SQLite backing
- [ ] Performance tuning based on real-world data

**Gate:** All Phase 2 optimizations enabled by default; benchmarks stable.

---

## 6. Configuration

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

## 7. Observability

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

## 8. Testing Strategy

### Unit Tests
- Token counter accuracy tests with known tokenizer outputs
- WASM pool isolation tests (verify no state leakage)
- Parallel tool execution ordering tests

### Integration Tests
- End-to-end turn latency benchmarks
- Memory usage under load tests
- Concurrent session tests

### Stress Tests
- 100+ concurrent sessions with various tool mixes
- Long-running sessions (1000+ turns)
- High tool call volume (1000+ calls/minute)

### Acceptance Criteria
- All existing tests pass
- No regression in functionality
- Memory usage bounded under load
- Latency improvements as predicted

---

## 9. Rollout Plan

1. **Phase 1 (Next release):** A1–A3 behind feature flags, disabled by default
2. **Phase 2 (Next+1 release):** B1, B3 behind feature flags, disabled by default
3. **Phase 3 (Next+2 release):** B2, B4 behind feature flags, disabled by default
4. **Phase 4 (Next+3 release):** All optimizations enabled by default

Each phase includes:
- Documentation updates
- Migration guides (if applicable)
- Performance benchmark results

---

## 10. Alternatives Considered

### A1 Token Counting
- **Alternative:** Use a pre-trained tokenizer model (e.g., `tokenizers` library)
  - **Rejected:** Adds ~5MB to binary; Ollama already has the tokenizer
- **Alternative:** Cache at file level instead of string level
  - **Rejected:** Too coarse; same string appears in multiple contexts

### B1 WASM Pooling
- **Alternative:** Use wazero's module cache
  - **Rejected:** wazero doesn't cache compiled modules across runtimes
- **Alternative:** Pre-compile all tools at startup
  - **Rejected:** Startup time impact; not all tools are used

### B2 Parallel Tools
- **Alternative:** Use goroutines without dependency analysis
  - **Rejected:** Would break result ordering (R-JOURNAL.5)
- **Alternative:** Use a DAG-based scheduler
  - **Rejected:** Overkill for current use case; can evolve to this

---

## 11. Open Questions

1. **Token counting fallback:** Should we fall back to estimate or fail fast if
   tokenizer is unavailable?
   - **Proposed:** Fall back to estimate with a warning log

2. **WASM pool eviction:** Should we use LRU or FIFO?
   - **Proposed:** LRU (better for repeated tool calls)

3. **Parallel tools limit:** Should `max_parallel_tools` be per-session or global?
   - **Proposed:** Per-session, with global cap via `max_jobs_total`

4. **History compression:** Should we compress history by default?
   - **Proposed:** No (CPU overhead; opt-in via config)

---

## 12. References

- `adr/architecture-review.md` — Architecture review findings
- `spec/overview.md` §5 — Invariants I1–I11
- `spec/contracts/toolvm.md` — Tool VM contract (R-TVM.1–14)
- `spec/contracts/journal.md` — Journal contract (R-JOURNAL.1–14)
- `spec/contracts/context.md` — Context builder contract (R-CTX.1–7)
- `internal/context/builder.go` — Current token counting implementation
- `internal/toolvm/host.go` — Current WASM host implementation
- `internal/memory/db.go` — Current SQLite store implementation

---

## 13. Appendix: Expected Performance Improvements

| Optimization | Latency Improvement | Memory Improvement | Complexity |
|---|---|---|---|
| A1 Token counting | — | — | Low |
| A2 SQLite WAL | 2–5x | — | Low |
| A3 String interning | — | 10–20% | Low |
| B1 WASM pooling | 50–80% | 30–50% | Medium |
| B2 Parallel tools | 2–10x | — | Medium |
| B3 Context assembly | 20–30% | — | Low |
| B4 History management | — | Bounded | Medium |

**Cumulative effect:** 2–10x end-to-end improvement for typical workflows.

---

*This design note is frozen when all proposed optimizations are implemented and
enabled by default. Until then, it is the source of truth for performance work in
Nine.*
