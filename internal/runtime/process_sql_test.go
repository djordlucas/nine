package runtime

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"nine/internal/memory"
	"nine/internal/memory/memtest"
	"nine/internal/sqldb"
)

// A process's writes to the database go through, and each is journaled under
// the process as a summary — kinds and tables, never values. A denied
// statement is refused with its reason.
func TestProcessSQLIsJournaledAsSummaries(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sqldb.Open(filepath.Join(t.TempDir(), "database.sqlite"), sqldb.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	r := NewStandingRunner(store, standingHost(t, "eventrelay", eventRelayManifest, eventRelaySource, nil), 1, 2)
	r.SetDatabase(db)
	h := &liveHandler{r: r, lp: &liveProc{row: memory.Process{ID: "gen:digest", SessionID: "gen:digest"}}}
	ctx := context.Background()

	for _, q := range []string{
		"CREATE TABLE seen (url TEXT PRIMARY KEY, meta TEXT)",
		"INSERT INTO seen VALUES (?, ?)",
	} {
		if _, err := h.SQL(ctx, true, q, []any{"https://secret.example/a", map[string]any{"k": "SECRET-VALUE"}}[:strings.Count(q, "?")]); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	out, err := h.SQL(ctx, false, "SELECT meta FROM seen", nil)
	if err != nil || !strings.Contains(string(out), `SECRET-VALUE`) {
		t.Fatalf("query = %s, %v; want the object stored as JSON", out, err)
	}
	if _, err := h.SQL(ctx, false, "DELETE FROM seen", nil); err == nil || !strings.Contains(err.Error(), "denied") {
		t.Errorf("a write through query: err = %v, want a denial", err)
	}
	if _, err := h.SQL(ctx, true, "ATTACH DATABASE 'x' AS x", nil); err == nil || !strings.Contains(err.Error(), "denied: attach") {
		t.Errorf("ATTACH: err = %v", err)
	}

	evs, err := store.SessionEventsByAgent("gen:digest")
	if err != nil {
		t.Fatal(err)
	}
	var journal []string
	for _, ev := range evs {
		if ev.Type == evSQLWrite {
			journal = append(journal, string(ev.Payload))
		}
	}
	all := strings.Join(journal, "\n")
	if len(journal) != 2 || !strings.Contains(all, `"create table"`) || !strings.Contains(all, `"insert into"`) {
		t.Errorf("journal = %v, want a create and an insert", journal)
	}
	if strings.Contains(all, "SECRET") {
		t.Errorf("the journal carries values: %s", all)
	}

	r.SetDatabase(nil)
	if _, err := h.SQL(ctx, false, "SELECT 1", nil); err == nil || !strings.Contains(err.Error(), "no database") {
		t.Errorf("without a database: err = %v", err)
	}
}
