package memory

import "time"

// timeLayout is the one and only on-disk timestamp format.
//
// Every property of it is load-bearing, because SQLite compares TEXT bytewise —
// `ORDER BY created_at` and `stored_at < ?` are string comparisons:
//
//	fixed width       a variable-width format breaks ordering outright.
//	                  time.RFC3339Nano trims trailing zeros, so "…:05Z" sorts
//	                  *after* "…:05.5Z" ('Z' > '.'). Every value must be the
//	                  same length.
//	always UTC        likewise: a mix of "Z" and "+02:00" would sort by offset
//	                  text rather than by instant.
//	microseconds      matches the resolution of the TIMESTAMPTZ columns this
//	                  replaces. Second resolution would collapse rows written in
//	                  a loop and make `ORDER BY created_at` non-deterministic,
//	                  which several tests depend on.
//	RFC3339-parseable time.Parse(time.RFC3339, …) accepts it, so the existing
//	                  expires_at parsing in hitl.go keeps working unchanged.
const timeLayout = "2006-01-02T15:04:05.000000Z07:00"

// nowExpr is the DDL default for a timestamp column: the exact shape writeTime
// produces, so a value defaulted by SQL sorts correctly against one written by
// Go. strftime's %f stops at milliseconds, hence the three literal trailing
// zeros — without them a SQL-written value would be 23 characters against Go's
// 27, and two timestamps in the same millisecond would compare on the wrong
// byte. strftime('now') is UTC.
//
// The default is a safety net only: every write path supplies the value
// explicitly via writeTime or nowText.
const nowExpr = `(strftime('%Y-%m-%dT%H:%M:%f000Z','now'))`

// writeTime renders t for storage. Every timestamp bound as a query argument
// must go through here — see checkArgs for what happens otherwise.
func writeTime(t time.Time) string { return t.UTC().Format(timeLayout) }

// nowText renders the current instant for storage.
func nowText() string { return writeTime(time.Now()) }

// parseStoredTime converts a stored timestamp back to a time.Time. Everything
// nine writes goes through writeTime, so a parse failure means a hand-edited
// row: report the zero time rather than failing an otherwise-good read.
func parseStoredTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}
