package cli

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// A combined backup is a gzipped tar holding the database snapshot and the
// workspace tree. The two travel together because they reference each other: a
// spill path or a workspace index row in the store names a file on disk, and a
// database restored beside a workspace from a different moment describes files
// that are not there.
const (
	archiveDBEntry    = "nine.db"
	archiveWorkPrefix = "workspace/"
)

// archiveSuffix is the extension that selects the combined format. The
// destination names the shape: `.db` is the store alone, `.tar.gz` is the store
// and the workspace.
const archiveSuffix = ".tar.gz"

// isArchivePath reports whether path asks for the combined format.
func isArchivePath(path string) bool {
	return strings.HasSuffix(strings.ToLower(path), archiveSuffix)
}

// writeArchive builds the combined backup: dbSrc becomes nine.db, and every
// regular file under workRoot lands beneath workspace/.
//
// Symlinks are skipped rather than stored. A link is a path, not content: one
// pointing outside the workspace would either break on extract or, worse,
// recreate a pointer into the host filesystem somewhere it did not exist
// before. The count is returned so the caller can say what was left out.
func writeArchive(dst, dbSrc, workRoot string) (files int, skipped int, err error) {
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, 0, err
	}
	defer out.Close() //nolint:errcheck

	gz := gzip.NewWriter(out)
	tw := tar.NewWriter(gz)

	if err := addFile(tw, dbSrc, archiveDBEntry); err != nil {
		return 0, 0, fmt.Errorf("add %s: %w", archiveDBEntry, err)
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
			return addFile(tw, path, archiveWorkPrefix+filepath.ToSlash(rel))
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

// extractArchive unpacks src into dir, returning the number of workspace files
// written. It is the untrusted half: an archive is a file someone hands you, so
// every entry's destination is checked to be inside dir before anything is
// created.
func extractArchive(src, dir string) (int, error) {
	f, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer f.Close() //nolint:errcheck

	gz, err := gzip.NewReader(f)
	if err != nil {
		return 0, fmt.Errorf("%s is not a gzip archive: %w", src, err)
	}
	defer gz.Close() //nolint:errcheck

	var workFiles int
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return workFiles, fmt.Errorf("read archive: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue // directories are implied; nothing else is archived
		}
		dest, err := safeJoin(dir, hdr.Name)
		if err != nil {
			return workFiles, err
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
			return workFiles, err
		}
		if err := writeEntry(tr, dest, hdr.FileInfo().Mode()); err != nil {
			return workFiles, fmt.Errorf("write %s: %w", hdr.Name, err)
		}
		if strings.HasPrefix(hdr.Name, archiveWorkPrefix) {
			workFiles++
		}
	}
	return workFiles, nil
}

// safeJoin resolves name under dir and refuses anything that escapes it. An
// archive entry is attacker-controlled input: "../../etc/cron.d/x" and an
// absolute path are both ways to write outside the directory the operator
// named, and neither is a thing a backup of ours ever contains.
func safeJoin(dir, name string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(name))
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("archive entry %q escapes the destination directory", name)
	}
	dest := filepath.Join(dir, clean)
	// Join cleans, so compare the result rather than trusting the check above
	// to have covered every spelling.
	if rel, err := filepath.Rel(dir, dest); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("archive entry %q escapes the destination directory", name)
	}
	return dest, nil
}

func writeEntry(r io.Reader, dest string, mode fs.FileMode) error {
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode.Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, r); err != nil {
		out.Close() //nolint:errcheck
		return err
	}
	return out.Close()
}
