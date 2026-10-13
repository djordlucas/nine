package runtime

import (
	"encoding/json"
	"log/slog"
	"strings"

	"nine/internal/agent"
	"nine/internal/memory"
)

// taint carries the restriction of a turn a piped report or an event's
// content started past that turn (adr/process-sessions.md §7). What such a
// turn writes — a new file, a memory — is marked; a later turn that reads it
// is restricted from that read on, as the first was, and what it writes from
// then on is marked too. Without it an injected instruction rode the record
// the restricted turn wrote into the agent's next, unrestricted turn: on
// process-pipe-history qwen3.5:4b recorded "delete notes/keep.md" in its
// incident log, read the log on its next tick, and deleted the file.
type taint struct {
	store         *memory.Store
	session       string
	workspaceRoot string
}

// newTaint is the taint tracking of one session's loop.
func newTaint(store *memory.Store, session, workspaceRoot string) agent.Taint {
	if store == nil {
		return nil
	}
	return &taint{store: store, session: session, workspaceRoot: workspaceRoot}
}

type taintArgs struct {
	Path string `json:"path"`
	From string `json:"from"`
	To   string `json:"to"`
	Mode string `json:"mode"`
	Key  string `json:"key"`
}

func (t *taint) Wrote(tool string, args json.RawMessage, untrusted bool) {
	var a taintArgs
	if json.Unmarshal(args, &a) != nil {
		return
	}
	file := func(p string) (string, bool) { return workspaceRel(p) }
	set := func(kind, key string) {
		if err := t.store.TaintSet(kind, key, t.session); err != nil {
			slog.Warn("taint: mark", "kind", kind, "key", key, "err", err)
		}
	}
	clear := func(kind, key string) {
		if err := t.store.TaintClear(kind, key); err != nil {
			slog.Warn("taint: clear", "kind", kind, "key", key, "err", err)
		}
	}
	switch tool {
	case "write_file", "edit_file":
		p, ok := file(a.Path)
		switch {
		case !ok:
		case untrusted:
			set(memory.TaintFile, p)
		case tool == "write_file" && a.Mode != "append":
			// Rewritten whole by a clean turn: nothing untrusted is left in it.
			clear(memory.TaintFile, p)
		}
	case "delete_file":
		if p, ok := file(a.Path); ok {
			clear(memory.TaintFile, p)
		}
	case "move_file", "copy_file":
		src, ok1 := file(a.From)
		dst, ok2 := file(a.To)
		if !ok1 || !ok2 {
			return
		}
		was, _ := t.store.Tainted(memory.TaintFile, src)
		if was || untrusted {
			set(memory.TaintFile, dst)
		} else {
			clear(memory.TaintFile, dst)
		}
		if tool == "move_file" {
			clear(memory.TaintFile, src)
		}
	case "memory_set":
		if a.Key == "" {
			return
		}
		if untrusted {
			set(memory.TaintMemory, a.Key)
		} else {
			clear(memory.TaintMemory, a.Key)
		}
	case "memory_delete":
		if a.Key != "" {
			clear(memory.TaintMemory, a.Key)
		}
	}
}

func (t *taint) Read(tool string, args json.RawMessage, output string) bool {
	var a taintArgs
	_ = json.Unmarshal(args, &a)
	switch tool {
	case "read_file", "diff_file":
		p, ok := workspaceRel(a.Path)
		if !ok {
			return false
		}
		yes, _ := t.store.Tainted(memory.TaintFile, p)
		return yes
	case "file_search_text":
		return t.mentions(memory.TaintFile, output)
	case "memory_get":
		yes, _ := t.store.Tainted(memory.TaintMemory, a.Key)
		return yes
	case "memory_list", "memory_query":
		return t.mentions(memory.TaintMemory, output)
	}
	return false
}

// mentions reports whether output names a marked key — a search hit in a
// marked file, a marked memory among those listed.
func (t *taint) mentions(kind, output string) bool {
	keys, err := t.store.TaintedKeys(kind)
	if err != nil {
		return false
	}
	for _, k := range keys {
		if strings.Contains(output, k) {
			return true
		}
	}
	return false
}

func (t *taint) Restriction() agent.TurnRestriction {
	return pipedTurn(PipedTurnTools, t.workspaceRoot)
}
