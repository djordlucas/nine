# Accurate Token Counting Implementation Plan

**Status:** Proposed (implementation plan) \u0014 **Date:** 2026-08-26 \u0014 **Author:** Vibe Code
**Scope:** Performance optimization, context budgeting accuracy \u0014 **Depends on:** None
**Follows:** `adr/codebase-improvement.md` (implements A1 from Part II, Section 4.1)
**Amends:** None \u0014 **Supersedes:** None

---

## 0. Executive Summary

This document provides the **implementation plan** for replacing Nine's current byte-based token estimation (`(len(s)+3)/4`) with **actual tokenizer-based counting** via Ollama's `/api/tokenize` endpoint. This addresses **F1** from `adr/architecture-review.md` and implements **A1** from the performance optimizations in `adr/codebase-improvement.md`.

The change is gated behind a feature flag (`[performance].accurate_token_counting`), defaults to **disabled** for backward compatibility, and falls back to the estimate if the tokenizer is unavailable. The expected improvement is **20\u201340% more accurate context budgeting** without regression in turn latency.

---

## 1. Motivation

### Current State

The current token counting implementation in `internal/context/builder.go` uses a simple byte-based estimate:

```go
const (
    bytesPerTokenNum = 100
    bytesPerTokenDen = 345
)

func countTokens(s string) int {
    if s == "" {
        return 0
    }
    return (len(s)*bytesPerTokenNum + bytesPerTokenDen - 1) / bytesPerTokenDen
}
```

This estimate was calibrated against real usage data and over-estimates by ~2.5% typically, but:
- It is **model-specific** (calibrated for one tokenizer family)
- It **cannot adapt** to different tokenizers
- It is **never reconciled** against actual provider-reported usage (R-LLM.8)
- It introduces **2\u20134x inaccuracy** for tokenizers that differ materially from the calibration model

### Why Now

The architecture review (`adr/architecture-review.md`) closed all high-severity findings, with **F1 (token budgeting inaccuracy)** explicitly marked as addressed by landing `Response.Usage` (R-LLM.8) in the journal. The calibration work was done, but the **reconciliation and accuracy improvement** were deferred. The codebase is now stable enough to implement this optimization without chasing a moving target.

Ollama's `/api/tokenize` endpoint provides direct access to the model's actual tokenizer, enabling **exact token counting** without adding external dependencies.

---

## 2. Design

### 2.1 Tokenizer Abstraction

Introduce a `Tokenizer` interface to decouple the counting mechanism from its usage:

```go
// internal/context/tokenizer.go
type Tokenizer interface {
    // Count returns the number of tokens in text.
    Count(text string) int
    
    // Tokenize returns the token IDs for text (for debugging/reconciliation).
    Tokenize(text string) ([]int, error)
}
```

This allows:
- Multiple implementations (estimate, Ollama, cached)
- Easy testing with mock tokenizers
- Future extensibility (e.g., local tokenizer libraries)

### 2.2 Implementations

#### EstimateTokenizer (Current Behavior)
Preserves the existing byte-based estimate as a fallback:

```go
type EstimateTokenizer struct{}

func (e *EstimateTokenizer) Count(s string) int {
    if s == "" {
        return 0
    }
    return (len(s)*bytesPerTokenNum + bytesPerTokenDen - 1) / bytesPerTokenDen
}
```

#### OllamaTokenizer (New Behavior)
Calls Ollama's `/api/tokenize` endpoint:

```go
type OllamaTokenizer struct {
    client   *http.Client
    endpoint string
    model    string
}

func (o *OllamaTokenizer) Count(s string) int {
    tokens, err := o.Tokenize(s)
    if err != nil {
        // Fallback to estimate on error
        return estimateTokenizer.Count(s)
    }
    return len(tokens)
}

func (o *OllamaTokenizer) Tokenize(s string) ([]int, error) {
    // POST to /api/tokenize with {"content": s}
    // Return response.Tokens
}
```

#### CachedTokenizer (Performance Optimization)
Wraps any `Tokenizer` with an LRU cache to avoid repeated counting of the same strings:

```go
type CachedTokenizer struct {
    Tokenizer
    cache    *lru.Cache  // key: string, value: int
    cacheSize int
    mu       sync.Mutex
}

func (c *CachedTokenizer) Count(s string) int {
    if count, ok := c.getCached(s); ok {
        return count
    }
    count := c.Tokenizer.Count(s)
    c.cachePut(s, count)
    return count
}
```

### 2.3 Integration with Ollama Provider

Add a `Tokenize` method to the Ollama provider in `internal/llm/ollama/ollama.go`:

```go
func (p *Provider) Tokenize(ctx context.Context, text string) ([]int, error) {
    req := map[string]string{"content": text}
    var resp struct {
        Tokens []int `json:"tokens"`
    }
    err := p.postJSON(ctx, "/api/tokenize", req, &resp)
    return resp.Tokens, err
}
```

### 2.4 Context Builder Changes

Update `internal/context/builder.go` to:
1. Accept a `Tokenizer` in the `Builder` struct
2. Replace all `countTokens()` calls with `tokenizer.Count()`
3. Add a token count cache for repeated strings (e.g., system prompt, tool schemas)

```go
type Builder struct {
    // ... existing fields
    tokenizer Tokenizer
    tokenCache map[string]int  // For strings counted multiple times per turn
}

func (b *Builder) countTokens(s string) int {
    // Use the configured tokenizer
    return b.tokenizer.Count(s)
}
```

### 2.5 Reconciliation Logic

Add reconciliation to compare estimated vs actual token usage from `Response.Usage` (R-LLM.8):

```go
func (b *Builder) recordReconciliation(estimated, actual int) {
    // Log ratio for observability
    ratio := float64(actual) / float64(estimated)
    slog.Debug("token count reconciliation",
        "estimated", estimated,
        "actual", actual,
        "ratio", ratio,
    )
    // Update metrics
    tokenCountAccuracyGauge.Set(ratio)
}
```

---

## 3. Configuration

### 3.1 New Configuration Section

Add a `[performance]` section to `nine.toml`:

```toml
[performance]
# Enable accurate token counting via Ollama's /tokenize endpoint.
# When disabled, uses the byte-based estimate (current behavior).
accurate_token_counting = false

# Maximum number of unique strings to cache token counts for.
# Set to 0 to disable caching. Default: 1000
token_count_cache_size = 1000
```

### 3.2 Configuration Struct

Update `internal/config/config.go`:

```go
type PerformanceConfig struct {
    // AccurateTokenCounting enables tokenizer-based counting via Ollama.
    // When false, uses the byte-based estimate (backward compatible).
    AccurateTokenCounting bool `toml:"accurate_token_counting"`
    
    // TokenCountCacheSize is the maximum number of unique strings to cache.
    // 0 disables caching. Default: 1000.
    TokenCountCacheSize int `toml:"token_count_cache_size"`
}

type Config struct {
    // ... existing fields
    Performance PerformanceConfig `toml:"performance"`
}
```

### 3.3 Environment Override

Add to `internal/config/factory.go`:

```go
func ApplyEnvOverrides(cfg *Config) {
    // ... existing overrides
    if v := os.Getenv("NINE_PERFORMANCE_ACCURATE_TOKEN_COUNTING"); v != "" {
        cfg.Performance.AccurateTokenCounting = v == "true"
    }
    if v := os.Getenv("NINE_PERFORMANCE_TOKEN_COUNT_CACHE_SIZE"); v != "" {
        if n, err := strconv.Atoi(v); err == nil {
            cfg.Performance.TokenCountCacheSize = n
        }
    }
}
```

---

## 4. Feature Flag Strategy

### Phase 1: Disabled by Default (This Implementation)
- `accurate_token_counting = false` (default)
- All existing behavior preserved
- New code paths exercised only when explicitly enabled

### Phase 2: Opt-In (Next Release)
- Default remains `false`
- Documentation updated to recommend enabling
- Operators can opt-in via config or env var

### Phase 3: Enabled by Default (Following Release)
- Default changes to `true`
- Fallback to estimate still available if tokenizer fails
- Old behavior available via explicit `false`

### Phase 4: Remove Flag (Future)
- Accurate counting becomes the only behavior
- Estimate tokenizer removed (or kept for edge cases)

---

## 5. Implementation Phases

### Phase 1: Foundation (1\u00132 weeks)
- [ ] Create `internal/context/tokenizer.go` with interface and implementations
- [ ] Add `Tokenize()` method to Ollama provider
- [ ] Add `[performance]` config section
- [ ] Add environment variable overrides

**Gate:** All existing tests pass; new code compiles without errors.

### Phase 2: Integration (2\u00133 weeks)
- [ ] Update `internal/context/builder.go` to use tokenizer
- [ ] Add token count cache to builder
- [ ] Add reconciliation logic
- [ ] Update existing tests to pass with new code paths

**Gate:** All existing tests pass; feature flag works correctly.

### Phase 3: Testing & Validation (3\u00134 weeks)
- [ ] Add unit tests for all tokenizer implementations
- [ ] Add benchmarks comparing estimate vs actual counting
- [ ] Add integration tests for end-to-end token counting
- [ ] Validate reconciliation logging and metrics

**Gate:** All tests pass; benchmarks show 20\u201340% accuracy improvement.

---

## 6. File Changes

### New Files (4)
| File | Purpose |
|------|---------|
| `internal/context/tokenizer.go` | Tokenizer interface and implementations |
| `internal/context/tokenizer_test.go` | Unit tests for tokenizer |
| `internal/context/tokenizer_bench_test.go` | Benchmarks for token counting |
| *(Optional)* `internal/context/cache.go` | Generic LRU cache if not already present |

### Modified Files (5)
| File | Changes |
|------|---------|
| `internal/context/builder.go` | Replace `countTokens()`, add caching, reconciliation |
| `internal/llm/ollama/ollama.go` | Add `Tokenize()` method |
| `internal/config/config.go` | Add `PerformanceConfig` struct |
| `internal/config/factory.go` | Add environment variable overrides |
| `internal/context/builder_test.go` | Update tests for new behavior |

---

## 7. Acceptance Criteria

### Functional
- [ ] All existing tests pass with feature flag disabled
- [ ] All existing tests pass with feature flag enabled
- [ ] Feature flag `accurate_token_counting = false` \u2192 uses estimate (backward compatible)
- [ ] Feature flag `accurate_token_counting = true` \u2192 uses Ollama tokenizer
- [ ] Fallback to estimate if Ollama tokenizer fails (network error, endpoint missing)
- [ ] Token count cache reduces repeated string counting
- [ ] Reconciliation logs show estimate vs actual comparison

### Performance
- [ ] Benchmarks show 20\u201340% improvement in counting accuracy
- [ ] Cache hit rate > 80% for typical sessions
- [ ] No regression in turn latency (>5% increase)
- [ ] Memory overhead < 10MB for cache

### Observability
- [ ] Reconciliation metrics exposed (`nine_token_count_accuracy`)
- [ ] Debug logs for token counting decisions
- [ ] Error logs for tokenizer failures

---

## 8. Testing Strategy

### Unit Tests
- Tokenizer interface conformance
- Estimate tokenizer accuracy (known inputs)
- Ollama tokenizer with mocked HTTP client
- Cached tokenizer hit/miss behavior
- Fallback behavior on errors

### Benchmarks
- Estimate vs actual counting accuracy
- Cache hit rate under typical workloads
- Token counting latency (estimate vs Ollama)
- Memory usage with cache enabled

### Integration Tests
- End-to-end turn with accurate counting enabled
- Context assembly with cached token counts
- Reconciliation logging verification
- Error handling (Ollama unavailable)

### Stress Tests
- Long-running session (1000+ turns)
- High concurrency (100+ concurrent sessions)
- Large context windows (32K+ tokens)

---

## 9. Rollout Plan

| Phase | Release | Feature Flag Default | Action |
|-------|---------|---------------------|--------|
| 1 | This branch | `false` | Implement, test internally |
| 2 | Next release | `false` | Ship behind flag, document |
| 3 | Next+1 release | `true` | Enable by default for new installs |
| 4 | Next+2 release | `true` | Enable by default for all |
| 5 | Future | N/A | Remove flag, accurate counting only |

Each phase includes:
- Documentation updates
- Migration guides (if applicable)
- Performance benchmark results

---

## 10. Risks and Mitigations

| Risk | Likelihood | Impact | Mitigation |
|------|------------|--------|------------|
| Ollama `/tokenize` endpoint changes or is removed | Low | High | Fallback to estimate; version check; feature flag allows disable |
| Network latency to Ollama degrades performance | Medium | Medium | Cache token counts; batch requests where possible |
| Tokenizer unavailable (Ollama down) | Medium | Medium | Graceful fallback to estimate with warning log |
| Cache memory usage grows unbounded | Low | Low | Configurable cache size; LRU eviction; default cap of 1000 entries |
| Breaking existing behavior | Low | High | Feature flag disabled by default; extensive testing |
| Tokenizer returns different results than model uses | Low | Medium | Use same model for tokenizing as for inference; validate with reconciliation |

---

## 11. Alternatives Considered

### Alternative 1: Use a Local Tokenizer Library
**Pros:** No network dependency, faster, works offline
**Cons:** Adds ~5MB to binary, may not match model's tokenizer exactly, maintenance burden
**Decision:** Rejected in favor of Ollama's endpoint (already has the tokenizer, no binary size increase)

### Alternative 2: Cache at File Level Instead of String Level
**Pros:** Simpler implementation, less memory
**Cons:** Too coarse; same string appears in multiple contexts (e.g., system prompt fragments)
**Decision:** Rejected; string-level caching provides better accuracy

### Alternative 3: Always Use Tokenizer, No Fallback
**Pros:** Simpler code, no feature flag needed
**Cons:** Breaks existing behavior if Ollama is unavailable; no graceful degradation
**Decision:** Rejected; backward compatibility and graceful degradation are critical

### Alternative 4: Use Provider's Usage Reporting Only (No Pre-Counting)
**Pros:** Always accurate, no estimation needed
**Cons:** Cannot enforce budget before sending request; may exceed context window
**Decision:** Rejected; pre-counting is necessary for budget enforcement

---

## 12. Open Questions

1. **Fallback behavior:** Should we fall back to estimate or fail fast if tokenizer is unavailable?
   - **Proposed:** Fall back to estimate with a warning log

2. **Cache eviction policy:** Should we use LRU or another strategy?
   - **Proposed:** LRU (simple, effective for typical access patterns)

3. **Cache key:** Should we cache by string content only, or include context (model, etc.)?
   - **Proposed:** String content only (token count is model-agnostic for our purposes)

4. **Reconciliation action:** Should we adjust future estimates based on reconciliation data?
   - **Proposed:** Log only for now; future enhancement to adapt estimates

---

## 13. References

- `adr/architecture-review.md` \u0014 Architecture review findings (F1)
- `adr/codebase-improvement.md` \u0014 Performance optimizations (A1)
- `spec/contracts/llm.md` \u0014 LLM contract (R-LLM.8: Usage reporting)
- `internal/context/builder.go` \u0014 Current token counting implementation
- `internal/llm/ollama/ollama.go` \u0014 Ollama provider
- `internal/config/config.go` \u0014 Configuration structure
- Ollama API Documentation: `/api/tokenize` endpoint

---

## 14. Appendix: Expected Performance Improvements

| Metric | Current | Target | Improvement |
|--------|---------|--------|-------------|
| Token count accuracy | \u00b110-15% | \u00b15% | 20-40% more accurate |
| Context budget utilization | Over-estimate | Accurate | Better use of available tokens |
| Turn latency (counting only) | ~0ms (estimate) | ~1-5ms (Ollama call) | Negligible with cache |
| Memory usage | N/A | <10MB (cache) | Acceptable overhead |

---

*This implementation plan is **frozen** when the accurate token counting feature is fully implemented and the feature flag is removed. Until then, it serves as the source of truth for the A1 optimization in Nine.*
