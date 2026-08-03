package memory

import (
	"fmt"
	"strconv"
	"strings"
)

// MemoriesNamespace is the shared pgvector namespace that agent key-value
// memories are mirrored into (one vector per KV key, embedding of the value).
// The context builder queries it to pull-surface memories relevant to the
// current turn. It is a single shared pool — memories are not isolated per
// agent — so any session can surface any recorded memory.
const MemoriesNamespace = "memories"

// DocsNamespace is the pgvector namespace holding one vector per section of the
// documentation and specification embedded in the binary. Keys are docindex
// addresses ("docs/skills.md#tools") and the vectors are a pure index: section
// text is never copied into the store, it is sliced back out of the embedded FS
// on read, so the docs Nine cites are always the ones its own version ships.
// The seeder (runtime.SeedDocs) writes it; doc_search ranks against it.
const DocsNamespace = "docs"

// formatVector renders a float32 slice as a pgvector literal, e.g. "[0.1,0.2]".
func formatVector(v []float32) string {
	var b strings.Builder
	b.Grow(len(v)*8 + 2)
	b.WriteByte('[')
	for i, f := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(f), 'g', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}

// VectorStore saves or replaces a vector in the given namespace under id/key.
func (s *Store) VectorStore(id, namespace, key string, vector []float32) error {
	if len(vector) == 0 {
		return fmt.Errorf("vector must not be empty")
	}
	_, err := s.db.Exec(
		`INSERT INTO vectors(id, namespace, key, embedding, dim, stored_at)
		 VALUES(?,?,?,?::vector,?,now())
		 ON CONFLICT(id) DO UPDATE SET
		   namespace=excluded.namespace, key=excluded.key,
		   embedding=excluded.embedding, dim=excluded.dim,
		   stored_at=now()`,
		id, namespace, key, formatVector(vector), len(vector))
	return err
}

// VectorDelete removes the vector with the given id, if present.
func (s *Store) VectorDelete(id string) error {
	_, err := s.db.Exec(`DELETE FROM vectors WHERE id = ?`, id)
	return err
}

// VectorDeleteNamespace removes every vector in namespace, returning how many
// were deleted. It backs wholesale reindexing of a derived namespace — one
// whose contents are a pure function of some other source of truth (the docs
// embedded in the binary), where rebuilding is cheaper and less error-prone
// than diffing against what is already stored.
func (s *Store) VectorDeleteNamespace(namespace string) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM vectors WHERE namespace = ?`, namespace)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// VectorResult is one result from a nearest-neighbour query.
type VectorResult struct {
	Key   string  `json:"key"`
	Score float32 `json:"score"`
}

// VectorQuery returns the topK most similar vectors in namespace, ranked by
// cosine similarity. pgvector's `<=>` yields cosine distance, so similarity is
// 1 - distance. Only vectors of matching dimensionality are considered.
func (s *Store) VectorQuery(namespace string, vector []float32, topK int) ([]VectorResult, error) {
	if topK <= 0 {
		topK = 5
	}
	lit := formatVector(vector)
	rows, err := s.db.Query(
		`SELECT key, 1 - (embedding <=> ?::vector) AS score
		 FROM vectors
		 WHERE namespace = ? AND dim = ?
		 ORDER BY embedding <=> ?::vector
		 LIMIT ?`,
		lit, namespace, len(vector), lit, topK)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	results := []VectorResult{}
	for rows.Next() {
		var r VectorResult
		if err := rows.Scan(&r.Key, &r.Score); err != nil {
			return nil, err
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

// VectorCount returns the number of vectors stored in namespace. It backs
// count-based assertions (e.g. eval side-effects: "at least one vector was
// indexed into the skills namespace") that need a total rather than a ranked
// nearest-neighbour result.
func (s *Store) VectorCount(namespace string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT count(*) FROM vectors WHERE namespace = ?`, namespace).Scan(&n)
	return n, err
}
