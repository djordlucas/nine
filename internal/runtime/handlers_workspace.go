package runtime

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"
	"time"

	"nine/internal/protocol"
)

// The operator's workspace files (adr/web-ui.md §10): list a directory, read a
// file, write one, delete one. The workspace is the operator's directory, so
// they reach all of it but Nine's own bookkeeping in .nine/, which stays
// private. An overwrite or a delete moves the old file to the trash, as the
// file tools do, so the agent's trash_list and restore_file can bring it back.

// errVersion is a write or delete whose file changed since the caller read it.
var errVersion = errors.New("the file changed since it was read")

func (d *Daemon) handleWorkspace(enc *json.Encoder, r protocol.WorkspaceReq) {
	reply := func(v any, err error) {
		if err != nil {
			enc.Encode(protocol.NewErrorMsg(err.Error())) //nolint:errcheck
			return
		}
		data, _ := json.Marshal(v)
		enc.Encode(protocol.NewTextMsg(protocol.TypeWorkspace, string(data))) //nolint:errcheck
	}
	if d.workspaceRoot == "" {
		reply(nil, errors.New("no workspace is configured ([workspace].root)"))
		return
	}
	var args protocol.WorkspaceArgs
	if len(r.Args) > 0 {
		if err := json.Unmarshal(r.Args, &args); err != nil {
			reply(nil, fmt.Errorf("workspace: %w", err))
			return
		}
	}
	root, err := os.OpenRoot(d.workspaceRoot)
	if err != nil {
		reply(nil, fmt.Errorf("workspace: %w", err))
		return
	}
	defer root.Close() //nolint:errcheck
	ws := workspaceFiles{root: root}
	switch r.Action {
	case protocol.WorkspaceList:
		reply(ws.list(args.Path))
	case protocol.WorkspaceRead:
		reply(ws.read(args.Path))
	case protocol.WorkspaceWrite:
		reply(ws.write(args.Path, args.Content, args.IfVersion, time.Now()))
	case protocol.WorkspaceDelete:
		reply(ws.remove(args.Path, args.IfVersion, time.Now()))
	default:
		reply(nil, fmt.Errorf("unknown workspace action %q (want one of %s)", r.Action,
			strings.Join(protocol.WorkspaceActions, ", ")))
	}
}

// workspaceFiles is the workspace opened as an os.Root: no path, and no
// symbolic link inside it, resolves outside the root.
type workspaceFiles struct{ root *os.Root }

// rel cleans a path relative to the root and refuses Nine's own state; "" and
// "." are the root itself.
func (workspaceFiles) rel(p string) (string, error) {
	p = path.Clean(strings.TrimPrefix(strings.TrimSpace(p), "/"))
	if p == ".." || strings.HasPrefix(p, "../") {
		return "", fmt.Errorf("%s is outside the workspace", p)
	}
	if first, _, _ := strings.Cut(p, "/"); strings.EqualFold(first, WorkspaceStateDir) {
		return "", fmt.Errorf("%s is Nine's own state and is not reachable here", p)
	}
	return p, nil
}

func entryOf(p string, info fs.FileInfo) protocol.WorkspaceEntry {
	e := protocol.WorkspaceEntry{
		Name: info.Name(), Path: p, Dir: info.IsDir(),
		Modified: info.ModTime().UTC().Format(time.RFC3339),
	}
	if !e.Dir {
		e.Bytes = info.Size()
		e.Version = fmt.Sprintf("%x-%x", info.ModTime().UnixNano(), info.Size())
	}
	return e
}

func (w workspaceFiles) list(dir string) (protocol.WorkspaceListing, error) {
	p, err := w.rel(dir)
	if err != nil {
		return protocol.WorkspaceListing{}, err
	}
	f, err := w.root.Open(p)
	if err != nil {
		return protocol.WorkspaceListing{}, notFound(p, err)
	}
	defer f.Close() //nolint:errcheck
	names, err := f.Readdirnames(-1)
	if err != nil {
		return protocol.WorkspaceListing{}, fmt.Errorf("%s is not a directory", p)
	}
	out := protocol.WorkspaceListing{Entries: []protocol.WorkspaceEntry{}}
	if p != "." {
		out.Path = p
	}
	for _, name := range names {
		child := path.Join(p, name)
		if _, err := w.rel(child); err != nil {
			continue // .nine at the root
		}
		// Stat follows a link inside the root; one that leaves it is not listed.
		info, err := w.root.Stat(child)
		if err != nil {
			continue
		}
		out.Entries = append(out.Entries, entryOf(child, info))
	}
	slices.SortFunc(out.Entries, func(a, b protocol.WorkspaceEntry) int {
		if a.Dir != b.Dir {
			if a.Dir {
				return -1
			}
			return 1
		}
		return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
	return out, nil
}

func (w workspaceFiles) read(file string) (protocol.WorkspaceFile, error) {
	p, info, err := w.file(file)
	if err != nil {
		return protocol.WorkspaceFile{}, err
	}
	if info.Size() > protocol.MaxWorkspaceFileBytes {
		return protocol.WorkspaceFile{}, fmt.Errorf("%s is %d bytes, over the %d MB this reads; use a backup to take it", p, info.Size(), protocol.MaxWorkspaceFileBytes>>20)
	}
	content, err := w.root.ReadFile(p)
	if err != nil {
		return protocol.WorkspaceFile{}, err
	}
	return protocol.WorkspaceFile{WorkspaceEntry: entryOf(p, info), Content: content}, nil
}

func (w workspaceFiles) write(file string, content []byte, ifVersion string, now time.Time) (protocol.WorkspaceEntry, error) {
	p, err := w.rel(file)
	if err != nil {
		return protocol.WorkspaceEntry{}, err
	}
	if p == "." {
		return protocol.WorkspaceEntry{}, errors.New("path is required")
	}
	if first, _, _ := strings.Cut(p, "/"); first == "spill" {
		return protocol.WorkspaceEntry{}, fmt.Errorf("%s is reserved: spill/ names truncated tool output, which only Nine writes", p)
	}
	if len(content) > protocol.MaxWorkspaceFileBytes {
		return protocol.WorkspaceEntry{}, fmt.Errorf("the file is over the %d MB this writes", protocol.MaxWorkspaceFileBytes>>20)
	}
	var trashed string
	switch info, err := w.root.Stat(p); {
	case err == nil && info.IsDir():
		return protocol.WorkspaceEntry{}, fmt.Errorf("%s is a directory", p)
	case err == nil:
		if ifVersion != "" && ifVersion != entryOf(p, info).Version {
			return protocol.WorkspaceEntry{}, fmt.Errorf("%s: %w", p, errVersion)
		}
		if trashed, err = w.trash(p, now); err != nil {
			return protocol.WorkspaceEntry{}, err
		}
	case !errors.Is(err, fs.ErrNotExist):
		return protocol.WorkspaceEntry{}, err
	case ifVersion != "":
		return protocol.WorkspaceEntry{}, fmt.Errorf("%s: %w", p, errVersion)
	}
	if dir := path.Dir(p); dir != "." {
		if err := w.root.MkdirAll(dir, 0o755); err != nil {
			return protocol.WorkspaceEntry{}, err
		}
	}
	if err := w.root.WriteFile(p, content, 0o644); err != nil {
		if trashed != "" {
			w.root.Rename(path.Join(WorkspaceTrashDir, trashed, p), p) //nolint:errcheck // put the old file back
		}
		return protocol.WorkspaceEntry{}, err
	}
	info, err := w.root.Stat(p)
	if err != nil {
		return protocol.WorkspaceEntry{}, err
	}
	e := entryOf(p, info)
	e.Trashed = trashed
	return e, nil
}

// remove moves a file to the trash, or removes an empty directory.
func (w workspaceFiles) remove(file, ifVersion string, now time.Time) (protocol.WorkspaceEntry, error) {
	p, err := w.rel(file)
	if err != nil {
		return protocol.WorkspaceEntry{}, err
	}
	if p == "." {
		return protocol.WorkspaceEntry{}, errors.New("the workspace root cannot be deleted")
	}
	info, err := w.root.Stat(p)
	if err != nil {
		return protocol.WorkspaceEntry{}, notFound(p, err)
	}
	e := entryOf(p, info)
	if info.IsDir() {
		if err := w.root.Remove(p); err != nil {
			return protocol.WorkspaceEntry{}, fmt.Errorf("%s is not empty: delete what it holds first", p)
		}
		return e, nil
	}
	if ifVersion != "" && ifVersion != e.Version {
		return protocol.WorkspaceEntry{}, fmt.Errorf("%s: %w", p, errVersion)
	}
	if e.Trashed, err = w.trash(p, now); err != nil {
		return protocol.WorkspaceEntry{}, err
	}
	return e, nil
}

// file resolves a path that must name a file.
func (w workspaceFiles) file(file string) (string, fs.FileInfo, error) {
	p, err := w.rel(file)
	if err != nil {
		return "", nil, err
	}
	info, err := w.root.Stat(p)
	if err != nil {
		return "", nil, notFound(p, err)
	}
	if info.IsDir() {
		return "", nil, fmt.Errorf("%s is a directory", p)
	}
	return p, info, nil
}

// trash moves a file to .nine/trash/<entry>/<path>, the layout the file tools
// and the trash sweeper share, and returns the entry's name.
func (w workspaceFiles) trash(p string, now time.Time) (string, error) {
	b := make([]byte, 4)
	rand.Read(b) //nolint:errcheck // never fails (crypto/rand)
	entry := now.UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(b)
	dest := path.Join(WorkspaceTrashDir, entry, p)
	if err := w.root.MkdirAll(path.Dir(dest), 0o755); err != nil {
		return "", fmt.Errorf("trash %s: %w", p, err)
	}
	if err := w.root.Rename(p, dest); err != nil {
		return "", fmt.Errorf("trash %s: %w", p, err)
	}
	return entry, nil
}

func notFound(p string, err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("not found: %s", p)
	}
	return err
}
