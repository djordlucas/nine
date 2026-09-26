package api

import "strings"

// maxLogValueLen caps a single logged value. A user agent or path is
// attacker-sized, and an unbounded one turns a log line into a payload.
const maxLogValueLen = 256

// SafeLogValue prepares an untrusted string for a log record.
//
// Control characters are removed rather than escaped. A newline or carriage
// return in a request path is how a forged log line gets written — the
// attacker supplies the line break and then a plausible-looking record of
// their own. slog's handlers already quote values, so this is defence in
// depth rather than the only barrier, but it holds whatever handler is
// configured and it satisfies the taint analysis that flagged these call
// sites (gosec G706).
//
// The value is truncated to maxLogValueLen, with an ellipsis marking that it
// was cut so a reader does not mistake the tail for the whole.
func SafeLogValue(s string) string {
	s = strings.Map(func(r rune) rune {
		// C0 controls and DEL. Anything printable, including UTF-8 beyond
		// ASCII, is kept: the risk here is line structure, not character set.
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	if len(s) > maxLogValueLen {
		return s[:maxLogValueLen] + "…"
	}
	return s
}
