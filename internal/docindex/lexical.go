package docindex

import (
	"math"
	"sort"
	"strings"
	"sync"
	"unicode"
)

// BM25 parameters at their conventional defaults: k1 controls how fast term
// frequency saturates, b how strongly long documents are penalised for length.
const (
	bm25K1 = 1.2
	bm25B  = 0.75
)

// Lexical is a BM25 index over the bundled corpus, complementing the vector
// index rather than replacing it.
//
// It exists because the default embedder is term frequency with no inverse
// document frequency (internal/embed/keyword): every term counts the same, so a
// long vocabulary-dense section — a compatibility matrix, a reference table —
// matches any query drawn from its vocabulary and outranks the short section
// actually about the subject. IDF is exactly the missing signal: it discounts
// terms that appear everywhere ("agent", "tool", "session") and rewards the ones
// that pick a document out of the corpus.
//
// The index is built from the embedded documents, so it needs no database, no
// embedder, and no boot-time work — see [LexicalIndex].
type Lexical struct {
	addrs []string
	tf    []map[string]int
	lens  []float64
	df    map[string]int
	avgdl float64
	n     int
}

var (
	lexicalOnce sync.Once
	lexicalIdx  *Lexical
	lexicalErr  error
)

// LexicalIndex returns the process-wide BM25 index, building it on first use.
// The corpus is embedded in the binary and therefore immutable, so one build
// serves the life of the process; a daemon that never searches never pays for
// it.
func LexicalIndex() (*Lexical, error) {
	lexicalOnce.Do(func() {
		secs, err := Sections()
		if err != nil {
			lexicalErr = err
			return
		}
		lexicalIdx = newLexical(secs)
	})
	return lexicalIdx, lexicalErr
}

func newLexical(secs []Section) *Lexical {
	l := &Lexical{df: map[string]int{}, n: len(secs)}
	var total float64
	for _, s := range secs {
		// The address is indexed alongside the prose: it is kebab-cased from the
		// document name and section heading, so it carries the section's subject
		// in exactly the terms a question tends to use.
		terms := analyze(s.Addr + " " + s.Title + " " + s.Heading + " " + s.Body)
		tf := make(map[string]int, len(terms))
		for _, t := range terms {
			tf[t]++
		}
		for t := range tf {
			l.df[t]++
		}
		l.addrs = append(l.addrs, s.Addr)
		l.tf = append(l.tf, tf)
		l.lens = append(l.lens, float64(len(terms)))
		total += float64(len(terms))
	}
	if l.n > 0 {
		l.avgdl = total / float64(l.n)
	}
	return l
}

// Rank scores every section against query and returns the addresses that match
// at all, best first. Sections sharing no term with the query are omitted
// rather than returned with a zero score, so callers can treat the result as a
// candidate list.
func (l *Lexical) Rank(query string) []string {
	if l == nil || l.n == 0 {
		return nil
	}
	terms := analyze(query)
	scores := make(map[string]float64, 16)
	for i, addr := range l.addrs {
		var score float64
		for _, t := range terms {
			f := float64(l.tf[i][t])
			if f == 0 {
				continue
			}
			df := float64(l.df[t])
			idf := math.Log(1 + (float64(l.n)-df+0.5)/(df+0.5))
			score += idf * f * (bm25K1 + 1) /
				(f + bm25K1*(1-bm25B+bm25B*l.lens[i]/l.avgdl))
		}
		if score > 0 {
			scores[addr] = score
		}
	}
	ranked := make([]string, 0, len(scores))
	for addr := range scores {
		ranked = append(ranked, addr)
	}
	// Ties break on address so the ranking is deterministic across runs.
	sort.Slice(ranked, func(i, j int) bool {
		if scores[ranked[i]] != scores[ranked[j]] {
			return scores[ranked[i]] > scores[ranked[j]]
		}
		return ranked[i] < ranked[j]
	})
	return ranked
}

// analyze lowercases, splits on non-alphanumeric runs, and stems. No stop-word
// list is needed: IDF already discounts terms that appear in most documents,
// which is what a stop-word list approximates by hand.
func analyze(text string) []string {
	fields := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		out = append(out, stem(f))
	}
	return out
}

// stemSuffixes are stripped longest-first so "configurations" reduces the same
// way "configuration" does.
var stemSuffixes = []string{
	"ations", "ation", "tions", "tion", "ings", "ing",
	"ers", "ies", "ied", "es", "ed", "er", "al", "ly", "s",
}

// stem is a deliberately crude suffix stripper, not a linguistic stemmer. It
// exists for one job: letting a question's wording reach a document's wording —
// "how do I configure the database" should find "Configuration". Measured on the
// bundled corpus it is worth several places of ranking accuracy; a real stemmer
// would be a dependency for a fraction more.
//
// The length guard keeps it from mangling short words into collisions ("ties"
// into "t", "goals" into "go").
func stem(word string) string {
	for _, suf := range stemSuffixes {
		if len(word) > len(suf)+3 && strings.HasSuffix(word, suf) {
			word = strings.TrimSuffix(word, suf)
			break
		}
	}
	// A trailing "e" is dropped last, because it is what keeps the base form
	// apart from the derived one after the suffix is gone: "configuration"
	// stems to "configur" but "configure" would stay whole, and the two would
	// never meet — which is precisely the query/document pair this is for.
	if len(word) > 4 && strings.HasSuffix(word, "e") {
		word = strings.TrimSuffix(word, "e")
	}
	return word
}

// FuseRRF merges ranked address lists by reciprocal rank fusion:
// score(d) = Σ 1/(k + rank(d)) over the lists containing d.
//
// Fusing ranks rather than scores is what makes this safe. A cosine similarity
// and a BM25 score have no common scale — BM25 is unbounded and shifts with
// query length — so any weighted sum of the two would need normalisation that
// is itself arbitrary. Ranks are directly comparable, and a document missing
// from one list simply contributes nothing from it, which is the behaviour we
// want when the two retrievers disagree about what is even a candidate.
func FuseRRF(k float64, lists ...[]string) []string {
	scores := map[string]float64{}
	for _, list := range lists {
		for i, addr := range list {
			scores[addr] += 1 / (k + float64(i+1))
		}
	}
	fused := make([]string, 0, len(scores))
	for addr := range scores {
		fused = append(fused, addr)
	}
	sort.Slice(fused, func(i, j int) bool {
		if scores[fused[i]] != scores[fused[j]] {
			return scores[fused[i]] > scores[fused[j]]
		}
		return fused[i] < fused[j]
	})
	return fused
}

// RRFK is the rank-fusion constant. Lower values concentrate weight on the very
// top of each input ranking; the conventional default is 60. Swept against a
// labelled query set over the bundled corpus, 10 was the better choice here —
// both retrievers are decent, so trusting their top few pays off. That sweep was
// 30 queries, so treat this as a tuned default rather than an optimum.
const RRFK = 10
