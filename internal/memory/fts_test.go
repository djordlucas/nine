package memory_test

import (
	"strings"
	"testing"

	"nine/internal/memory"
	"nine/internal/memory/memtest"
)

// The Postgres full-text search this replaced accepted literally any string:
// websearch_to_tsquery never errored. FTS5's MATCH parser does, on input a model
// produces routinely. These are the shapes that used to be free and must stay
// free.
var ftsHostileInputs = []string{
	``,
	` `,
	`"`,
	`""`,
	`"unterminated`,
	`a" OR "b`,
	`*`,
	`*foo`,
	`AND`,
	`a AND`,
	`OR OR OR`,
	`NOT`,
	`NEAR`,
	`NEAR(a b)`,
	`((`,
	`)`,
	`a:b`,
	`nosuchcolumn:value`,
	`^`,
	`^a`,
	`-`,
	`- -`,
	`--`,
	`{}`,
	`a AND (b OR`,
	`😀`,
	`"" ""`,
	`. , ; !`,
	strings.Repeat(`"`, 50),
}

// TestFileSearchTextNeverErrors is the core guarantee of the query sanitizer:
// no user input, however malformed, can turn a search into an error. A model
// composing a query cannot be trusted to produce valid FTS5 syntax, and under
// the previous backend it never had to.
func TestFileSearchTextNeverErrors(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FileStore("notes/db.md", "PostgreSQL is a relational database engine"); err != nil {
		t.Fatal(err)
	}

	for _, in := range ftsHostileInputs {
		t.Run(in, func(t *testing.T) {
			if _, err := store.FileSearchText(in, 5); err != nil {
				t.Errorf("FileSearchText(%q) returned error %v; malformed input must yield no results, not an error", in, err)
			}
			if _, err := store.FileSearchTextScoped(in, "notes/", 5); err != nil {
				t.Errorf("FileSearchTextScoped(%q) returned error %v", in, err)
			}
		})
	}
}

func FuzzFileSearchText(f *testing.F) {
	for _, in := range ftsHostileInputs {
		f.Add(in)
	}
	f.Add("database")
	f.Add(`"exact phrase" -excluded or other`)

	store, err := memtest.Open(f)
	if err != nil {
		f.Fatal(err)
	}
	if err := store.FileStore("notes/db.md", "PostgreSQL is a relational database engine"); err != nil {
		f.Fatal(err)
	}

	f.Fuzz(func(t *testing.T, query string) {
		if _, err := store.FileSearchText(query, 5); err != nil {
			t.Fatalf("FileSearchText(%q) = %v; want no error for any input", query, err)
		}
	})
}

// TestFTSQuerySemantics pins the websearch-compatible reading of a query, so
// the sanitizer stays a translation rather than drifting into its own dialect.
func TestFTSQuerySemantics(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"bare words are ANDed", `alpha beta`, `"alpha" AND "beta"`},
		{"quoted phrase stays one term", `"alpha beta"`, `"alpha beta"`},
		{"or joins neighbours", `alpha or beta`, `"alpha" OR "beta"`},
		{"OR is case-insensitive", `alpha OR beta`, `"alpha" OR "beta"`},
		{"leading dash excludes", `alpha -beta`, `"alpha" NOT "beta"`},
		{"operators inside a term are inert", `a*b`, `"a*b"`},
		{"a quote splits a term, as websearch did", `say"hi`, `"say" AND "hi"`},
		{"quoted or is a literal word", `alpha "or" beta`, `"alpha" AND "or" AND "beta"`},
		{"unsearchable input yields nothing", `. , ;`, ``},
		{"pure negation yields nothing", `-alpha`, ``},
		{"dangling or is dropped", `alpha or`, `"alpha"`},
		{"leading or is dropped", `or alpha`, `"alpha"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := memory.FTSQuery(tt.in); got != tt.want {
				t.Errorf("FTSQuery(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestFileSearchTextFindsAndScopes covers the happy path the sanitizer must not
// break: a real match, a snippet with the configured delimiters, and the path
// scope actually narrowing.
func TestFileSearchTextFindsAndScopes(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FileStore("notes/db.md", "PostgreSQL is a powerful relational database engine"); err != nil {
		t.Fatal(err)
	}
	if err := store.FileStore("spill/other.md", "the database of cats is unrelated"); err != nil {
		t.Fatal(err)
	}

	hits, err := store.FileSearchText("database", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("FileSearchText(database) returned %d hits, want 2", len(hits))
	}
	if !strings.Contains(hits[0].Snippet, "[") || !strings.Contains(hits[0].Snippet, "]") {
		t.Errorf("snippet %q lacks the [ ] match delimiters", hits[0].Snippet)
	}

	scoped, err := store.FileSearchTextScoped("database", "notes/", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(scoped) != 1 || scoped[0].Path != "notes/db.md" {
		t.Fatalf("scoped search = %+v, want only notes/db.md", scoped)
	}
}

// TestFileSearchTextStems guards the tokenizer choice. The Postgres 'english'
// configuration stemmed, so a search for the plural found the singular; the
// porter tokenizer is what preserves that.
func TestFileSearchTextStems(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FileStore("notes/db.md", "a relational database engine"); err != nil {
		t.Fatal(err)
	}
	hits, err := store.FileSearchText("databases", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("searching %q found %d hits, want 1 — the tokenizer must stem", "databases", len(hits))
	}
}

// TestFileSearchIndexFollowsWrites covers the three FTS triggers: an update must
// re-derive the index, and a delete must retract it. Getting the 'delete'
// command row wrong leaves an external-content index returning stale rowids.
func TestFileSearchIndexFollowsWrites(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FileStore("notes/a.md", "original kangaroo content"); err != nil {
		t.Fatal(err)
	}

	// Overwrite: the old term must stop matching and the new one start.
	if err := store.FileStore("notes/a.md", "replaced platypus content"); err != nil {
		t.Fatal(err)
	}
	if hits, err := store.FileSearchText("kangaroo", 5); err != nil || len(hits) != 0 {
		t.Errorf("after overwrite, kangaroo returned %d hits (err %v), want 0 — the update trigger did not retract", len(hits), err)
	}
	if hits, err := store.FileSearchText("platypus", 5); err != nil || len(hits) != 1 {
		t.Errorf("after overwrite, platypus returned %d hits (err %v), want 1", len(hits), err)
	}

	// Delete: the retention sweep removes rows with a bare DELETE, never going
	// through any FTS-aware Go code, so the delete trigger is the only thing
	// keeping the index honest. Exercise it the same way.
	if err := store.ExecRaw(`DELETE FROM files WHERE path = ?`, "notes/a.md"); err != nil {
		t.Fatal(err)
	}
	if hits, err := store.FileSearchText("platypus", 5); err != nil || len(hits) != 0 {
		t.Errorf("after delete, platypus returned %d hits (err %v), want 0 — the delete trigger did not retract", len(hits), err)
	}
}
