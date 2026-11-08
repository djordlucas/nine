package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// shippedFS implements the file half of the built-in tier in Go, confined to the
// workspace at root. The guest sees the workspace as /work; the host path is
// root. Ported from the removed wasm sandbox (adr/modular_nine.md §9.10) —
// the semantics, including the trash and the error messages, are carried over
// so a model sees the same behavior.

// guestRoot is the path the workspace appears under in tool arguments.
const guestRoot = "/work"

type shippedFS struct {
	root string
}

// stateDir and trashDir are Nine's own bookkeeping inside the workspace.
func (f *shippedFS) stateDir() string { return filepath.Join(f.root, ".nine") }
func (f *shippedFS) trashDir() string { return filepath.Join(f.root, ".nine", "trash") }

// resolveErr maps a tool-supplied path onto the workspace, refusing anything
// outside it. "/work/..." and relative paths are workspace paths; any other
// absolute path is refused with the same message the sandboxed tool gave.
func (f *shippedFS) resolveErr(path, label string) (string, error) {
	p := strings.TrimSpace(path)
	if p == "" {
		if label == "" {
			label = "path"
		}
		return "", fmt.Errorf("%s is required", label)
	}
	if p == guestRoot || strings.HasPrefix(p, guestRoot+"/") {
		rel := strings.TrimPrefix(strings.TrimPrefix(p, guestRoot), "/")
		return filepath.Join(f.root, rel), nil
	}
	if strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("%s is outside this tool's workspace. Paths resolve under %s — use a relative path, or a %s/... path", p, guestRoot, guestRoot)
	}
	return filepath.Join(f.root, p), nil
}

// guestPath renders a host path back as the /work/... name the tools report.
func (f *shippedFS) guestPath(host string) string {
	rel, err := filepath.Rel(f.root, host)
	if err != nil || strings.HasPrefix(rel, "..") {
		return host
	}
	return guestRoot + "/" + filepath.ToSlash(rel)
}

// refuseState keeps Nine's own bookkeeping out of reach of the file tools.
func (f *shippedFS) refuseState(host string) error {
	if host == f.stateDir() || strings.HasPrefix(host, f.stateDir()+string(os.PathSeparator)) {
		return fmt.Errorf("%s is Nine's own bookkeeping and is not writable. Use trash_list and restore_file to reach a deleted file", f.guestPath(host))
	}
	return nil
}

// refuseSpill keeps the reserved prefix out of the workspace.
func (f *shippedFS) refuseSpill(host string) error {
	rel, err := filepath.Rel(f.root, host)
	if err != nil {
		return nil
	}
	if rel == "spill" || strings.HasPrefix(rel, "spill"+string(os.PathSeparator)) {
		return fmt.Errorf("%s is reserved: spill/ holds truncated tool output, which only Nine writes. Choose another path", f.guestPath(host))
	}
	return nil
}

// entryName is the trash entry name the daemon's sweeper parses for age:
// <UTC>-<random hex>.
func entryName() string {
	now := time.Now().UTC()
	var b [4]byte
	rand.Read(b[:]) //nolint:errcheck // non-critical randomness in a name
	return fmt.Sprintf("%04d%02d%02dT%02d%02d%02dZ-%s",
		now.Year(), now.Month(), now.Day(), now.Hour(), now.Minute(), now.Second(), hex.EncodeToString(b[:]))
}

// trashTo moves a workspace path into a fresh trash entry, keeping its relative
// path so an operator reading the trash can see where a file came from.
func (f *shippedFS) trashTo(host string) error {
	rel, err := filepath.Rel(f.root, host)
	if err != nil {
		return err
	}
	dest := filepath.Join(f.trashDir(), entryName(), rel)
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return err
	}
	return os.Rename(host, dest)
}

// versionToken identifies a file as it was read: mtime-size.
func versionToken(info os.FileInfo) string {
	return fmt.Sprintf("%d-%d", info.ModTime().UnixMilli(), info.Size())
}

const sniffBytes = 8192

// refuseBinary stops a file that is not text from being decoded into
// replacement characters.
func (f *shippedFS) refuseBinary(host string, info os.FileInfo) error {
	fh, err := os.Open(host)
	if err != nil {
		return err
	}
	defer fh.Close() //nolint:errcheck
	head := make([]byte, min(sniffBytes, info.Size()))
	n, _ := fh.Read(head)
	for _, b := range head[:n] {
		if b == 0 {
			return fmt.Errorf("%s is not a text file (%d bytes, contains NUL). read_file returns text; use a tool that understands the format", guestOf(f, host), info.Size())
		}
	}
	return nil
}

// guestOf renders a host path as its guest name (a free function so a shadowed
// receiver still works).
func guestOf(f *shippedFS, host string) string { return f.guestPath(host) }

// decodeOrExplain decodes bytes as strict UTF-8 or explains why it cannot.
func decodeOrExplain(b []byte, guest string) (string, error) {
	if !utf8.Valid(b) {
		return "", fmt.Errorf("%s is not valid UTF-8, so it cannot be returned as text. Read a byte range with offset and limit if you need part of it", guest)
	}
	return string(b), nil
}

// trimPartialRunes drops a continuation-byte run at the start of a window and an
// incomplete sequence at its end, so a window landing mid-character decodes
// cleanly instead of failing.
func trimPartialRunes(b []byte, trimHead bool) []byte {
	from, to := 0, len(b)
	if trimHead {
		for from < to && b[from]&0xc0 == 0x80 {
			from++
		}
	}
	back, seen := to-1, 0
	for back >= from && b[back]&0xc0 == 0x80 && seen < 3 {
		back--
		seen++
	}
	if back >= from {
		lead := b[back]
		need := 1
		switch {
		case lead >= 0xf0:
			need = 4
		case lead >= 0xe0:
			need = 3
		case lead >= 0xc0:
			need = 2
		}
		if need > 1 && seen < need-1 {
			to = back
		}
	}
	return b[from:to]
}

// numberFrom prefixes each line with its number.
func numberFrom(text string, start int) string {
	lines := strings.Split(text, "\n")
	var b strings.Builder
	for i, l := range lines {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "%d\t%s", start+i, l)
	}
	return b.String()
}

// readFile implements read_file.
func (f *shippedFS) readFile(_ context.Context, args json.RawMessage) (string, error) {
	var req struct {
		Path        string `json:"path"`
		Offset      int    `json:"offset"`
		Limit       int    `json:"limit"`
		Lines       string `json:"lines"`
		LineNumbers bool   `json:"line_numbers"`
	}
	if err := json.Unmarshal(args, &req); err != nil {
		return "", fmt.Errorf("read_file: %w", err)
	}
	target, err := f.resolveErr(req.Path, "path")
	if err != nil {
		return "", err
	}
	guest := f.guestPath(target)

	info, err := os.Stat(target)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("no file at %s", guest)
		}
		return "", err
	}
	if info.IsDir() {
		return "", fmt.Errorf("%s is a directory", guest)
	}
	if err := f.refuseBinary(target, info); err != nil {
		return "", err
	}
	version := versionToken(info)

	if strings.TrimSpace(req.Lines) != "" {
		out, err := f.readLines(target, info, req.Lines, version, req.LineNumbers)
		if err != nil {
			return "", err
		}
		return out, nil
	}

	off := max(req.Offset, 0)
	lim := max(req.Limit, 0)

	// A plain whole-file read stays a plain string.
	if off == 0 && lim == 0 {
		b, err := os.ReadFile(target)
		if err != nil {
			return "", err
		}
		text, err := decodeOrExplain(b, guest)
		if err != nil {
			return "", err
		}
		if req.LineNumbers {
			text = numberFrom(text, 1)
		}
		return text, nil
	}

	data, err := readRange(target, off, lim, info.Size())
	if err != nil {
		return "", err
	}
	text, err := decodeOrExplain(trimPartialRunes(data, off > 0), guest)
	if err != nil {
		return "", err
	}
	content := text
	if req.LineNumbers {
		content = numberFrom(text, 1)
	}
	out, _ := json.Marshal(map[string]any{
		"content": content,
		"offset":  off,
		"bytes":   len(data),
		"total":   info.Size(),
		"version": version,
	})
	return string(out), nil
}

// readRange reads up to limit bytes from offset; limit 0 means the rest.
func readRange(path string, off, lim int, size int64) ([]byte, error) {
	if off >= int(size) {
		return nil, nil
	}
	if lim <= 0 || int64(off+lim) > size {
		lim = int(size) - off
	}
	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer fh.Close() //nolint:errcheck
	if _, err := fh.Seek(int64(off), 0); err != nil {
		return nil, err
	}
	buf := make([]byte, lim)
	n, err := fh.Read(buf)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return buf[:n], nil
}

// readLines serves a 1-based inclusive line range ("120-180", or "120").
func (f *shippedFS) readLines(target string, info os.FileInfo, spec, version string, numbered bool) (string, error) {
	var first, last int
	if _, err := fmt.Sscanf(strings.TrimSpace(spec), "%d-%d", &first, &last); err != nil {
		if _, err2 := fmt.Sscanf(strings.TrimSpace(spec), "%d", &first); err2 != nil {
			return "", fmt.Errorf("lines must look like \"120-180\" or \"120\", got %q", spec)
		}
		last = first
	}
	if first < 1 {
		return "", fmt.Errorf("lines are 1-based; the first line is 1")
	}
	if last < first {
		return "", fmt.Errorf("lines range ends before it starts (%s)", spec)
	}

	data, err := os.ReadFile(target)
	if err != nil {
		return "", err
	}
	guest := f.guestPath(target)
	text, err := decodeOrExplain(data, guest)
	if err != nil {
		return "", err
	}
	all := strings.Split(text, "\n")
	if len(all) > 0 && all[len(all)-1] == "" && len(all) > 1 {
		// trailing newline: the final empty piece is not a line
		all = all[:len(all)-1]
	}
	totalLines := len(all)
	if first > totalLines {
		return "", fmt.Errorf("%s has no lines in range %s (it has %d line(s))", guest, spec, totalLines)
	}
	last = min(last, totalLines)
	picked := all[first-1 : last]
	content := strings.Join(picked, "\n")
	if numbered {
		content = numberFrom(content, first)
	}
	out, _ := json.Marshal(map[string]any{
		"content":    content,
		"first_line": first,
		"lines":      len(picked),
		"version":    version,
	})
	return string(out), nil
}

// writeFile implements write_file. content_ref arrives already expanded by the
// dispatcher's ref resolver.
func (f *shippedFS) writeFile(_ context.Context, args json.RawMessage) (string, error) {
	var req struct {
		Path        string `json:"path"`
		Content     string `json:"content"`
		ContentRef  string `json:"content_ref"`
		Mode        string `json:"mode"`
		IfUnchanged string `json:"if_unchanged"`
		Preview     bool   `json:"preview"`
	}
	if err := json.Unmarshal(args, &req); err != nil {
		return "", fmt.Errorf("write_file: %w", err)
	}
	target, err := f.resolveErr(req.Path, "path")
	if err != nil {
		return "", err
	}
	guest := f.guestPath(target)
	body := req.Content
	if body == "" && req.ContentRef != "" {
		body = req.ContentRef // already expanded to the payload by the ref resolver
	}
	appendMode := req.Mode == "append"

	if err := f.refuseState(target); err != nil {
		return "", err
	}
	if err := f.refuseSpill(target); err != nil {
		return "", err
	}

	info, statErr := os.Stat(target)
	if statErr == nil && info.IsDir() {
		return "", fmt.Errorf("%s is a directory", guest)
	}

	if req.IfUnchanged != "" {
		now := "absent"
		if statErr == nil {
			now = versionToken(info)
		}
		if now != req.IfUnchanged {
			return "", fmt.Errorf("%s changed since you read it (version %s, you passed %s). Read it again and redo the change against the current contents", guest, now, req.IfUnchanged)
		}
	}

	if req.Preview {
		before := ""
		if statErr == nil {
			b, err := os.ReadFile(target)
			if err == nil {
				before = string(b)
			}
		}
		after := body
		if appendMode {
			after = before + body
		}
		h := diffHunks(before, after)
		out, _ := json.Marshal(map[string]any{
			"preview":       true,
			"path":          guest,
			"exists":        statErr == nil,
			"added_lines":   h.added,
			"removed_lines": h.removed,
			"diff":          h.text,
			"truncated":     h.truncated,
			"note":          "Nothing was written. Call again without preview to apply this.",
		})
		return string(out), nil
	}

	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return "", err
	}

	if appendMode {
		fh, err := os.OpenFile(target, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return "", err
		}
		if _, err := fh.WriteString(body); err != nil {
			fh.Close() //nolint:errcheck
			return "", err
		}
		fh.Close() //nolint:errcheck
		return fmt.Sprintf("appended %d character(s) to %s", len(body), guest), nil
	}

	// The previous contents go to the trash first, unless identical.
	trashed := false
	if statErr == nil {
		same, err := sameContent(target, info.Size(), body)
		if err != nil {
			return "", err
		}
		if !same {
			if err := f.trashTo(target); err != nil {
				return "", err
			}
			trashed = true
		}
	}
	if err := os.WriteFile(target, []byte(body), 0o600); err != nil {
		return "", err
	}
	if trashed {
		return fmt.Sprintf("wrote %s (previous version is in the trash)", guest), nil
	}
	return fmt.Sprintf("wrote %s", guest), nil
}

// sameContent reports whether the file already holds exactly body.
func sameContent(path string, size int64, body string) (bool, error) {
	if int64(len(body)) != size {
		return false, nil
	}
	fh, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer fh.Close() //nolint:errcheck
	buf := make([]byte, 1<<20)
	at := 0
	for at < len(body) {
		n, err := fh.Read(buf)
		if n == 0 {
			return false, nil
		}
		if string(buf[:n]) != body[at:at+n] {
			return false, nil
		}
		at += n
		if err != nil {
			return false, nil
		}
	}
	return true, nil
}

// editFile implements edit_file: exact-text replacement via a temp file and
// rename, with the previous version trashed.
func (f *shippedFS) editFile(_ context.Context, args json.RawMessage) (string, error) {
	var req struct {
		Path    string `json:"path"`
		OldText string `json:"old_text"`
		NewText string `json:"new_text"`
		Expect  any    `json:"expect"`
		Preview bool   `json:"preview"`
	}
	if err := json.Unmarshal(args, &req); err != nil {
		return "", fmt.Errorf("edit_file: %w", err)
	}
	target, err := f.resolveErr(req.Path, "path")
	if err != nil {
		return "", err
	}
	guest := f.guestPath(target)
	if err := f.refuseState(target); err != nil {
		return "", err
	}
	if req.OldText == "" {
		return "", fmt.Errorf("old_text is required and cannot be empty")
	}

	info, err := os.Stat(target)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("no file at %s", guest)
		}
		return "", err
	}
	if info.IsDir() {
		return "", fmt.Errorf("%s is a directory", guest)
	}

	all := false
	want := 1
	switch e := req.Expect.(type) {
	case nil:
	case string:
		if e == "all" {
			all = true
		} else {
			return "", fmt.Errorf("expect must be a positive integer or \"all\", got %q", e)
		}
	case float64:
		if !isInt(e) || int(e) < 1 {
			return "", fmt.Errorf("expect must be a positive integer or \"all\", got %v", e)
		}
		want = int(e)
	default:
		return "", fmt.Errorf("expect must be a positive integer or \"all\", got %v", req.Expect)
	}

	data, err := os.ReadFile(target)
	if err != nil {
		return "", err
	}
	hits := findAllOffsets(data, []byte(req.OldText), all, want)
	if len(hits) == 0 {
		return "", fmt.Errorf("old_text was not found in %s. It must match the file exactly, including indentation and line breaks — read the region first", guest)
	}
	if !all && len(hits) != want {
		got := len(hits)
		if got == want+1 {
			got = want + 1 // report "more than want"
		}
		msg := fmt.Sprintf("old_text occurs %d time(s) in %s, but expect is %d. Include more surrounding text to make it unique, or pass expect to match the count you intend.", got, guest, want)
		if len(hits) > want {
			msg = fmt.Sprintf("old_text occurs more than %d time(s) in %s, but expect is %d. Include more surrounding text to make it unique, or pass expect to match the count you intend.", want, guest, want)
		}
		return "", fmt.Errorf("%s", msg)
	}

	if req.Preview {
		return f.editPreview(target, info, data, hits, req.OldText, req.NewText)
	}

	// Rebuild via temp + rename; the original is trashed first.
	tmp := fmt.Sprintf("%s.nine-edit-%d", target, time.Now().UnixNano())
	var out strings.Builder
	cursor := 0
	for _, at := range hits {
		out.Write(data[cursor:at])
		out.WriteString(req.NewText)
		cursor = at + len(req.OldText)
	}
	out.Write(data[cursor:])
	if err := os.WriteFile(tmp, []byte(out.String()), 0o600); err != nil {
		return "", err
	}
	if err := f.trashTo(target); err != nil {
		os.Remove(tmp) //nolint:errcheck
		return "", err
	}
	if err := os.Rename(tmp, target); err != nil {
		os.Remove(tmp) //nolint:errcheck
		return "", err
	}

	newInfo, _ := os.Stat(target)
	firstLine := 1 + strings.Count(string(data[:hits[0]]), "\n")
	return fmt.Sprintf("replaced %d occurrence(s) in %s; first at line %d, file is now %d bytes", len(hits), guest, firstLine, newInfo.Size()), nil
}

func isInt(f float64) bool { return f == float64(int(f)) }

// findAllOffsets returns byte offsets of every occurrence of needle, stopping
// once want+1 have been found (to detect "more than want").
func findAllOffsets(hay, needle []byte, all bool, want int) []int {
	limit := want + 1
	if all {
		limit = len(hay) + 1
	}
	var out []int
	from := 0
	for from <= len(hay)-len(needle) && len(out) < limit {
		at := strings.Index(string(hay[from:]), string(needle))
		if at < 0 {
			break
		}
		out = append(out, from+at)
		from = from + at + 1
	}
	return out
}

// editPreview renders the diff of each replacement in context without writing.
func (f *shippedFS) editPreview(target string, info os.FileInfo, data []byte, hits []int, oldText, newText string) (string, error) {
	const pad = 2000
	var parts []string
	added, removed := 0, 0
	for _, at := range hits {
		from := max(0, at-pad)
		to := min(len(data), at+len(oldText)+pad)
		region := string(data[from:to])
		idx := strings.Index(region, oldText)
		if idx < 0 {
			continue
		}
		after := region[:idx] + newText + region[idx+len(oldText):]
		h := diffHunks(trimToLines(region, from > 0, to < len(data)), trimToLines(after, from > 0, to < len(data)))
		if h.text == "" {
			continue
		}
		parts = append(parts, h.text)
		added += h.added
		removed += h.removed
	}
	out, _ := json.Marshal(map[string]any{
		"preview":       true,
		"path":          f.guestPath(target),
		"occurrences":   len(hits),
		"added_lines":   added,
		"removed_lines": removed,
		"diff":          strings.Join(parts, "\n"),
		"note":          "Nothing was written. Call again without preview to apply this.",
	})
	return string(out), nil
}

// trimToLines drops the partial first and last lines of a byte window.
func trimToLines(text string, cutHead, cutTail bool) string {
	if cutHead {
		if nl := strings.Index(text, "\n"); nl >= 0 {
			text = text[nl+1:]
		}
	}
	if cutTail {
		if nl := strings.LastIndex(text, "\n"); nl >= 0 {
			text = text[:nl]
		}
	}
	return text
}

// moveFile implements move_file (a rename; refuses an existing destination
// unless overwrite).
func (f *shippedFS) moveFile(_ context.Context, args json.RawMessage) (string, error) {
	var req struct {
		From, To  string
		Overwrite bool `json:"overwrite"`
	}
	if err := json.Unmarshal(args, &req); err != nil {
		return "", fmt.Errorf("move_file: %w", err)
	}
	src, err := f.resolveErr(req.From, "from")
	if err != nil {
		return "", err
	}
	dst, err := f.resolveErr(req.To, "to")
	if err != nil {
		return "", err
	}
	if src == dst {
		return "", fmt.Errorf("from and to are the same path (%s)", f.guestPath(src))
	}
	info, err := os.Stat(src)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("no file at %s", f.guestPath(src))
		}
		return "", err
	}
	_ = info
	existing, statErr := os.Stat(dst)
	if statErr == nil && !req.Overwrite {
		return "", fmt.Errorf("%s already exists. Pass overwrite: true to replace it, or choose another path", f.guestPath(dst))
	}
	if statErr == nil && existing.IsDir() {
		return "", fmt.Errorf("%s is a directory; name the destination file itself", f.guestPath(dst))
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return "", err
	}
	if err := os.Rename(src, dst); err != nil {
		return "", err
	}
	if statErr == nil {
		return fmt.Sprintf("moved %s to %s (replaced)", f.guestPath(src), f.guestPath(dst)), nil
	}
	return fmt.Sprintf("moved %s to %s", f.guestPath(src), f.guestPath(dst)), nil
}

// copyFile implements copy_file (streams; refuses an existing destination unless
// overwrite).
func (f *shippedFS) copyFile(_ context.Context, args json.RawMessage) (string, error) {
	var req struct {
		From, To  string
		Overwrite bool `json:"overwrite"`
	}
	if err := json.Unmarshal(args, &req); err != nil {
		return "", fmt.Errorf("copy_file: %w", err)
	}
	src, err := f.resolveErr(req.From, "from")
	if err != nil {
		return "", err
	}
	dst, err := f.resolveErr(req.To, "to")
	if err != nil {
		return "", err
	}
	if src == dst {
		return "", fmt.Errorf("from and to are the same path (%s)", f.guestPath(src))
	}
	info, err := os.Stat(src)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("no file at %s", f.guestPath(src))
		}
		return "", err
	}
	if info.IsDir() {
		return "", fmt.Errorf("%s is a directory; copy_file copies one file", f.guestPath(src))
	}
	if _, statErr := os.Stat(dst); statErr == nil && !req.Overwrite {
		return "", fmt.Errorf("%s already exists. Pass overwrite: true to replace it, or choose another path", f.guestPath(dst))
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return "", err
	}
	n, err := copyFileBytes(src, dst)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("copied %s to %s (%d bytes)", f.guestPath(src), f.guestPath(dst), n), nil
}

func copyFileBytes(src, dst string) (int64, error) {
	in, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer in.Close() //nolint:errcheck
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(out, in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return n, err
}

// deleteFile implements delete_file: move to trash, never recursive, never .git.
func (f *shippedFS) deleteFile(_ context.Context, args json.RawMessage) (string, error) {
	var req struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(args, &req); err != nil {
		return "", fmt.Errorf("delete_file: %w", err)
	}
	target, err := f.resolveErr(req.Path, "path")
	if err != nil {
		return "", err
	}
	guest := f.guestPath(target)
	if target == f.root {
		return "", fmt.Errorf("refusing to delete the workspace root")
	}
	if err := f.refuseState(target); err != nil {
		return "", fmt.Errorf("%s is Nine's own bookkeeping (the trash lives there). Use restore_file to recover a deleted file", guest)
	}
	rel0, relErr := filepath.Rel(f.root, target)
	if relErr != nil {
		return "", relErr
	}
	for _, part := range strings.Split(filepath.ToSlash(rel0), "/") {
		if part == ".git" {
			return "", fmt.Errorf("refusing to delete %s: it is inside a git directory", guest)
		}
	}
	info, err := os.Stat(target)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("no file at %s", guest)
		}
		return "", err
	}
	if info.IsDir() {
		entries, err := os.ReadDir(target)
		if err != nil {
			return "", err
		}
		if len(entries) > 0 {
			return "", fmt.Errorf("%s is a directory and is not empty. Delete its contents first — there is no recursive delete", guest)
		}
	}
	if err := f.trashTo(target); err != nil {
		return "", err
	}
	if info.IsDir() {
		return fmt.Sprintf("deleted %s (empty directory). It is recoverable with trash_list and restore_file until the trash is swept.", guest), nil
	}
	return fmt.Sprintf("deleted %s (%d bytes). It is recoverable with trash_list and restore_file until the trash is swept.", guest, info.Size()), nil
}

// trashEntryTime parses an entry name's UTC timestamp, or "" if malformed.
func trashEntryTime(name string) string {
	var y, mo, d, h, mi, s int
	if n, _ := fmt.Sscanf(name, "%4d%2d%2dT%2d%2d%2dZ-", &y, &mo, &d, &h, &mi, &s); n != 6 {
		return ""
	}
	return fmt.Sprintf("%04d-%02d-%02dT%02d:%02d:%02dZ", y, mo, d, h, mi, s)
}

// walkTrash lists the files an entry holds, as workspace-relative paths.
func (f *shippedFS) walkTrash(dir, prefix string, out *[]trashFile) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		full := filepath.Join(dir, e.Name())
		rel := prefix + "/" + e.Name()
		if prefix == "" {
			rel = e.Name()
		}
		info, err := os.Stat(full)
		if err != nil {
			continue
		}
		if info.IsDir() {
			f.walkTrash(full, rel, out)
			continue
		}
		*out = append(*out, trashFile{trashed: full, original: guestRoot + "/" + filepath.ToSlash(rel), size: info.Size()})
	}
}

type trashFile struct {
	trashed  string
	original string
	size     int64
}

// trashList implements trash_list: newest first.
func (f *shippedFS) trashList(_ context.Context, args json.RawMessage) (string, error) {
	var req struct {
		Path  string `json:"path"`
		Limit int    `json:"limit"`
	}
	if err := json.Unmarshal(args, &req); err != nil {
		return "", fmt.Errorf("trash_list: %w", err)
	}
	if _, err := os.Stat(f.trashDir()); err != nil {
		out, _ := json.Marshal(map[string]any{"entries": []any{}, "note": "the trash is empty"})
		return string(out), nil
	}
	filter := strings.TrimSpace(req.Path)
	cap := req.Limit
	if cap <= 0 {
		cap = 50
	}
	names, err := os.ReadDir(f.trashDir())
	if err != nil {
		return "", err
	}
	var valid []string
	for _, e := range names {
		if trashEntryTime(e.Name()) != "" {
			valid = append(valid, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(valid))) // newest first

	var entries []map[string]any
	for _, name := range valid {
		var files []trashFile
		f.walkTrash(filepath.Join(f.trashDir(), name), "", &files)
		var matched []map[string]any
		for _, tf := range files {
			if filter != "" && !strings.Contains(tf.original, filter) {
				continue
			}
			matched = append(matched, map[string]any{"original_path": tf.original, "size": tf.size})
		}
		if len(matched) == 0 {
			continue
		}
		entries = append(entries, map[string]any{"entry": name, "deleted_at": trashEntryTime(name), "files": matched})
		if len(entries) >= cap {
			break
		}
	}
	if len(entries) == 0 {
		note := "the trash is empty"
		if filter != "" {
			note = fmt.Sprintf("nothing in the trash matches %s", filter)
		}
		out, _ := json.Marshal(map[string]any{"entries": []any{}, "note": note})
		return string(out), nil
	}
	out, _ := json.Marshal(map[string]any{"entries": entries})
	return string(out), nil
}

// restoreFile implements restore_file: never overwrites.
func (f *shippedFS) restoreFile(_ context.Context, args json.RawMessage) (string, error) {
	var req struct {
		Entry string `json:"entry"`
		Path  string `json:"path"`
		To    string `json:"to"`
	}
	if err := json.Unmarshal(args, &req); err != nil {
		return "", fmt.Errorf("restore_file: %w", err)
	}
	name := strings.TrimSpace(req.Entry)
	if name == "" {
		return "", fmt.Errorf("entry is required — list them with trash_list")
	}
	if strings.Contains(name, "/") {
		return "", fmt.Errorf("entry must be a trash entry name, not a path (got %s)", name)
	}
	dir := filepath.Join(f.trashDir(), name)
	if _, err := os.Stat(dir); err != nil {
		return "", fmt.Errorf("no trash entry named %s. It may have been swept; list what remains with trash_list", name)
	}
	var files []trashFile
	f.walkTrash(dir, "", &files)
	if len(files) == 0 {
		return "", fmt.Errorf("trash entry %s holds no files", name)
	}
	want := strings.TrimSpace(req.Path)
	var chosen []trashFile
	if want != "" {
		for _, tf := range files {
			if strings.Contains(tf.original, want) {
				chosen = append(chosen, tf)
			}
		}
	} else {
		chosen = files
	}
	if len(chosen) == 0 {
		return "", fmt.Errorf("trash entry %s has no file matching %s", name, want)
	}
	if len(chosen) > 1 {
		names := make([]string, 0, len(chosen))
		for _, tf := range chosen {
			names = append(names, tf.original)
		}
		return "", fmt.Errorf("trash entry %s holds %d files; pass path to choose one of: %s", name, len(chosen), strings.Join(names, ", "))
	}
	file := chosen[0]
	dest := file.original
	if strings.TrimSpace(req.To) != "" {
		d, err := f.resolveErr(req.To, "to")
		if err != nil {
			return "", err
		}
		dest = f.guestPath(d)
	}
	destHost, err := f.resolveErr(dest, "to")
	if err != nil {
		return "", err
	}
	if _, statErr := os.Stat(destHost); statErr == nil {
		return "", fmt.Errorf("%s already exists. Restoring over it would destroy the current file — pass to with a free path if you want both", dest)
	}
	if err := os.MkdirAll(filepath.Dir(destHost), 0o700); err != nil {
		return "", err
	}
	if err := os.Rename(file.trashed, destHost); err != nil {
		return "", err
	}
	if dest == file.original {
		return fmt.Sprintf("restored %s", dest), nil
	}
	return fmt.Sprintf("restored %s (originally %s)", dest, file.original), nil
}

// diffFile implements diff_file: unified diff against the most recent trashed
// version (or a named trash entry).
func (f *shippedFS) diffFile(_ context.Context, args json.RawMessage) (string, error) {
	var req struct {
		Path    string `json:"path"`
		Against string `json:"against"`
	}
	if err := json.Unmarshal(args, &req); err != nil {
		return "", fmt.Errorf("diff_file: %w", err)
	}
	target, err := f.resolveErr(req.Path, "path")
	if err != nil {
		return "", err
	}
	guest := f.guestPath(target)
	rel, _ := filepath.Rel(f.root, target)

	currentInfo, currentErr := os.Stat(target)
	currentText := ""
	currentTruncated := false
	if currentErr == nil {
		currentText, currentTruncated = readWholeCapped(target, currentInfo.Size())
	}

	entry := strings.TrimSpace(req.Against)
	entry = strings.TrimPrefix(entry, "trash:")

	var prevPath string
	var prevSize int64
	var prevAt string
	if entry != "" {
		cand := filepath.Join(f.trashDir(), entry, rel)
		info, err := os.Stat(cand)
		if err != nil || info.IsDir() {
			return "", fmt.Errorf("trash entry %s does not hold %s", entry, guest)
		}
		prevPath, prevSize, prevAt = cand, info.Size(), trashEntryTime(entry)
	} else {
		if _, err := os.Stat(f.trashDir()); err != nil {
			return "", fmt.Errorf("nothing is in the trash, so %s has no previous version to compare", guest)
		}
		names, _ := os.ReadDir(f.trashDir())
		var valid []string
		for _, e := range names {
			if trashEntryTime(e.Name()) != "" {
				valid = append(valid, e.Name())
			}
		}
		sort.Sort(sort.Reverse(sort.StringSlice(valid)))
		for _, name := range valid {
			cand := filepath.Join(f.trashDir(), name, rel)
			if info, err := os.Stat(cand); err == nil && !info.IsDir() {
				prevPath, prevSize, prevAt, entry = cand, info.Size(), trashEntryTime(name), name
				break
			}
		}
		if prevPath == "" {
			return "", fmt.Errorf("no previous version of %s is in the trash. Only a file that write_file, edit_file or delete_file has changed has one", guest)
		}
	}

	prevText, prevTrunc := readWholeCapped(prevPath, prevSize)
	h := diffHunks(prevText, currentText)

	note := ""
	if h.added == 0 && h.removed == 0 {
		note = "No line-level difference between the two versions."
	}
	out, _ := json.Marshal(map[string]any{
		"path":          guest,
		"against":       entry,
		"previous_at":   prevAt,
		"deleted":       currentErr != nil,
		"added_lines":   h.added,
		"removed_lines": h.removed,
		"diff":          h.text,
		"truncated":     h.truncated || prevTrunc || currentTruncated,
		"note":          note,
	})
	return string(out), nil
}

const maxDiffSide = 1 << 20

// readWholeCapped reads a file up to maxDiffSide bytes; reports truncation.
func readWholeCapped(path string, size int64) (string, bool) {
	take := min(size, maxDiffSide)
	fh, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer fh.Close() //nolint:errcheck
	buf := make([]byte, take)
	n, _ := fh.Read(buf)
	return string(buf[:n]), take < size
}
