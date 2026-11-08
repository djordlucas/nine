package agent

import (
	"context"
	"encoding/json"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The shipped tools as Go handlers (adr/modular_nine.md §9.10): same names,
// same confinement, same trash semantics as the sandboxed tier they replaced.

func newShipped(t *testing.T) (*Dispatcher, string) {
	t.Helper()
	root := t.TempDir()
	d := New()
	RegisterShippedTools(d, root)
	return d, root
}

func callTool(t *testing.T, d *Dispatcher, tool string, args string) (string, error) {
	t.Helper()
	cr, err := d.Dispatch(context.Background(), tool, json.RawMessage(args))
	if err != nil {
		return "", err
	}
	return cr.Output, nil
}

func mustCall(t *testing.T, d *Dispatcher, tool, args string) string {
	t.Helper()
	out, err := callTool(t, d, tool, args)
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	return out
}

func TestShippedTimeReturnsTheClock(t *testing.T) {
	d, _ := newShipped(t)
	out := mustCall(t, d, "time", `{}`)
	var got struct {
		ISO8601  string `json:"iso8601"`
		Human    string `json:"human"`
		Timezone string `json:"timezone"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("unmarshal %q: %v", out, err)
	}
	if !strings.HasSuffix(got.ISO8601, "Z") || got.Timezone != "UTC" || !strings.HasSuffix(got.Human, "UTC") {
		t.Errorf("time = %+v, want UTC ISO-8601 and human forms", got)
	}
}

func TestShippedWriteFileCreatesParentsAndReadsBack(t *testing.T) {
	d, root := newShipped(t)
	mustCall(t, d, "write_file", `{"path":"notes/2026/today.md","content":"hello"}`)
	got, err := os.ReadFile(filepath.Join(root, "notes", "2026", "today.md"))
	if err != nil {
		t.Fatalf("file not written to the host: %v", err)
	}
	if string(got) != "hello" {
		t.Errorf("content = %q, want hello", got)
	}
	if out := mustCall(t, d, "read_file", `{"path":"notes/2026/today.md"}`); out != "hello" {
		t.Errorf("read_file = %q, want hello", out)
	}
}

// The narrowing the sandbox enforced: reads are confined to the workspace, where
// the old plugin read any absolute path on the host.
func TestShippedReadFileIsConfinedToTheWorkspace(t *testing.T) {
	d, _ := newShipped(t)
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("not yours"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := callTool(t, d, "read_file", `{"path":`+strconvQuote(outside)+`}`)
	if err == nil {
		t.Fatal("read_file reached a file outside the workspace")
	}
	if !strings.Contains(err.Error(), "outside this tool's workspace") {
		t.Errorf("error = %v, want it to explain the confinement", err)
	}
}

// /work is the alias the tools accept, so a model that learned it keeps working.
func TestShippedFilesAcceptWorkAlias(t *testing.T) {
	d, root := newShipped(t)
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("via-alias"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out := mustCall(t, d, "read_file", `{"path":"/work/a.txt"}`); out != "via-alias" {
		t.Errorf("read_file /work/a.txt = %q, want via-alias", out)
	}
}

func TestShippedEditFileReplacesExactText(t *testing.T) {
	d, root := newShipped(t)
	p := filepath.Join(root, "f.txt")
	if err := os.WriteFile(p, []byte("alpha\nbeta\ngamma\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := mustCall(t, d, "edit_file", `{"path":"f.txt","old_text":"beta","new_text":"BETA"}`)
	if !strings.Contains(out, "replaced 1 occurrence(s)") {
		t.Errorf("out = %q", out)
	}
	got, _ := os.ReadFile(p)
	if string(got) != "alpha\nBETA\ngamma\n" {
		t.Errorf("file = %q", got)
	}
}

func TestShippedEditFileRefusesAmbiguousMatch(t *testing.T) {
	d, root := newShipped(t)
	p := filepath.Join(root, "f.txt")
	os.WriteFile(p, []byte("x\nx\n"), 0o600) //nolint:errcheck
	if _, err := callTool(t, d, "edit_file", `{"path":"f.txt","old_text":"x","new_text":"y"}`); err == nil || !strings.Contains(err.Error(), "time(s) in /work/f.txt") {
		t.Errorf("err = %v, want the ambiguous-match refusal", err)
	}
	// expect: "all" replaces both.
	mustCall(t, d, "edit_file", `{"path":"f.txt","old_text":"x","new_text":"y","expect":"all"}`)
	got, _ := os.ReadFile(p)
	if string(got) != "y\ny\n" {
		t.Errorf("after expect=all: file = %q", got)
	}
}

func TestShippedEditFileReportsMissingText(t *testing.T) {
	d, root := newShipped(t)
	os.WriteFile(filepath.Join(root, "f.txt"), []byte("abc"), 0o600) //nolint:errcheck
	if _, err := callTool(t, d, "edit_file", `{"path":"f.txt","old_text":"zzz","new_text":"y"}`); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("err = %v, want not-found", err)
	}
}

func TestShippedEditFilePreviewWritesNothing(t *testing.T) {
	d, root := newShipped(t)
	p := filepath.Join(root, "f.txt")
	os.WriteFile(p, []byte("keep\n"), 0o600) //nolint:errcheck
	out := mustCall(t, d, "edit_file", `{"path":"f.txt","old_text":"keep","new_text":"gone","preview":true}`)
	if !strings.Contains(out, "Nothing was written") {
		t.Errorf("preview out = %q", out)
	}
	got, _ := os.ReadFile(p)
	if string(got) != "keep\n" {
		t.Errorf("preview changed the file: %q", got)
	}
}

func TestShippedMoveAndCopyFile(t *testing.T) {
	d, root := newShipped(t)
	os.WriteFile(filepath.Join(root, "a.txt"), []byte("data"), 0o600) //nolint:errcheck
	mustCall(t, d, "copy_file", `{"from":"a.txt","to":"b.txt"}`)
	mustCall(t, d, "move_file", `{"from":"a.txt","to":"c.txt"}`)
	if _, err := os.Stat(filepath.Join(root, "a.txt")); !os.IsNotExist(err) {
		t.Error("a.txt should be gone after move")
	}
	for _, name := range []string{"b.txt", "c.txt"} {
		got, _ := os.ReadFile(filepath.Join(root, name))
		if string(got) != "data" {
			t.Errorf("%s = %q, want data", name, got)
		}
	}
	// move refuses an existing destination unless overwrite
	if _, err := callTool(t, d, "move_file", `{"from":"b.txt","to":"c.txt"}`); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("err = %v, want exists refusal", err)
	}
	mustCall(t, d, "move_file", `{"from":"b.txt","to":"c.txt","overwrite":true}`)
}

func TestShippedReadFileLineRangeAndBinaryRefusal(t *testing.T) {
	d, root := newShipped(t)
	os.WriteFile(filepath.Join(root, "f.txt"), []byte("l1\nl2\nl3\nl4\n"), 0o600) //nolint:errcheck
	out := mustCall(t, d, "read_file", `{"path":"f.txt","lines":"2-3"}`)
	var lr struct {
		Content   string `json:"content"`
		FirstLine int    `json:"first_line"`
		Lines     int    `json:"lines"`
	}
	if err := json.Unmarshal([]byte(out), &lr); err != nil {
		t.Fatalf("unmarshal %q: %v", out, err)
	}
	if lr.Content != "l2\nl3" || lr.FirstLine != 2 || lr.Lines != 2 {
		t.Errorf("line range = %+v", lr)
	}
	// whole-file read stays a plain string
	if out := mustCall(t, d, "read_file", `{"path":"f.txt"}`); out != "l1\nl2\nl3\nl4\n" {
		t.Errorf("whole-file = %q", out)
	}
	// binary refusal
	os.WriteFile(filepath.Join(root, "bin.dat"), []byte{'a', 0, 'b'}, 0o600) //nolint:errcheck
	if _, err := callTool(t, d, "read_file", `{"path":"bin.dat"}`); err == nil || !strings.Contains(err.Error(), "not a text file") {
		t.Errorf("err = %v, want binary refusal", err)
	}
}

func TestShippedWriteFileAppendAndIfUnchanged(t *testing.T) {
	d, root := newShipped(t)
	mustCall(t, d, "write_file", `{"path":"log.txt","content":"one\n"}`)
	out := mustCall(t, d, "write_file", `{"path":"log.txt","content":"two\n","mode":"append"}`)
	if !strings.Contains(out, "appended") {
		t.Errorf("append out = %q", out)
	}
	got, _ := os.ReadFile(filepath.Join(root, "log.txt"))
	if string(got) != "one\ntwo\n" {
		t.Errorf("file = %q", got)
	}

	// if_unchanged with a stale token fails; with the current token succeeds.
	rout := mustCall(t, d, "read_file", `{"path":"log.txt","offset":0,"limit":1}`)
	var wr struct {
		Version string `json:"version"`
	}
	json.Unmarshal([]byte(rout), &wr) //nolint:errcheck
	if _, err := callTool(t, d, "write_file", `{"path":"log.txt","content":"x","if_unchanged":"bogus"}`); err == nil || !strings.Contains(err.Error(), "changed since you read it") {
		t.Errorf("stale token err = %v, want conflict", err)
	}
	mustCall(t, d, "write_file", `{"path":"log.txt","content":"x","if_unchanged":"`+wr.Version+`"}`)
}

func TestShippedTrashCycle(t *testing.T) {
	d, root := newShipped(t)
	mustCall(t, d, "write_file", `{"path":"doc.md","content":"v1"}`)
	mustCall(t, d, "write_file", `{"path":"doc.md","content":"v2"}`) // previous version trashed
	mustCall(t, d, "delete_file", `{"path":"doc.md"}`)

	// trash_list shows the entries, newest first
	out := mustCall(t, d, "trash_list", `{}`)
	if !strings.Contains(out, "doc.md") {
		t.Errorf("trash_list = %q, want doc.md entries", out)
	}
	// diff against the most recent previous version
	dout := mustCall(t, d, "diff_file", `{"path":"doc.md"}`)
	if !strings.Contains(dout, `"deleted":true`) {
		t.Errorf("diff after delete = %q, want deleted:true", dout)
	}
	// restore the last deleted version
	var tl struct {
		Entries []struct {
			Entry string `json:"entry"`
		} `json:"entries"`
	}
	json.Unmarshal([]byte(out), &tl) //nolint:errcheck
	if len(tl.Entries) == 0 {
		t.Fatal("no trash entries")
	}
	// Restore the newest entry (same-second entries order randomly, so accept
	// either trashed version — the point is the restore path works and never
	// clobbers).
	entry := tl.Entries[0].Entry
	rout := mustCall(t, d, "restore_file", `{"entry":"`+entry+`"}`)
	if !strings.Contains(rout, "restored") {
		t.Errorf("restore = %q", rout)
	}
	got, _ := os.ReadFile(filepath.Join(root, "doc.md"))
	if string(got) != "v1" && string(got) != "v2" {
		t.Errorf("restored content = %q, want a trashed version", got)
	}
	// Restoring over an existing destination must fail (never clobbers). The
	// other entry still holds a file; aim it at the restored doc.md.
	other := ""
	for _, e := range tl.Entries {
		if e.Entry != entry {
			other = e.Entry
		}
	}
	if other == "" {
		t.Fatal("expected a second trash entry")
	}
	if _, err := callTool(t, d, "restore_file", `{"entry":"`+other+`","to":"doc.md"}`); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("clobbering restore err = %v, want already-exists refusal", err)
	}
}

// The SSRF gate: dial-time, on the resolved IP, per connection. The metadata
// endpoint is blocked even with a "*" host allowlist.
func TestShippedHTTPBlocksMetadataAndPrivate(t *testing.T) {
	for _, addr := range []string{"169.254.169.254", "127.0.0.1", "10.0.0.5", "192.168.1.1", "0.0.0.0", "100.64.0.1", "fe80::1", "[::1]"} {
		if err := checkDialAddr(addr + ":80"); err == nil {
			t.Errorf("checkDialAddr(%s:80) = nil, want blocked", addr)
		}
	}
	for _, addr := range []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111"} {
		if err := checkDialAddr(addr + ":443"); err != nil {
			t.Errorf("checkDialAddr(%s:443) = %v, want allowed", addr, err)
		}
	}
	// IPv4-mapped IPv6 loopback is unmapped before the check.
	if err := checkIP(netip.MustParseAddr("::ffff:127.0.0.1")); err == nil {
		t.Error("checkIP(::ffff:127.0.0.1) = nil, want blocked")
	}
	// allowlist: exact, subdomain, apex exclusion, wildcard
	if err := allowHost("api.search.brave.com", []string{"api.search.brave.com"}); err != nil {
		t.Errorf("exact host blocked: %v", err)
	}
	if err := allowHost("a.b.example.com", []string{"*.example.com"}); err != nil {
		t.Errorf("subdomain blocked: %v", err)
	}
	if err := allowHost("example.com", []string{"*.example.com"}); err == nil {
		t.Error("apex allowed by *.example.com, want refused")
	}
	if err := allowHost("anything.example.org", []string{"*"}); err != nil {
		t.Errorf("wildcard blocked a public host: %v", err)
	}
}

// http_get to the metadata endpoint is refused at dial time — no network needed,
// the Control hook fires before connect.
func TestShippedHTTPGetBlocksMetadataEndpoint(t *testing.T) {
	d, _ := newShipped(t)
	if _, err := callTool(t, d, "http_get", `{"url":"http://169.254.169.254/latest/meta-data/"}`); err == nil || !strings.Contains(err.Error(), "blocked") {
		t.Errorf("err = %v, want a blocked error", err)
	}
}

func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
