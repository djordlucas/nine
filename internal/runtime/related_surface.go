package runtime

import (
	"context"

	"nine/internal/memory"
)

// sessionIndexNamespace is the pgvector namespace the RelatedIndexer writes one
// embedding per completed turn into, keyed by the session's agent id
// (subscribers.indexNamespace). The surfacer queries it to rank recorded links
// by relevance to the *current* turn.
const sessionIndexNamespace = "session-index"

// relatedSurfaceThreshold is the minimum current-query similarity a linked
// session must clear to be surfaced. It is deliberately lower than the indexer's
// 0.75 *linking* threshold: a link compares two answers, but surfacing compares
// the user's *question* to a prior answer, and a question is structurally less
// similar to its answer than two answers are to each other. Live measurement
// (nomic-embed-text) put an on-topic question at ~0.70 against the matching
// session and an off-topic one at ~0.34 — 0.6 sits well inside that gap, so
// genuine relevance surfaces while off-topic questions stay silent.
const relatedSurfaceThreshold = 0.6

// relatedReader is the narrow read surface the enrichment needs from the store.
type relatedReader interface {
	RelatedSessions(agentID string) ([]memory.RelatedSession, error)
	VectorQuery(namespace string, vector []float32, topK int) ([]memory.VectorResult, error)
	LatestTurnResult(agentID string) (string, error)
}

// relatedEnrichmentFn builds the per-turn pull-surfacing function for one
// session (docs/reactive-events.md phase 3). It reads the recorded
// related_sessions links the out-of-band indexer maintains and surfaces the one
// most relevant to the current query, as a compact note the context builder
// places under its token budget. It never calls a generative LLM and never
// touches the active session; the value reaches the user only because their own
// question made a prior session relevant — pull, not push.
//
// Relevance gate: a recorded link is surfaced only when the current query is
// itself close (>= relatedSurfaceThreshold) to that session's index vector, so a
// link formed from an earlier, now-off-topic turn stays silent.
func relatedEnrichmentFn(r relatedReader, agentID string) func(context.Context, []float32) string {
	return func(_ context.Context, queryVec []float32) string {
		if agentID == "" || len(queryVec) == 0 {
			return ""
		}
		links, err := r.RelatedSessions(agentID)
		if err != nil || len(links) == 0 {
			return ""
		}
		linked := make(map[string]bool, len(links))
		for _, l := range links {
			linked[l.RelatedAgentID] = true
		}

		// Rank the recorded links by their similarity to *this* query.
		matches, err := r.VectorQuery(sessionIndexNamespace, queryVec, len(links)+5)
		if err != nil {
			return ""
		}
		for _, m := range matches {
			if m.Key == agentID || m.Score < relatedSurfaceThreshold || !linked[m.Key] {
				continue
			}
			gist, err := r.LatestTurnResult(m.Key)
			if err != nil || gist == "" {
				continue
			}
			return "Related earlier session — the user has worked on a closely related topic before; " +
				"draw on it only if it helps, and don't assume they remember it:\n" + gist
		}
		return ""
	}
}
