package runtime

import (
	"context"
	"strings"

	"nine/internal/memory"
)

// memorySurfaceThreshold is the minimum similarity a stored memory must clear
// against the current query to be surfaced. It mirrors relatedSurfaceThreshold:
// both compare the user's question to previously-recorded text, so genuine
// on-topic relevance clears the bar while off-topic turns stay silent.
const memorySurfaceThreshold = 0.6

// memorySurfaceTopN is the most memories surfaced in one turn. Kept small so
// enrichment informs the turn without crowding it (the context builder also
// caps the whole enrichment band at priority 2.6).
const memorySurfaceTopN = 3

// memoryReader is the narrow read surface the memory surfacer needs from the
// store: rank the shared memory pool by relevance, then resolve each hit's
// current value from the KV store (the pool stores only embeddings).
type memoryReader interface {
	VectorQuery(namespace string, vector []float32, topK int) ([]memory.VectorResult, error)
	Get(key string) (string, bool, error)
}

// memoryEnrichmentFn builds the per-turn pull-surfacing function for stored
// key-value memories. Every memory_set is mirrored into memory.MemoriesNamespace
// (keyed by the KV key); this ranks that pool against the current query and
// surfaces the few most relevant memories as advisory enrichment the context
// builder places under its token budget. It never calls a generative LLM.
//
// Values are re-read from the KV store (not from the vector row) so a surfaced
// memory always reflects the latest memory_set, never a stale embedding-time
// snapshot. Framing is deliberately non-authoritative: memories can be out of
// date, so the note tells the model to lean on them only when they help.
func memoryEnrichmentFn(r memoryReader) func(context.Context, []float32) string {
	return func(_ context.Context, queryVec []float32) string {
		log.Debug("memoryEnrichmentFn called", "queryVec_len", len(queryVec))
		if len(queryVec) == 0 {
			log.Debug("memoryEnrichmentFn empty query")
			return ""
		}
		// Over-fetch a little: some hits are dropped below threshold or because
		// their KV row was deleted between indexing and now.
		matches, err := r.VectorQuery(memory.MemoriesNamespace, queryVec, memorySurfaceTopN+2)
		if err != nil {
			log.Warn("memoryEnrichmentFn VectorQuery failed", "err", err)
			return ""
		}
		log.Debug("memoryEnrichmentFn got matches", "count", len(matches))
		lines := make([]string, 0, memorySurfaceTopN)
		for _, m := range matches {
			if m.Score < memorySurfaceThreshold {
				log.Debug("memoryEnrichmentFn match below threshold", "key", m.Key, "score", m.Score)
				continue
			}
			val, found, err := r.Get(m.Key)
			if err != nil {
				log.Warn("memoryEnrichmentFn Get failed", "key", m.Key, "err", err)
				continue
			}
			if !found || val == "" {
				log.Debug("memoryEnrichmentFn key not found or empty", "key", m.Key)
				continue
			}
			log.Debug("memoryEnrichmentFn adding memory", "key", m.Key, "score", m.Score)
			lines = append(lines, "- "+m.Key+": "+val)
			if len(lines) >= memorySurfaceTopN {
				log.Debug("memoryEnrichmentFn reached max lines", "count", len(lines))
				break
			}
		}
		if len(lines) == 0 {
			log.Debug("memoryEnrichmentFn no matching memories")
			return ""
		}
		log.Debug("memoryEnrichmentFn returning result", "line_count", len(lines))
		return "Related things you noted earlier - draw on them only if they help, " +
			"and don't assume they are still current:\n" + strings.Join(lines, "\n")
	}
}

// composeEnrichment folds several per-turn enrichment functions into one, so the
// loop's single enrichment channel can carry more than one pull-surfacer. It
// invokes each non-nil function in order and joins the non-empty results; the
// shared enrichment token cap (context builder priority 2.6) then applies to the
// combined note. Returns nil when no function is supplied, so the loop's
// "enrichment off, no extra path" fast path is preserved.
func composeEnrichment(fns ...func(context.Context, []float32) string) func(context.Context, []float32) string {
	log.Debug("composeEnrichment", "fn_count", len(fns))
	active := make([]func(context.Context, []float32) string, 0, len(fns))
	for _, fn := range fns {
		if fn != nil {
			active = append(active, fn)
		}
	}
	if len(active) == 0 {
		log.Debug("composeEnrichment no active functions")
		return nil
	}
	log.Debug("composeEnrichment has active functions", "count", len(active))
	return func(ctx context.Context, queryVec []float32) string {
		parts := make([]string, 0, len(active))
		for _, fn := range active {
			if s := fn(ctx, queryVec); s != "" {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, "\n\n")
	}
}
