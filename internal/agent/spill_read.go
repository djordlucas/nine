package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"nine/internal/memory"
)

// RegisterSpillReader lets read_file serve a `spill/...` path from the store.
//
// Truncated tool output lives in the `files` table, not on disk, because only a
// daemon-owned table keeps a spill byte-identical to what the tool returned
// (spec/contracts/memory-store.md R-MEM.9). That left the agent with two read
// tools over two namespaces, and adr/tool-output-spill.md records the result:
// reaching into the wrong one is the most common model error there is, with
// three recovery messages written to explain it after the fact.
//
// One read tool cannot make that mistake. The dispatcher routes by prefix —
// `spill/` from the store, everything else to the sandboxed tool over the
// workspace — so the model names a path and gets its contents, whichever side
// it lives on.
//
// Wrapping rather than replacing: the sandboxed handler keeps every property it
// has, including the pre-open that confines it. This adds a branch in front of
// it, not a second implementation of it.
func RegisterSpillReader(d *Dispatcher, store *memory.Store) {
	if store == nil {
		return
	}
	sandboxed, ok := d.handlers["read_file"]
	if !ok {
		// No sandboxed read_file (a bare dispatcher, or tools disabled): serving
		// spills alone is still better than not serving them.
		sandboxed = func(context.Context, json.RawMessage) (string, error) {
			return "", fmt.Errorf("read_file: no workspace filesystem is available")
		}
	}

	d.handlers["read_file"] = func(ctx context.Context, args json.RawMessage) (string, error) {
		var req struct {
			Path   string `json:"path"`
			Offset int    `json:"offset"`
			Limit  int    `json:"limit"`
		}
		if err := json.Unmarshal(args, &req); err != nil {
			return "", fmt.Errorf("read_file: %w", err)
		}
		if !strings.HasPrefix(req.Path, SpillPathPrefix) {
			return sandboxed(ctx, args)
		}
		return readSpill(store, req.Path, req.Offset, req.Limit)
	}
	d.backends["read_file"] = "builtin"
}

// readSpill returns a stored file, whole or as a window. The shapes match
// file_fetch's exactly — a bare string for a whole read, a FileSlice for a
// window — so the two tools cannot disagree about what a spill read looks like
// while both exist.
func readSpill(store *memory.Store, path string, offset, limit int) (string, error) {
	if offset <= 0 && limit <= 0 {
		content, found, err := store.FileFetch(path)
		if err != nil {
			return "", err
		}
		if !found {
			return "", MissingStorePathError(store, path)
		}
		return content, nil
	}
	slice, found, err := store.FileFetchRange(path, offset, limit)
	if err != nil {
		return "", err
	}
	if !found {
		return "", MissingStorePathError(store, path)
	}
	data, err := json.Marshal(slice)
	if err != nil {
		return "", err
	}
	return string(data), nil
}
