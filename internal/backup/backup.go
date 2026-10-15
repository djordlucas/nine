// Package backup writes the combined backup archive: a database snapshot and
// the workspace tree in one gzipped tar. The CLI's `nine backup` and the
// daemon's `backup` message, behind `GET /api/v1/backup`, both write it.
package backup

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// A combined backup is a gzipped tar holding the database snapshot and the
// workspace tree. The two travel together because they reference each other: a
// spill path or a workspace index row in the store names a file on disk, and a
// database restored beside a workspace from a different moment describes files
// that are not there.
const (
	DBEntry    = "nine.db"
	WorkPrefix = "workspace/"
)

// WriteArchive builds the combined backup: dbSrc becomes nine.db, and every
// regular file under workRoot lands beneath workspace/.
//
// Symlinks are skipped rather than stored. A link is a path, not content: one
// pointing outside the workspace would either break on extract or, worse,
// recreate a pointer into the host filesystem somewhere it did not exist
// before. The count is returned so the caller can say what was left out.
func WriteArchive(dst, dbSrc, workRoot string) (files int, skipped int, err error) {
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, 0, err
	}
	defer out.Close() //nolint:errcheck

	gz := gzip.NewWriter(out)
	tw := tar.NewWriter(gz)

	if err := addFile(tw, dbSrc, DBEntry); err != nil {
		return 0, 0, fmt.Errorf("add %s: %w", DBEntry, err)
	}

	if workRoot != "" {
		err = filepath.WalkDir(workRoot, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(workRoot, path)
			if err != nil || rel == "." {
				return err
			}
			switch {
			case d.IsDir():
				return nil // directories are implied by the files inside them
			case !d.Type().IsRegular():
				skipped++
				return nil
			}
			files++
			return addFile(tw, path, WorkPrefix+filepath.ToSlash(rel))
		})
		if err != nil {
			return files, skipped, fmt.Errorf("walk workspace: %w", err)
		}
	}

	if err := tw.Close(); err != nil {
		return files, skipped, err
	}
	if err := gz.Close(); err != nil {
		return files, skipped, err
	}
	return files, skipped, out.Sync()
}

// addFile copies one file into the archive under name.
func addFile(tw *tar.Writer, path, name string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	hdr, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return err
	}
	hdr.Name = name
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck
	_, err = io.Copy(tw, f)
	return err
}

// TempPrefix names the temporary directories a snapshot for download is
// written into (the daemon's `backup` message). The API server streams the
// file and removes the directory; one it never removed is swept by the next
// backup once it is older than TempMaxAge.
const (
	TempPrefix = "nine-backup-"
	TempMaxAge = time.Hour
)

// NewTempDir creates a directory for one snapshot, first removing any left
// over from a download that never finished.
func NewTempDir() (string, error) {
	sweepTemp(time.Now())
	return os.MkdirTemp("", TempPrefix)
}

func sweepTemp(now time.Time) {
	entries, err := os.ReadDir(os.TempDir())
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), TempPrefix) {
			continue
		}
		if info, err := e.Info(); err == nil && now.Sub(info.ModTime()) > TempMaxAge {
			os.RemoveAll(filepath.Join(os.TempDir(), e.Name())) //nolint:errcheck
		}
	}
}

// RemoveTemp removes the temporary directory holding path. It refuses any path
// that is not a file directly inside a directory NewTempDir made, so a reply
// naming some other path cannot make the caller delete it.
func RemoveTemp(path string) error {
	dir := filepath.Dir(filepath.Clean(path))
	if filepath.Dir(dir) != filepath.Clean(os.TempDir()) || !strings.HasPrefix(filepath.Base(dir), TempPrefix) {
		return fmt.Errorf("%s is not a backup's temporary file", path)
	}
	return os.RemoveAll(dir)
}
