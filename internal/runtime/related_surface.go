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
// session and an off-topic one at ~0.34  0.6 sits well inside that gap, so
// genuine relevance surfaces while off-topic questions stay silent.
const relatedSurfaceThreshold = 0.6

// relatedReader is the narrow read surface the enrichment needs from the store.
type relatedReader interface {
	RelatedSessions(agentID string) ([]memory.RelatedSession, error)
	VectorQuery(namespace string, vector []float32, topK int) ([]memory.VectorResult, error)
	LatestTurnResult(agentID string) (string, error)
}

// relatedEnrichmentFn builds the per-turn pull-surfacing function for one
// session (adr/reactive-events.md phase 3). It reads the recorded
// related_sessions links the out-of-band indexer maintains and surfaces the one
// most relevant to the current query, as a compact note the context builder
// places under its token budget. It never calls a generative LLM and never
// touches the active session; the value reaches the user only because their own
// question made a prior session relevant  pull, not push.
//
// Relevance gate: a recorded link is surfaced only when the current query is
// itself close (>= relatedSurfaceThreshold) to that session's index vector, so a
// link formed from an earlier, now-off-topic turn stays silent.
func relatedEnrichmentFn(r relatedReader, agentID string) func(context.Context, []float32) string {
	return func(_ context.Context, queryVec []float32) string {
		log.Debug("relatedEnrichmentFn called", "agentID", agentID, "queryVec_len", len(queryVec))
		if agentID == "" || len(queryVec) == 0 {
			log.Debug("relatedEnrichmentFn empty agentID or query")
			return ""
		}
		links, err := r.RelatedSessions(agentID)
		if err != nil {
			log.Warn("relatedEnrichmentFn RelatedSessions failed", "agentID", agentID, "err", err)
			return ""
		}
		if len(links) == 0 {
			log.Debug("relatedEnrichmentFn no links", "agentID", agentID)
			return ""
		}
		log.Debug("relatedEnrichmentFn got links", "agentID", agentID, "count", len(links))
		linked := make(map[string]bool, len(links))
		for _, l := range links {
			linked[l.RelatedAgentID] = true
		}

		// Rank the recorded links by their similarity to *this* query.
		matches, err := r.VectorQuery(sessionIndexNamespace, queryVec, len(links)+5)
		if err != nil {
			log.Warn("relatedEnrichmentFn VectorQuery failed", "agentID", agentID, "err", err)
			return ""
		}
		log.Debug("relatedEnrichmentFn got matches", "agentID", agentID, "count", len(matches))
		for _, m := range matches {
			if m.Key == agentID || m.Score < relatedSurfaceThreshold || !linked[m.Key] {
				log.Debug("relatedEnrichmentFn skipping match", "key", m.Key, "score", m.Score, "linked", linked[m.Key])
				continue
			}
			gist, err := r.LatestTurnResult(m.Key)
			if err != nil {
				log.Warn("relatedEnrichmentFn LatestTurnResult failed", "key", m.Key, "err", err)
				continue
			}
			if gist == "" {
				log.Debug("relatedEnrichmentFn empty gist", "key", m.Key)
				continue
			}
			log.Debug("relatedEnrichmentFn returning result", "key", m.Key)
			return "Related earlier session - the user has worked on a closely related topic before; " +
				"draw on it only if it helps, and don't assume they remember it:\n" + gist
		}
		log.Debug("relatedEnrichmentFn no matching session", "agentID", agentID)
		return ""
	}
}
