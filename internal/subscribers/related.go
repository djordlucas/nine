// Package subscribers holds optional, config-gated journal subscribers that
// react to session events out-of-band (adr/reactive-events.md §4). They are
// registered on the daemon, never wired into the agent loop, and enrich derived
// stores that later user-initiated turns pull from — enrich, don't interject.
package subscribers

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"nine/internal/embed"
	"nine/internal/memory"
)

// indexNamespace is the vector namespace holding one embedding per completed
// turn, keyed by the session's agent id, used to find topically-similar prior
// sessions.
const indexNamespace = "session-index"

// RelatedStore is the derived-store surface the indexer writes to.
type RelatedStore interface {
	VectorStore(id, namespace, key string, vector []float32) error
	VectorQuery(namespace string, vector []float32, topK int) ([]memory.VectorResult, error)
	RelatedSessionAdd(agentID, relatedAgentID string, score float32) error
}

// RelatedIndexer is a programmatic, out-of-band subscriber: on each turn_end it
// embeds the answer, links the session to topically-similar prior sessions
// (recorded in related_sessions), and adds this turn's vector to the index. It
// makes no generative LLM call and never touches the active session — the value
// surfaces only when a later turn pulls related_sessions (adr/reactive-events.md
// §5).
type RelatedIndexer struct {
	store     RelatedStore
	embedder  embed.Embedder
	topK      int
	threshold float32
}

// NewRelatedIndexer builds the indexer. embedder must be non-nil (the subscriber
// is only registered when embeddings are configured). The 0.75 similarity
// threshold suits a semantic embedder; the built-in keyword (feature-hashing)
// embedder scores related-but-distinct text lower, so it links less readily.
func NewRelatedIndexer(store RelatedStore, embedder embed.Embedder) *RelatedIndexer {
	return &RelatedIndexer{store: store, embedder: embedder, topK: 5, threshold: 0.75}
}

func (r *RelatedIndexer) ID() string      { return "related-session-indexer" }
func (r *RelatedIndexer) Types() []string { return []string{"turn_end"} }

// Handle indexes one completed turn and records its links. Idempotent: the
// vector id is per-event (agent:seq, overwrites on redelivery) and
// RelatedSessionAdd upserts, so at-least-once delivery is safe.
func (r *RelatedIndexer) Handle(ctx context.Context, ev memory.SessionEvent) error {
	var p struct {
		Result string `json:"result"`
	}
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		return err
	}
	text := strings.TrimSpace(p.Result)
	if text == "" {
		return nil // nothing to index (e.g. an errored turn with no answer)
	}
	vec, err := r.embedder.Embed(ctx, text)
	if err != nil {
		return err
	}
	if len(vec) == 0 {
		return nil
	}

	// Link to prior sessions similar to this turn (query before adding self, so
	// the current vector can't match itself). Results are similarity-ordered;
	// keep the best score per distinct other agent above the threshold.
	matches, err := r.store.VectorQuery(indexNamespace, vec, r.topK+1)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, m := range matches {
		if m.Key == ev.AgentID || seen[m.Key] {
			continue
		}
		if m.Score < r.threshold {
			continue
		}
		seen[m.Key] = true
		if err := r.store.RelatedSessionAdd(ev.AgentID, m.Key, m.Score); err != nil {
			return err
		}
	}

	// Add this turn to the index for future queries.
	id := ev.AgentID + ":" + strconv.FormatInt(ev.Seq, 10)
	return r.store.VectorStore(id, indexNamespace, ev.AgentID, vec)
}
