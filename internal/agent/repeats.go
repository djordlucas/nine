package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// Repeated calls: a model that has lost track of what it already did — its
// context full, its earlier calls cut — may make the same call again and
// again within one turn. A goal session was journaled calling goal_get 13
// times with the same arguments in one turn, each call another model request
// over a full context. The loop counts identical calls that return the same
// result: from RepeatNoticeAt on, the result says so, and at RepeatStopAt the
// turn ends.

// RepeatNoticeAt is the identical call, with the same result, whose
// observation tells the model the result will not change.
const RepeatNoticeAt = 3

// RepeatStopAt is the identical call, with the same result, that ends the turn.
const RepeatStopAt = 5

// repeatGuard counts, within one turn, the calls of each tool with each input
// that returned the same observation as the one before.
type repeatGuard struct {
	seen map[string]*repeatRun
}

type repeatRun struct {
	n           int
	observation string
}

// note records a call and returns how many times in a row this tool, with
// this input, has returned this observation. A different observation starts
// the count again: a call whose result changes is not stuck.
func (g *repeatGuard) note(name string, input json.RawMessage, observation string) int {
	if g.seen == nil {
		g.seen = make(map[string]*repeatRun)
	}
	key := name + "\x00" + canonicalJSON(input)
	r, ok := g.seen[key]
	if !ok || r.observation != observation {
		g.seen[key] = &repeatRun{n: 1, observation: observation}
		return 1
	}
	r.n++
	return r.n
}

// canonicalJSON compacts input so whitespace does not make two calls differ.
func canonicalJSON(input json.RawMessage) string {
	var b bytes.Buffer
	if err := json.Compact(&b, input); err != nil {
		return string(input)
	}
	return b.String()
}

// repeatNotice is appended to the observation of a repeated call.
func repeatNotice(name string, n int) string {
	return fmt.Sprintf("Note: this is call %d of %s with the same arguments this turn, and it returned the same result "+
		"as before. Calling it again will not change it. Use what you already have, take a different action, or answer now; "+
		"at %d identical calls the turn ends.", n, name, RepeatStopAt)
}

// repeatStopAnswer is the turn's answer when it ends on a repeated call.
func repeatStopAnswer(name string, n int) string {
	return fmt.Sprintf("Stopped this turn: %s was called %d times with the same arguments and kept returning the same result.", name, n)
}
