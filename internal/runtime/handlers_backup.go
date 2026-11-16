package runtime

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"nine/internal/backup"
	"nine/internal/protocol"
)

// handleBackup writes a snapshot of the store, and with r.Workspace the
// workspace beside it, into a temporary file for the API server to stream as a
// download (adr/web-ui.md §4). The API server may not open the database
// (spec/contracts/api.md API-A-1), so the daemon writes it; both run on one
// machine, as the socket between them requires.
func (d *Daemon) handleBackup(enc *json.Encoder, r protocol.BackupReq) {
	if d.store == nil {
		enc.Encode(protocol.NewErrorMsg("no store to back up")) //nolint:errcheck
		return
	}
	if r.Workspace && d.workspaceRoot == "" {
		enc.Encode(protocol.NewErrorMsg("no workspace is configured ([workspace].root); back up the store alone")) //nolint:errcheck
		return
	}
	res, err := d.writeBackup(r.Workspace, time.Now())
	if err != nil {
		enc.Encode(protocol.NewErrorMsg("backup: " + err.Error())) //nolint:errcheck
		return
	}
	data, _ := json.Marshal(res)
	enc.Encode(protocol.NewTextMsg(protocol.TypeBackup, string(data))) //nolint:errcheck
}

func (d *Daemon) writeBackup(withWorkspace bool, now time.Time) (res protocol.BackupResult, err error) {
	dir, err := backup.NewTempDir()
	if err != nil {
		return res, err
	}
	defer func() {
		if err != nil {
			os.RemoveAll(dir) //nolint:errcheck
		}
	}()
	stamp := now.UTC().Format("20060102-150405")
	// Per-agent layout (adr/agents-concept.md A4): the snapshot is a tar of the
	// registry db plus every agents/<id>.db, each VACUUM INTO its own file so
	// each is consistent by construction (Q33).
	if bd, ok := d.store.(interface{ BackupDBs(dir string) ([]backup.DBFile, error) }); ok {
		dbs, err := bd.BackupDBs(dir)
		if err != nil {
			return res, err
		}
		workRoot := ""
		if withWorkspace {
			workRoot = d.workspaceRoot
		}
		archive := filepath.Join(dir, "nine-"+stamp+".tar.gz")
		files, skipped, err := backup.WriteArchiveDBs(archive, dbs, workRoot)
		if err != nil {
			return res, fmt.Errorf("archive: %w", err)
		}
		res = protocol.BackupResult{Path: archive, Name: filepath.Base(archive), Files: files, Skipped: skipped}
	} else {
		db, err := d.store.BackupTo(filepath.Join(dir, backup.DBEntry))
		if err != nil {
			return res, err
		}
		res = protocol.BackupResult{Path: db, Name: "nine-" + stamp + ".db"}
		if withWorkspace {
			archive := filepath.Join(dir, "nine-"+stamp+".tar.gz")
			files, skipped, err := backup.WriteArchive(archive, db, d.workspaceRoot)
			if err != nil {
				return res, fmt.Errorf("archive: %w", err)
			}
			os.Remove(db) //nolint:errcheck // it is inside the archive now
			res = protocol.BackupResult{Path: archive, Name: filepath.Base(archive), Files: files, Skipped: skipped}
		}
	}
	info, err := os.Stat(res.Path)
	if err != nil {
		return res, err
	}
	res.Bytes = info.Size()
	return res, nil
}
