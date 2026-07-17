package subscribers_test

import (
	"context"
	"encoding/json"
	"testing"

	"nine/internal/embed"
	"nine/internal/memory"
	"nine/internal/subscribers"
)

type storedVec struct {
	id, namespace, key string
}

type link struct {
	agentID, relatedID string
	score              float32
}

// fakeRelatedStore records writes and returns a scripted VectorQuery result.
type fakeRelatedStore struct {
	queryResults []memory.VectorResult
	stored       []storedVec
	links        []link
}

func (f *fakeRelatedStore) VectorStore(id, namespace, key string, _ []float32) error {
	f.stored = append(f.stored, storedVec{id, namespace, key})
	return nil
}

func (f *fakeRelatedStore) VectorQuery(_ string, _ []float32, _ int) ([]memory.VectorResult, error) {
	return f.queryResults, nil
}

func (f *fakeRelatedStore) RelatedSessionAdd(agentID, relatedID string, score float32) error {
	f.links = append(f.links, link{agentID, relatedID, score})
	return nil
}

func fixedEmbedder() embed.Embedder {
	return embed.EmbedderFunc(func(_ context.Context, _ string) ([]float32, error) {
		return []float32{1, 0, 0}, nil
	})
}

func turnEnd(agentID string, seq int64, result string) memory.SessionEvent {
	payload, _ := json.Marshal(map[string]any{"result": result})
	return memory.SessionEvent{AgentID: agentID, Seq: seq, Type: "turn_end", Payload: payload}
}

func TestRelatedIndexerLinksAndIndexes(t *testing.T) {
	store := &fakeRelatedStore{
		queryResults: []memory.VectorResult{
			{Key: "A", Score: 0.99}, // self — excluded
			{Key: "B", Score: 0.90}, // linked
			{Key: "B", Score: 0.88}, // duplicate B — deduped
			{Key: "C", Score: 0.50}, // below threshold — excluded
		},
	}
	idx := subscribers.NewRelatedIndexer(store, fixedEmbedder())

	if err := idx.Handle(context.Background(), turnEnd("A", 42, "let's talk about postgres")); err != nil {
		t.Fatal(err)
	}

	// Exactly one link: A → B with B's best score.
	if len(store.links) != 1 {
		t.Fatalf("links = %+v, want exactly one (A→B)", store.links)
	}
	if store.links[0].agentID != "A" || store.links[0].relatedID != "B" || store.links[0].score != 0.90 {
		t.Errorf("link = %+v, want A→B @0.90", store.links[0])
	}

	// This turn is added to the index under the agent key with a per-event id.
	if len(store.stored) != 1 {
		t.Fatalf("stored = %+v, want one vector", store.stored)
	}
	got := store.stored[0]
	if got.namespace != "session-index" || got.key != "A" || got.id != "A:42" {
		t.Errorf("stored vector = %+v, want ns=session-index key=A id=A:42", got)
	}
}

func TestRelatedIndexerSkipsEmptyResult(t *testing.T) {
	store := &fakeRelatedStore{}
	idx := subscribers.NewRelatedIndexer(store, fixedEmbedder())
	if err := idx.Handle(context.Background(), turnEnd("A", 1, "   ")); err != nil {
		t.Fatal(err)
	}
	if len(store.stored) != 0 || len(store.links) != 0 {
		t.Errorf("empty-result turn should be a no-op; stored=%v links=%v", store.stored, store.links)
	}
}

func TestRelatedIndexerHandlerContract(t *testing.T) {
	idx := subscribers.NewRelatedIndexer(&fakeRelatedStore{}, fixedEmbedder())
	if idx.ID() != "related-session-indexer" {
		t.Errorf("ID = %q", idx.ID())
	}
	if types := idx.Types(); len(types) != 1 || types[0] != "turn_end" {
		t.Errorf("Types = %v, want [turn_end]", types)
	}
}
