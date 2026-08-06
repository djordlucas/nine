package memory

import (
	"encoding/binary"
	"fmt"
	"math"
	"sort"
)

// MemoriesNamespace is the shared vector namespace that agent key-value
// memories are mirrored into (one vector per KV key, embedding of the value).
// The context builder queries it to pull-surface memories relevant to the
// current turn. It is a single shared pool — memories are not isolated per
// agent — so any session can surface any recorded memory.
const MemoriesNamespace = "memories"

// DocsNamespace is the vector namespace holding one vector per section of the
// documentation and specification embedded in the binary. Keys are docindex
// addresses ("docs/skills.md#tools") and the vectors are a pure index: section
// text is never copied into the store, it is sliced back out of the embedded FS
// on read, so the docs Nine cites are always the ones its own version ships.
// The seeder (runtime.SeedDocs) writes it; doc_search ranks against it.
const DocsNamespace = "docs"

// encodeVector packs a float32 slice into a BLOB, little-endian, 4 bytes per
// component. The fixed stride means the dimensionality is derivable from
// len(blob)/4, which decodeVector uses to catch a truncated row rather than
// trusting the dim column.
func encodeVector(v []float32) []byte {
	b := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(f))
	}
	return b
}

// decodeVector unpacks a BLOB into dst, reusing dst's backing array when it is
// large enough. The reuse is the point: VectorQuery decodes every candidate in
// a namespace, and a fresh allocation per row would dominate the scan.
func decodeVector(b []byte, dst []float32) ([]float32, error) {
	if len(b)%4 != 0 {
		return nil, fmt.Errorf("vector blob length %d is not a multiple of 4", len(b))
	}
	n := len(b) / 4
	if cap(dst) < n {
		dst = make([]float32, n)
	}
	dst = dst[:n]
	for i := range dst {
		dst[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}
	return dst, nil
}

// norm returns the Euclidean length of v, accumulated in float64.
func norm(v []float32) float64 {
	var n float64
	for _, f := range v {
		n += float64(f) * float64(f)
	}
	return math.Sqrt(n)
}

// cosine returns the cosine similarity of a and b, given a's precomputed norm.
// The accumulators are float64 even though the data is float32, which keeps the
// ordering of near-ties stable. The clamp stops floating-point slop from
// surfacing a 1.0000001 in model-visible output.
func cosine(a []float32, an float64, b []float32) float32 {
	var dot, bn float64
	for i := range a {
		x, y := float64(a[i]), float64(b[i])
		dot += x * y
		bn += y * y
	}
	if bn == 0 {
		// A zero vector has no direction, so similarity is undefined. Report 0
		// rather than NaN: it sorts sanely and marshals to JSON.
		return 0
	}
	return float32(math.Max(-1, math.Min(1, dot/(an*math.Sqrt(bn)))))
}

// ranker keeps the K highest-scoring results in a descending sorted slice.
// container/heap would be asymptotically tidier, but K is 5–10 in practice: a
// linear insert has no interface dispatch, stays in cache, and — for the common
// case of a candidate that does not make the cut — costs exactly one comparison
// against the current worst. It also yields results already sorted, which a
// heap does not.
type ranker struct {
	k    int
	best []VectorResult
}

func (t *ranker) add(r VectorResult) {
	if len(t.best) == t.k && r.Score <= t.best[len(t.best)-1].Score {
		return
	}
	i := sort.Search(len(t.best), func(i int) bool { return t.best[i].Score < r.Score })
	if len(t.best) < t.k {
		t.best = append(t.best, VectorResult{})
	}
	copy(t.best[i+1:], t.best[i:])
	t.best[i] = r
}

func (t *ranker) results() []VectorResult {
	if t.best == nil {
		return []VectorResult{}
	}
	return t.best
}

// VectorStore saves or replaces a vector in the given namespace under id/key.
func (s *Store) VectorStore(id, namespace, key string, vector []float32) error {
	if len(vector) == 0 {
		return fmt.Errorf("vector must not be empty")
	}
	_, err := s.db.Exec(
		`INSERT INTO vectors(id, namespace, key, embedding, dim, stored_at)
		 VALUES(?,?,?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET
		   namespace=excluded.namespace, key=excluded.key,
		   embedding=excluded.embedding, dim=excluded.dim,
		   stored_at=excluded.stored_at`,
		id, namespace, key, encodeVector(vector), len(vector), nowText())
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
// cosine similarity. Only vectors of matching dimensionality are considered.
//
// The similarity is computed here rather than in SQL. That is not a regression:
// the index on (namespace, dim) is a plain btree, never an ANN index, so
// ranking has always been a filtered sequential scan — the arithmetic has
// simply moved to this side of the driver boundary, where it costs one blob
// decode per candidate and no round trip.
func (s *Store) VectorQuery(namespace string, vector []float32, topK int) ([]VectorResult, error) {
	if topK <= 0 {
		topK = 5
	}
	qn := norm(vector)
	if len(vector) == 0 || qn == 0 {
		return []VectorResult{}, nil
	}

	rows, err := s.db.Query(
		`SELECT key, embedding FROM vectors WHERE namespace = ? AND dim = ?`,
		namespace, len(vector))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	sel := ranker{k: topK}
	scratch := make([]float32, 0, len(vector))
	for rows.Next() {
		var (
			key  string
			blob []byte
		)
		if err := rows.Scan(&key, &blob); err != nil {
			return nil, err
		}
		scratch, err = decodeVector(blob, scratch)
		// A malformed or wrong-length row is skipped rather than fatal: a ranked
		// search that returns slightly fewer neighbours is a far better failure
		// than a context build that errors out over one corrupt embedding.
		if err != nil || len(scratch) != len(vector) {
			continue
		}
		sel.add(VectorResult{Key: key, Score: cosine(vector, qn, scratch)})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return sel.results(), nil
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
