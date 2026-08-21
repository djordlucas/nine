package toolvm

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The shipped tier's whole purpose: a first-party capability runs inside the
// sandbox instead of as a subprocess holding the daemon's uid authority.
func TestLoadShippedRegistersTimeWithNoCapabilities(t *testing.T) {
	ctx := context.Background()
	h, err := Open(ctx, Config{UserDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer h.Close(ctx) //nolint:errcheck

	h.LoadShipped(ctx, nil)

	tool := h.Get("time")
	if tool == nil {
		t.Fatal("shipped tool \"time\" did not register")
	}
	if !tool.Shipped {
		t.Error("tool is not marked Shipped")
	}
	if tool.Kind != KindJS {
		t.Errorf("Kind = %q, want js", tool.Kind)
	}

	// The point of the migration. As a plugin this had the daemon's full
	// authority; as a tool it has none — no mount, no env, no network.
	g := tool.Grant
	if len(g.FSRead) != 0 || len(g.FSWrite) != 0 {
		t.Errorf("time was granted filesystem access: %+v", g)
	}
	if g.HTTP != nil {
		t.Error("time was granted network access")
	}
	if len(g.Env) != 0 {
		t.Errorf("time was granted env access: %v", g.Env)
	}
}

// It has to actually work, not merely register.
func TestShippedTimeReturnsTheClock(t *testing.T) {
	ctx := context.Background()
	h, err := Open(ctx, Config{UserDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer h.Close(ctx) //nolint:errcheck
	h.LoadShipped(ctx, nil)

	out, err := h.Call(ctx, "time", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	var got struct {
		ISO8601  string `json:"iso8601"`
		Human    string `json:"human"`
		Timezone string `json:"timezone"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("unmarshal %q: %v", out, err)
	}
	// The same three field names the plugin returned, so a prompt or eval
	// expecting them does not move — but the values are UTC now, which is the
	// documented behavior change.
	if got.ISO8601 == "" || !strings.Contains(got.ISO8601, "T") {
		t.Errorf("iso8601 = %q, want an ISO timestamp", got.ISO8601)
	}
	if got.Human == "" {
		t.Errorf("human = %q, want a readable date", got.Human)
	}
	// The guest has no timezone database, so UTC is the only honest answer. The
	// plugin reported the daemon's local zone; a sandboxed tool cannot, and
	// claiming a zone it cannot compute would be worse than saying UTC.
	if got.Timezone != "UTC" {
		t.Errorf("timezone = %q, want UTC", got.Timezone)
	}
	if !strings.HasSuffix(got.ISO8601, "Z") {
		t.Errorf("iso8601 = %q, want a UTC timestamp", got.ISO8601)
	}
	if !strings.HasSuffix(got.Human, "UTC") {
		t.Errorf("human = %q, want it to name the zone it used", got.Human)
	}
}

// A shipped tool loses a name collision like everything else, so a developer or
// generated tool cannot shadow first-party behavior — and neither can a plugin.
func TestLoadShippedYieldsToAnExistingOwner(t *testing.T) {
	ctx := context.Background()
	h, err := Open(ctx, Config{UserDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer h.Close(ctx) //nolint:errcheck

	h.LoadShipped(ctx, func(name string) (string, bool) {
		if name == "time" {
			return "some-plugin", true
		}
		return "", false
	})

	if h.Get("time") != nil {
		t.Error("shipped tool registered over an existing owner")
	}
	var found bool
	for _, s := range h.Status() {
		if s.Name == "time" {
			found = true
			if s.Loaded {
				t.Error("status says loaded despite the collision")
			}
			if !strings.Contains(s.Err, "already provided") {
				t.Errorf("status err = %q, want it to name the collision", s.Err)
			}
		}
	}
	if !found {
		t.Error("a skipped shipped tool must still be reported in Status")
	}
}

// resolveShipped refuses a declaration the tier cannot actually confer, rather
// than registering a tool whose capability silently does nothing.
func TestResolveShippedRefusesUnbackedCapabilities(t *testing.T) {
	if _, err := resolveShipped(Declaration{FS: []string{"read"}}, ShippedWorkspace{}); err == nil {
		t.Error("a shipped fs declaration was accepted with no workspace to back it")
	}
	// With a workspace it resolves, and only to that workspace.
	g2, err := resolveShipped(Declaration{FS: []string{"read", "write"}}, ShippedWorkspace{Host: "/tmp/ws"})
	if err != nil {
		t.Fatalf("fs with a workspace should resolve: %v", err)
	}
	if len(g2.FSRead) != 1 || g2.FSRead[0].Host != "/tmp/ws" || g2.FSRead[0].Guest != "/work" {
		t.Errorf("read mount = %+v, want /tmp/ws at /work", g2.FSRead)
	}
	if len(g2.FSWrite) != 1 || g2.FSWrite[0].Host != "/tmp/ws" {
		t.Errorf("write mount = %+v", g2.FSWrite)
	}
	if _, err := resolveShipped(Declaration{Net: []string{"http"}}, ShippedWorkspace{}); err == nil {
		t.Error("a shipped net.http declaration was accepted with no allowlist")
	}
	if g, err := resolveShipped(Declaration{}, ShippedWorkspace{}); err != nil {
		t.Errorf("an empty declaration should resolve: %v", err)
	} else if len(g.capabilities()) != 0 {
		t.Errorf("empty declaration conferred %v", g.capabilities())
	}
}

// The reason this migration waited for a qjs.wasm bump: the plugin's write_file
// created parent directories, and a sandboxed tool had no way to. A write_file
// that cannot make a directory is a downgrade for the software-dev and sysadmin
// roles, both of which carry it.
func TestShippedWriteFileCreatesParentDirectories(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()
	h, err := Open(ctx, Config{UserDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer h.Close(ctx) //nolint:errcheck
	h.SetShippedWorkspace(ShippedWorkspace{Host: ws})
	h.LoadShipped(ctx, nil)

	// Two levels deep into an empty workspace — the case that used to fail.
	if _, err := h.Call(ctx, "write_file",
		json.RawMessage(`{"path":"notes/2026/today.md","content":"hello"}`)); err != nil {
		t.Fatalf("write_file into a new tree: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(ws, "notes", "2026", "today.md"))
	if err != nil {
		t.Fatalf("file was not written to the host: %v", err)
	}
	if string(got) != "hello" {
		t.Errorf("content = %q, want hello", got)
	}

	// And it reads back through the tool.
	out, err := h.Call(ctx, "read_file", json.RawMessage(`{"path":"notes/2026/today.md"}`))
	if err != nil {
		t.Fatalf("read_file: %v", err)
	}
	if out != "hello" {
		t.Errorf("read_file = %q, want hello", out)
	}
}

// The narrowing you chose: reads are confined to the mount, where the plugin
// read any absolute path on the host.
func TestShippedReadFileIsConfinedToTheWorkspace(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("not yours"), 0o600); err != nil {
		t.Fatal(err)
	}

	h, err := Open(ctx, Config{UserDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer h.Close(ctx) //nolint:errcheck
	h.SetShippedWorkspace(ShippedWorkspace{Host: ws})
	h.LoadShipped(ctx, nil)

	_, err = h.Call(ctx, "read_file", json.RawMessage(`{"path":`+strconv.Quote(outside)+`}`))
	if err == nil {
		t.Fatal("read_file reached a file outside the workspace")
	}
	if !strings.Contains(err.Error(), "outside this tool's workspace") {
		t.Errorf("error = %v, want it to explain the confinement", err)
	}
}

// /work is the alias the plugin accepted, so a model that learned it keeps working.
func TestShippedFilesAcceptWorkAlias(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()
	h, err := Open(ctx, Config{UserDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer h.Close(ctx) //nolint:errcheck
	h.SetShippedWorkspace(ShippedWorkspace{Host: ws})
	h.LoadShipped(ctx, nil)

	if _, err := h.Call(ctx, "write_file",
		json.RawMessage(`{"path":"/work/out.txt","content":"aliased"}`)); err != nil {
		t.Fatalf("write via /work: %v", err)
	}
	out, err := h.Call(ctx, "read_file", json.RawMessage(`{"path":"/work/out.txt"}`))
	if err != nil || out != "aliased" {
		t.Errorf("read via /work = %q, %v", out, err)
	}
}
