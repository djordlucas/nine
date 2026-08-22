package memory

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// ToolStateQuota bounds one (tool, scope) namespace. The host resolves these
// from the operator's grant before calling in; a zero field is unenforced.
// Deciding a default here would put a policy decision in the wrong layer.
type ToolStateQuota struct {
	MaxKeys    int
	MaxValueKB int
	MaxTotalKB int
	// TTL is how long a written value lives. Zero means it never expires.
	TTL time.Duration
}

// ToolStateQuotaError is a quota refusal.
//
// It is a distinct type because the host hands it back to the guest as a
// catchable error rather than failing the call: a tool that has filled its store
// should be able to evict and retry, and a silent drop would leave it believing
// it remembered something it did not
// (adr/durable-and-long-running-tools.md §3.4).
type ToolStateQuotaError struct{ Reason string }

func (e *ToolStateQuotaError) Error() string { return e.Reason }

// ToolStateGet returns a value, or (…, false, nil) when it is absent or expired.
//
// Expired rows are filtered here as well as swept (ToolStateExpire) because the
// sweep is periodic: without this filter a TTL would mean nothing until the next
// tick, which is not what an author who wrote one expects. The sweep reclaims
// the space; this makes the value invisible on time.
func (s *Store) ToolStateGet(tool, scopeKey, key string) (string, bool, error) {
	var v string
	err := s.db.QueryRow(
		`SELECT value FROM tool_state
		 WHERE tool = ? AND scope_key = ? AND key = ?
		   AND (expires_at = '' OR expires_at > ?)`,
		tool, scopeKey, key, nowText()).Scan(&v)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}

// ToolStateSet writes a value, enforcing q. It replaces an existing key.
//
// The count-then-write runs in one transaction on the writer pool: two
// concurrent calls to the same tool are ordinary — a module is instantiated per
// call, not per tool — so checking the quota outside the write would let both
// observe room for the last key and both take it.
func (s *Store) ToolStateSet(tool, scopeKey, key, value string, q ToolStateQuota) error {
	if err := checkValueSize(value, q); err != nil {
		return err
	}
	tx, err := s.db.BeginWrite()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op once Commit succeeds

	keys, total, err := scopeUsageTx(tx, tool, scopeKey, key)
	if err != nil {
		return err
	}
	if err := checkScopeQuota(keys, total, len(value), key, q); err != nil {
		return err
	}
	if err := putStateTx(tx, tool, scopeKey, key, value, q.TTL); err != nil {
		return err
	}
	return tx.Commit()
}

// ToolStateDelete removes a key. Deleting an absent key is not an error: a tool
// clearing state it may or may not have written should not have to look first.
func (s *Store) ToolStateDelete(tool, scopeKey, key string) error {
	_, err := s.db.Exec(
		`DELETE FROM tool_state WHERE tool = ? AND scope_key = ? AND key = ?`,
		tool, scopeKey, key)
	return err
}

// ToolStateList returns the live keys under prefix, sorted. An empty prefix
// lists the whole scope. Expired rows are omitted, per ToolStateGet.
func (s *Store) ToolStateList(tool, scopeKey, prefix string) ([]string, error) {
	rows, err := s.db.Query(
		`SELECT key FROM tool_state
		 WHERE tool = ? AND scope_key = ?
		   AND (? = '' OR key LIKE ? ESCAPE '\')
		   AND (expires_at = '' OR expires_at > ?)
		 ORDER BY key`,
		tool, scopeKey, prefix, likePrefix(prefix), nowText())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// ToolStateSwap is compare-and-set: it writes value only if the current value
// equals expected, or — when expected is nil — only if the key is absent. It
// reports whether the write took.
//
// This is not a convenience. Two turns calling one tool at once is ordinary, and
// a read-modify-write spanning two host calls is racy by construction, so
// without a compare-and-set the first non-trivial use of this capability is a
// bug the author cannot fix (adr/durable-and-long-running-tools.md §3.1).
func (s *Store) ToolStateSwap(tool, scopeKey, key string, expected *string, value string, q ToolStateQuota) (bool, error) {
	if err := checkValueSize(value, q); err != nil {
		return false, err
	}
	tx, err := s.db.BeginWrite()
	if err != nil {
		return false, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op once Commit succeeds

	var cur string
	err = tx.QueryRow(
		`SELECT value FROM tool_state
		 WHERE tool = ? AND scope_key = ? AND key = ?
		   AND (expires_at = '' OR expires_at > ?)`,
		tool, scopeKey, key, nowText()).Scan(&cur)
	exists := true
	switch {
	case err == sql.ErrNoRows:
		// An expired row is absent for the comparison and is overwritten by the
		// write below — the same thing ToolStateGet reports.
		exists = false
	case err != nil:
		return false, err
	}

	if expected == nil {
		if exists {
			return false, nil
		}
	} else if !exists || cur != *expected {
		return false, nil
	}

	keys, total, err := scopeUsageTx(tx, tool, scopeKey, key)
	if err != nil {
		return false, err
	}
	if err := checkScopeQuota(keys, total, len(value), key, q); err != nil {
		return false, err
	}
	if err := putStateTx(tx, tool, scopeKey, key, value, q.TTL); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// ToolStateExpire drops every expired row and returns how many. Run on the
// sweeper's tick: expiry is reclaimed on a clock rather than on read, so a tool
// cannot keep a value alive by never looking at it.
func (s *Store) ToolStateExpire() (int, error) {
	res, err := s.db.Exec(
		`DELETE FROM tool_state WHERE expires_at <> '' AND expires_at <= ?`, nowText())
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// ToolStateDropScope removes every key one scope holds. It is how a tool's state
// is reclaimed when whatever it was scoped to goes away — a deleted
// conversation, or a generated tool evicted from the catalog.
func (s *Store) ToolStateDropScope(tool, scopeKey string) error {
	_, err := s.db.Exec(
		`DELETE FROM tool_state WHERE tool = ? AND scope_key = ?`, tool, scopeKey)
	return err
}

// ToolStateUsage reports one scope's live key count and byte total, for
// `nine tools show`. An operator diagnosing a tool that has stopped remembering
// wants to see it sitting against its quota.
func (s *Store) ToolStateUsage(tool, scopeKey string) (keys, bytes int, err error) {
	err = s.db.QueryRow(
		`SELECT count(*), coalesce(sum(size), 0) FROM tool_state
		 WHERE tool = ? AND scope_key = ? AND (expires_at = '' OR expires_at > ?)`,
		tool, scopeKey, nowText()).Scan(&keys, &bytes)
	return keys, bytes, err
}

// scopeUsageTx counts the scope's keys and bytes, excluding `except`.
//
// The row being replaced counts against neither budget, which is why it is
// excluded: without that, rewriting the single key of a one-key quota would
// refuse itself.
func scopeUsageTx(tx *sql.Tx, tool, scopeKey, except string) (keys, total int, err error) {
	err = tx.QueryRow(
		`SELECT count(*), coalesce(sum(size), 0) FROM tool_state
		 WHERE tool = ? AND scope_key = ? AND key <> ?`,
		tool, scopeKey, except).Scan(&keys, &total)
	return keys, total, err
}

func putStateTx(tx *sql.Tx, tool, scopeKey, key, value string, ttl time.Duration) error {
	_, err := tx.Exec(
		`INSERT INTO tool_state(tool, scope_key, key, value, size, updated_at, expires_at)
		 VALUES(?,?,?,?,?,?,?)
		 ON CONFLICT(tool, scope_key, key) DO UPDATE SET
		   value = excluded.value, size = excluded.size,
		   updated_at = excluded.updated_at, expires_at = excluded.expires_at`,
		tool, scopeKey, key, value, len(value), nowText(), expiryText(ttl))
	return err
}

func checkScopeQuota(keys, total, incoming int, key string, q ToolStateQuota) error {
	if q.MaxKeys > 0 && keys+1 > q.MaxKeys {
		return &ToolStateQuotaError{Reason: fmt.Sprintf(
			"state quota: this tool may hold %d keys and already holds %d; delete one before writing %q",
			q.MaxKeys, keys, key)}
	}
	if q.MaxTotalKB > 0 && total+incoming > q.MaxTotalKB*1024 {
		return &ToolStateQuotaError{Reason: fmt.Sprintf(
			"state quota: this tool may hold %d KB and writing %q would bring it to %d bytes",
			q.MaxTotalKB, key, total+incoming)}
	}
	return nil
}

func checkValueSize(value string, q ToolStateQuota) error {
	if q.MaxValueKB > 0 && len(value) > q.MaxValueKB*1024 {
		return &ToolStateQuotaError{Reason: fmt.Sprintf(
			"state quota: a value may be %d KB and this one is %d bytes", q.MaxValueKB, len(value))}
	}
	return nil
}

// likePrefix turns a literal prefix into a LIKE pattern, escaping the wildcards
// so a key containing % or _ matches literally. These keys are chosen by tool
// code rather than by an operator, so "list everything under x_" silently
// meaning "x followed by anything" is a real way to be surprised.
func likePrefix(prefix string) string {
	r := strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`)
	return r.Replace(prefix) + "%"
}

// expiryText renders a TTL as the absolute instant the row dies, or empty for
// never — absolute rather than relative, so expiry does not depend on when the
// row is next read.
func expiryText(ttl time.Duration) string {
	if ttl <= 0 {
		return ""
	}
	return writeTime(time.Now().Add(ttl))
}
