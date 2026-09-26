package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"nine/internal/config"
	"nine/internal/memory"
	"nine/internal/protocol"
)

// Restore replaces the live database with a snapshot taken by `nine backup`.
//
// Doing this by hand is four steps in a required order, and the one people skip
// — removing the stale `-wal` and `-shm` sidecars — leaves SQLite pairing the
// restored file with the write-ahead log of the database it replaced. That is
// silent: the daemon starts, and the data is wrong. Encoding the order here is
// the point of the command.
//
// Nothing is deleted. The displaced database and its sidecars are renamed with
// a timestamp, so a restore aimed at the wrong file is itself reversible.
func (c *CLI) Restore(cfg *config.Config, src string) error {
	dst, err := cfg.DatabasePath()
	if err != nil {
		return err
	}
	// [memory].path may be relative; the self-restore guard below compares
	// paths, so both sides have to be resolved the same way.
	if dst, err = filepath.Abs(dst); err != nil {
		return fmt.Errorf("resolve database path: %w", err)
	}

	// A running daemon holds the database open. Swapping the file underneath it
	// corrupts both the restore and the session state still in flight.
	if protocol.CanConnect(cfg.SocketPath()) {
		return fmt.Errorf("the daemon is running and holds %s open; stop it first "+
			"(`docker stop <container>`, or interrupt a foreground `nine daemon`)", dst)
	}

	absSrc, err := filepath.Abs(src)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", src, err)
	}
	if absSrc == dst {
		return fmt.Errorf("%s is the live database, not a snapshot of one", absSrc)
	}

	// Check the snapshot before touching anything. A file that is not a Nine
	// database, or one written by a newer Nine, should be refused while the
	// current database is still in place.
	version, err := memory.InspectSnapshot(absSrc)
	if err != nil {
		return fmt.Errorf("read snapshot %s: %w", absSrc, err)
	}
	if known := memory.SchemaVersion(); version > known {
		return fmt.Errorf("snapshot %s is at schema version %d; this binary understands %d — "+
			"restore it with the version of nine that wrote it", absSrc, version, known)
	}

	stamp := time.Now().UTC().Format("20060102T150405Z")
	displaced, err := displace(dst, stamp)
	if err != nil {
		return err
	}

	if err := copyFile(absSrc, dst); err != nil {
		return fmt.Errorf("copy snapshot into place: %w", err)
	}

	fmt.Fprintf(c.Out, "restored: %s\nfrom:     %s (schema version %d)\n", dst, absSrc, version)
	switch {
	case len(displaced) == 0:
		fmt.Fprintln(c.Out, "nothing was displaced; there was no database at that path")
	default:
		fmt.Fprintf(c.Out, "displaced: %s\n", displaced[0])
		for _, p := range displaced[1:] {
			fmt.Fprintf(c.Out, "           %s\n", p)
		}
		fmt.Fprintf(c.Out, "undo with: mv %s %s\n", displaced[0], dst)
	}
	if known := memory.SchemaVersion(); version < known {
		fmt.Fprintf(c.Out, "note: schema %d will migrate to %d when the daemon next opens it\n", version, known)
	}
	return nil
}

// displace renames the database and its WAL sidecars out of the way, returning
// the new paths in the order they were moved. The sidecars must go with it: a
// restored database paired with the previous one's write-ahead log is the
// failure this command exists to prevent.
func displace(dst, stamp string) ([]string, error) {
	var moved []string
	for _, path := range []string{dst, dst + "-wal", dst + "-shm"} {
		if _, err := os.Stat(path); err != nil {
			continue // not present; nothing to move
		}
		to := fmt.Sprintf("%s.replaced-%s", path, stamp)
		if err := os.Rename(path, to); err != nil {
			return moved, fmt.Errorf("move %s aside: %w", path, err)
		}
		moved = append(moved, to)
	}
	return moved, nil
}

// copyFile writes src to dst, syncing before it returns so the restored
// database is on disk rather than in the page cache when the daemon starts.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close() //nolint:errcheck

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close() //nolint:errcheck
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close() //nolint:errcheck
		return err
	}
	return out.Close()
}
