package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"time"

	"nine/internal/memory"
)

// Genesis (adr/process-sessions.md §10, phase 7). On the first boot of a
// store, an operator's genesis document is turned into the instance's first
// processes: a `genesis` session, under the genesis role, is given the document
// and then further turns until it says it is done and a process it wrote is
// running, within a bound. tool_write's refusals come back inside the turn, so
// a refused write is retried there. A genesis that succeeds is marked and never
// runs again; one that does not is reported and runs again at the next boot.

const (
	// GenesisSession is the genesis session's id.
	GenesisSession = "genesis"
	// GenesisRole is the role it runs under.
	GenesisRole = "genesis"
	// genesisSentinel marks a store genesis has been done in; its value is
	// the document's hash.
	genesisSentinel = "genesis/_done"
	// genesisDone is what the session says when it has finished.
	genesisDone = "GENESIS DONE"
	// DefaultGenesisTurns bounds the session when [bootstrap] genesis_turns is
	// unset.
	DefaultGenesisTurns = 8
)

// GenesisOptions is what genesis needs to know about the instance.
type GenesisOptions struct {
	// Turns bounds the session; <= 0 is DefaultGenesisTurns.
	Turns int
	// Revision says a reviser can be written: [processes] allow_revision is
	// on and the reviser role is in [tools.agent] process_roles.
	Revision bool
	// ProcessRoles are the roles a written process may run under.
	ProcessRoles []string
	// Database says nine:sql and sql_query are available.
	Database bool
	// Verify bounds how long genesis watches what it wrote run before it
	// accepts GENESIS DONE; 0 derives it from the processes' cadence.
	Verify time.Duration
	// Poll is how often it looks; 0 is 2s.
	Poll time.Duration
}

// GenesisTurns runs genesis's turns; the daemon implements it.
type GenesisTurns interface {
	ProcessTurn(ctx context.Context, id string, p RoleParams, text string, opt TurnOptions) (string, int, error)
}

// RunGenesis runs genesis for the document at path, unless it is empty or
// genesis was done in this store. It reports whether genesis ran and
// succeeded; a failure is also reported to the human feed.
func RunGenesis(ctx context.Context, turns GenesisTurns, store *memory.Store, path string, opt GenesisOptions) (bool, error) {
	if turns == nil || store == nil || strings.TrimSpace(path) == "" {
		return false, nil
	}
	if _, done, err := store.Get(genesisSentinel); err != nil {
		return false, fmt.Errorf("check %s: %w", genesisSentinel, err)
	} else if done {
		return false, nil
	}
	data, err := os.ReadFile(path) //nolint:gosec // operator-configured path
	if err != nil {
		return false, fmt.Errorf("read the genesis document %s: %w", path, err)
	}
	doc := strings.TrimSpace(string(data))
	if doc == "" {
		return false, fmt.Errorf("the genesis document %s is empty", path)
	}
	max := opt.Turns
	if max <= 0 {
		max = DefaultGenesisTurns
	}

	slog.Info("genesis started", "document", path, "turns", max)
	text := genesisPrompt(doc, opt)
	for turn := 1; turn <= max; turn++ {
		reply, _, err := turns.ProcessTurn(ctx, GenesisSession, RoleParams{Role: GenesisRole}, text,
			TurnOptions{Trigger: "genesis"})
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		if err != nil {
			// A failed turn — a provider error, a cut reply — costs a turn, not
			// the genesis.
			slog.Warn("genesis turn failed", "turn", turn, "err", err)
			text = fmt.Sprintf("Your last turn failed: %v. Continue the genesis from where you are; "+
				"tool_read with no name shows what you have written. Say %s when you have finished.", err, genesisDone)
			continue
		}
		running, err := genesisProcesses(store)
		if err != nil {
			return false, err
		}
		if strings.Contains(reply, genesisDone) && len(running) > 0 {
			// Written is not working: watch what it wrote run, and hand back
			// what fails, as nine-will fed every error back for another try.
			failing, err := verifyGenesis(ctx, store, running, opt)
			if err != nil {
				return false, err
			}
			if len(failing) > 0 {
				text = "Not done yet: these processes fail when they run:\n" + strings.Join(failing, "\n") +
					"\nRead each with tool_read, fix the program, and write it again under the same name. " +
					"Then say " + genesisDone + "."
				continue
			}
		}
		switch {
		case strings.Contains(reply, genesisDone) && len(running) > 0:
			sum := sha256.Sum256(data)
			if err := store.Set(genesisSentinel, hex.EncodeToString(sum[:])); err != nil {
				return false, fmt.Errorf("mark genesis done: %w", err)
			}
			slog.Info("genesis done", "turns", turn, "processes", running)
			notify(store, fmt.Sprintf("Genesis done in %d turns: %s now running (from %s).",
				turn, strings.Join(running, ", "), path))
			return true, nil
		case strings.Contains(reply, genesisDone):
			text = "You said " + genesisDone + ", but no process you wrote is running. tool_read with no name " +
				"shows what you wrote, with each one's state and last error. Fix it, then say " + genesisDone + "."
		default:
			text = "Continue the genesis: write what the document still needs, one tool_write per tool or " +
				"process. tool_read with no name shows what you have written. Say " + genesisDone + " when you have finished."
		}
	}
	msg := fmt.Sprintf("Genesis from %s did not finish in %d turns; it runs again at the next boot. "+
		"See the genesis session's history for why.", path, max)
	notify(store, msg)
	return false, fmt.Errorf("%s", msg)
}

// verifyGenesis watches the processes genesis wrote until each has run once,
// one fails, or the window ends, and returns each failing one with its error.
// A process not yet triggered when the window ends — an hourly one — is
// accepted: nothing it did has failed.
func verifyGenesis(ctx context.Context, store *memory.Store, ids []string, opt GenesisOptions) ([]string, error) {
	window, poll := opt.Verify, opt.Poll
	if poll <= 0 {
		poll = 2 * time.Second
	}
	if window <= 0 {
		window = 3 * time.Minute
		for _, id := range ids {
			if p, ok, err := store.ProcessGet(id); err == nil && ok && p.IntervalSecs > 0 {
				window = min(window, max(2*time.Duration(p.IntervalSecs)*time.Second, 30*time.Second))
			}
		}
	}
	deadline := time.Now().Add(window)
	for {
		var failing []string
		proven := 0
		for _, id := range ids {
			p, ok, err := store.ProcessGet(id)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
			if p.Failures > 0 {
				failing = append(failing, fmt.Sprintf("- %s: %s", id, clipDetail(p.LastError, 600)))
				continue
			}
			if t, ok, err := store.GeneratedToolGet(p.Tool); err == nil && ok && !t.Probation {
				proven++
			}
		}
		if len(failing) > 0 || proven == len(ids) || !time.Now().Before(deadline) {
			return failing, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(poll):
		}
	}
}

// genesisProcesses lists the running processes Nine wrote.
func genesisProcesses(store *memory.Store) ([]string, error) {
	procs, err := store.ProcessList()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, p := range procs {
		if p.Generated && (p.State == memory.ProcessRunning || p.State == memory.ProcessFailing) {
			out = append(out, p.ID)
		}
	}
	slices.Sort(out)
	return out, nil
}

// genesisPrompt is the first turn: the document, and what this instance
// allows.
func genesisPrompt(doc string, opt GenesisOptions) string {
	var b strings.Builder
	b.WriteString("This is your genesis. The document below describes the instance you are to become: what it " +
		"does, for whom, and how. Turn it into running processes now.\n\n")
	b.WriteString("How:\n")
	b.WriteString("- Each process is one tool_write with a process block. Start from a program in the process-patterns " +
		"skill (skill_read) and change only what differs. The simplest, complete:\n\n" + genesisExample + "\n")
	b.WriteString("- Write them one at a time. A refusal, or a process that fails when it runs, tells you what to fix: " +
		"fix that one thing and write the whole program again under the same name.\n")
	fmt.Fprintf(&b, "- Processes run under one of these roles: %s.\n", strings.Join(opt.ProcessRoles, ", "))
	if opt.Database {
		b.WriteString("- For records that outlast a run, declare {\"sql\":true} and keep them in the database through " +
			"nine:sql; each process creates its own tables with CREATE TABLE IF NOT EXISTS.\n")
	}
	if opt.Revision {
		b.WriteString("- Revision is on: also write a reviser process, under the role \"reviser\", that every few hours " +
			"reviews what you wrote with tool_read and fixes one thing at a time (the process-revision skill shows how).\n")
	} else {
		b.WriteString("- Revision is off on this instance: write no process that rewrites other processes.\n")
	}
	b.WriteString("- Write only what the document asks for. Keep each program small and complete.\n")
	b.WriteString("- When every process the document needs is written and running, say " + genesisDone + ".\n\n")
	b.WriteString("The document:\n\n")
	b.WriteString(doc)
	return b.String()
}

// genesisExample is the process-patterns skill's first pattern, given inline
// so the first write needs no lookup.
const genesisExample = `import { next } from "nine:process";
import { appendFile } from "nine:fs";

export default () => {
  for (;;) {
    const t = next();
    appendFile("/work/heartbeat.log", t.at + "\n");
  }
};

with tool_write {"name":"heartbeat","capabilities":{"fs":["write"]},"process":{"every":"10s"},"source":"<the program>"}`

// notify puts a message in the human feed.
func notify(store *memory.Store, msg string) {
	if err := store.UserNotificationCreate(memory.NewID(), "", msg); err != nil {
		slog.Warn("genesis: notify", "err", err)
	}
}
