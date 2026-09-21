package runtime

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"nine/internal/agent"
	"nine/internal/memory"
)

// migratedStoreDir holds a file whose path was already taken in the workspace.
// Under .nine/ so it is out of the operator's version control and out of the
// scan, and named so an operator reading it knows where it came from.
const migratedStoreDir = WorkspaceStateDir + "/migrated-store"

// MigrateStoredFilesToWorkspace moves agent-authored files out of the `files`
// table and into the workspace, once.
//
// Until this change an agent had two places to put a file, and the memory file
// store was one of them. Retiring `file_store` without moving what it holds
// would strand every file an agent saved there: still in the database, no
// longer reachable by any tool.
//
// Same relative path, deliberately. An agent that recorded "notes/x.md" with
// memory_set, or named it in a skill, finds it at that path in the workspace —
// the migration is invisible to anything that remembered where it put a file.
//
// A path already occupied in the workspace is a different file with the same
// name, so it goes to .nine/migrated-store/ and is logged rather than
// overwriting what is there.
//
// It cannot be an R-MEM.10 migration step: those commit atomically with a
// schema version bump, and this writes outside the database. It runs from
// bootstrap, and re-runs safely because a row is deleted only after its file
// exists on disk.
func MigrateStoredFilesToWorkspace(store *memory.Store, root string) {
	if store == nil || root == "" {
		return
	}
	paths, err := store.FileList("")
	if err != nil {
		slog.Warn("store migration: cannot list stored files", "err", err)
		return
	}

	var moved, displaced int
	for _, p := range paths {
		// Spilled output stays where it is: the store is its home, and the
		// retention sweep still owns it (R-MEM.9).
		if strings.HasPrefix(p, agent.SpillPathPrefix) {
			continue
		}
		content, found, err := store.FileFetch(p)
		if err != nil || !found {
			continue
		}

		dest, wasDisplaced := migrationDestination(root, p)
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			slog.Warn("store migration: cannot create directory", "path", p, "err", err)
			continue
		}
		if err := os.WriteFile(dest, []byte(content), 0o644); err != nil { //nolint:gosec // agent-authored content, in the operator's own workspace
			slog.Warn("store migration: cannot write file", "path", p, "err", err)
			continue
		}
		// Only now: a row deleted before its file exists loses the file.
		if err := store.FileDelete(p); err != nil {
			slog.Warn("store migration: cannot drop row", "path", p, "err", err)
			continue
		}
		moved++
		if wasDisplaced {
			displaced++
			slog.Info("store migration: workspace path was taken",
				"stored_path", p, "written_to", dest)
		}
	}
	if moved > 0 {
		slog.Info("migrated stored files into the workspace",
			"files", moved, "displaced", displaced, "root", root)
	}
}

// migrationDestination picks where a stored file lands: its own path, or a
// parallel tree when something is already there.
func migrationDestination(root, stored string) (string, bool) {
	clean := strings.TrimPrefix(filepath.ToSlash(filepath.Clean(stored)), "/")
	dest := filepath.Join(root, filepath.FromSlash(clean))
	if _, err := os.Stat(dest); err == nil {
		return filepath.Join(root, filepath.FromSlash(migratedStoreDir), filepath.FromSlash(clean)), true
	}
	return dest, false
}
