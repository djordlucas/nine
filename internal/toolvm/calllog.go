package toolvm

import (
	"context"
	"strings"
	"sync"
)

// Bounds on what one call's log buffer keeps. Small on purpose: this is read by
// a language model inside a failure message, not by an operator tailing a log —
// the daemon log still receives every line in full.
const (
	// maxLoggedLines is how many of the most recent lines survive. A tool that
	// prints in a loop is usually most informative just before it threw.
	maxLoggedLines = 8
	// maxLoggedLineBytes truncates one very long line rather than dropping it: a
	// 40 KB stringified object still tells the model which field was wrong.
	maxLoggedLineBytes = 256
	// maxLoggedBytes caps the whole block, so eight long lines cannot crowd out
	// the error that actually explains the failure.
	maxLoggedBytes = 1024
)

// callLog buffers what one call printed through `nine.log`, so a failure can say
// what the tool saw on the way to failing.
//
// It exists because a failing tool returns its thrown error and nothing else:
// everything it printed through `console` reached the daemon log, where the
// model cannot see it, and was otherwise thrown away. For a small model, "here
// are the values printed before the throw" is often the difference between a
// one-turn repair and several turns of guessing at its own code.
//
// It confers nothing. The tool already printed these lines through an import
// granted to every tool; this only stops discarding them when they turn out to
// matter.
type callLog struct {
	mu    sync.Mutex
	lines []string
}

// add records one line, keeping the most recent maxLoggedLines.
func (l *callLog) add(msg string) {
	if l == nil {
		return
	}
	msg = strings.TrimRight(msg, "\n")
	if len(msg) > maxLoggedLineBytes {
		// Cut on a rune boundary: the buffer ends up in a JSON error message, and
		// a severed multi-byte sequence would make it invalid UTF-8.
		msg = strings.ToValidUTF8(msg[:maxLoggedLineBytes], "") + "…"
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, msg)
	if len(l.lines) > maxLoggedLines {
		l.lines = l.lines[len(l.lines)-maxLoggedLines:]
	}
}

// taken returns the buffered lines, oldest first, within maxLoggedBytes. Dropping
// from the front keeps the lines nearest the failure, which are the ones that
// explain it.
func (l *callLog) taken() []string {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	total := 0
	first := len(l.lines)
	for i := len(l.lines) - 1; i >= 0; i-- {
		total += len(l.lines[i]) + 1
		if total > maxLoggedBytes {
			break
		}
		first = i
	}
	if first >= len(l.lines) {
		return nil
	}
	out := make([]string, len(l.lines)-first)
	copy(out, l.lines[first:])
	return out
}

// callLogKey carries the current call's buffer into the `log` host function,
// the same way a grant travels: one shared import, one buffer per call.
type callLogKey struct{}

// logFor returns the calling call's buffer, or nil outside a call.
func logFor(ctx context.Context) *callLog {
	l, _ := ctx.Value(callLogKey{}).(*callLog)
	return l
}
