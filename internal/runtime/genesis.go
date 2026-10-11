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
	b.WriteString("- Each process is one tool_write with a process block (a live program: import { next, turn, report } " +
		"from \"nine:process\"). Write them one at a time; a refusal tells you what to fix, so fix it and write again.\n")
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

// notify puts a message in the human feed.
func notify(store *memory.Store, msg string) {
	if err := store.UserNotificationCreate(memory.NewID(), "", msg); err != nil {
		slog.Warn("genesis: notify", "err", err)
	}
}
