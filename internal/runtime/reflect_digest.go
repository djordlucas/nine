package runtime

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"nine/internal/agent"
	"nine/internal/memory"
)

// Reflection's digest (docs/self-model.md): a reflect process's tick used to
// ask the session to "reflect on your recent sessions" while giving it nothing
// about them, so its notes said only that a cycle ran — 48 times a day, idle
// or not. The runner now reads the journal since the last reflection and hands
// the process a digest: what ran, what failed, what was paused or stopped. A
// tick with nothing new to read is skipped and costs nothing.

// digestPageSize and digestMaxEvents bound one digest's journal read.
const (
	digestPageSize  = 2000
	digestMaxEvents = 50_000
)

// digestLineCap bounds how many items each digest list names.
const digestLineCap = 5

// activityDigest reads the journal after seq, leaving out the reflecting
// session's own events, and returns a digest of it and the last seq read. The
// digest is empty when no other session ran a turn and no process or goal
// changed.
func (r *StandingRunner) activityDigest(self string, after int64) (string, int64, error) {
	var (
		last      = after
		turns     = map[string]int{}
		tokens    = map[string]int{}
		errs      = map[string]int{}
		toolFails = map[string]int{}
		repeats   []string
		changes   []string
		read      int
	)
	for read < digestMaxEvents {
		evs, err := r.store.SessionEventsAfter(last, digestPageSize)
		if err != nil {
			return "", after, err
		}
		for _, e := range evs {
			last = e.Seq
			if e.AgentID == self {
				continue
			}
			var p map[string]any
			_ = json.Unmarshal(e.Payload, &p) //nolint:errcheck // a payload that is not an object reads as empty
			switch e.Type {
			case "turn_end":
				turns[e.AgentID]++
				if msg, _ := p["error"].(string); msg != "" {
					errs[clipDetail(msg, 120)]++
				}
				if res, _ := p["result"].(string); strings.HasPrefix(res, agent.RepeatStopPrefix) {
					repeats = append(repeats, clipDetail(res, 160))
				}
			case "llm_response":
				tokens[e.AgentID] += num(p["input_tokens"]) + num(p["output_tokens"])
			case "tool_end":
				if msg, _ := p["err"].(string); msg != "" {
					name, _ := p["name"].(string)
					toolFails[name]++
				}
			case evProcessPaused, evStandingFailing, evStandingStopped, memory.EvGoalStatus, memory.EvGoalCreated:
				changes = append(changes, fmt.Sprintf("%s %s %s", e.Type, e.AgentID, clipDetail(string(e.Payload), 160)))
			}
		}
		read += len(evs)
		if len(evs) < digestPageSize {
			break
		}
	}
	if len(turns) == 0 && len(changes) == 0 {
		return "", last, nil
	}

	var b strings.Builder
	b.WriteString("Sessions that ran turns, with the tokens their model calls used:\n")
	for _, id := range byCount(turns) {
		fmt.Fprintf(&b, "- %s: %d turns, %d tokens\n", r.sessionLabel(id), turns[id], tokens[id])
	}
	if len(errs) > 0 {
		b.WriteString("Turns that failed:\n")
		for _, msg := range capped(byCount(errs)) {
			fmt.Fprintf(&b, "- %dx %s\n", errs[msg], msg)
		}
	}
	if len(toolFails) > 0 {
		b.WriteString("Tool calls that failed:\n")
		for _, name := range capped(byCount(toolFails)) {
			fmt.Fprintf(&b, "- %s: %d\n", name, toolFails[name])
		}
	}
	if len(repeats) > 0 {
		b.WriteString("Turns stopped for repeating a call:\n")
		for _, s := range capped(repeats) {
			fmt.Fprintf(&b, "- %s\n", s)
		}
	}
	if len(changes) > 0 {
		b.WriteString("Processes and goals that changed:\n")
		for _, s := range capped(changes) {
			fmt.Fprintf(&b, "- %s\n", s)
		}
		if n := len(changes) - digestLineCap; n > 0 {
			fmt.Fprintf(&b, "- and %d more\n", n)
		}
	}
	return b.String(), last, nil
}

// reflectionDigest is the digest a reflect process's tick carries, and false
// when there is nothing new to reflect on. The first tick after a start reads
// from the end of the session's own last turn. A journal the runner cannot
// read is reflected on without a digest, as before.
func (r *StandingRunner) reflectionDigest(lp *liveProc) (string, bool) {
	session := lp.row.SessionID
	lp.mu.Lock()
	after := lp.reflectedSeq
	lp.mu.Unlock()
	if after == 0 {
		seq, err := r.store.LastEventSeq(session)
		if err != nil {
			slog.Warn("reflection: find its last turn", "session", session, "err", err)
			return "", true
		}
		after = seq
	}
	digest, last, err := r.activityDigest(session, after)
	if err != nil {
		slog.Warn("reflection: read the journal", "session", session, "err", err)
		return "", true
	}
	lp.mu.Lock()
	lp.reflectedSeq = last
	lp.mu.Unlock()
	if digest == "" {
		r.log.add(lp.row.ID, "skipped", "nothing new since the last reflection")
		return "", false
	}
	return digest, true
}

// sessionLabel names a session for the digest: a goal's description when the
// session pursues one, otherwise its id.
func (r *StandingRunner) sessionLabel(id string) string {
	if g, err := r.store.GoalGet(id); err == nil && g != nil {
		return fmt.Sprintf("goal %q (%s)", clipDetail(g.Description, 80), id)
	}
	return id
}

// byCount returns m's keys, the largest count first.
func byCount(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if m[keys[i]] != m[keys[j]] {
			return m[keys[i]] > m[keys[j]]
		}
		return keys[i] < keys[j]
	})
	return keys
}

func capped(s []string) []string {
	if len(s) > digestLineCap {
		return s[:digestLineCap]
	}
	return s
}

func num(v any) int {
	f, _ := v.(float64)
	return int(f)
}
