package toolvm

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// patternRE finds the programs in skills/process-patterns.md: a ```js block
// whose first line names the pattern and its capabilities.
var patternRE = regexp.MustCompile("(?s)```js\n// pattern: ([a-z]+) — capabilities (\\{[^\n]*\\})\n(.*?)```")

// Every program the process-patterns skill offers runs as written: it loads
// with the capabilities it names, handles two clock triggers in the sandbox,
// and is still running — so the skill cannot drift into examples that fail.
func TestProcessPatternsRun(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "skills", "process-patterns.md"))
	if err != nil {
		t.Fatal(err)
	}
	matches := patternRE.FindAllStringSubmatch(string(raw), -1)
	if len(matches) < 5 {
		t.Fatalf("found %d patterns in the skill, want 5", len(matches))
	}
	for _, m := range matches {
		name, caps, src := m[1], m[2], m[3]
		t.Run(name, func(t *testing.T) {
			work := t.TempDir()
			// The summarizer and the watcher read files a person would write.
			_ = os.WriteFile(filepath.Join(work, "notes.md"), []byte("a note"), 0o600)
			_ = os.WriteFile(filepath.Join(work, "status.txt"), []byte("green"), 0o600)
			var decl Declaration
			if err := json.Unmarshal([]byte(caps), &decl); err != nil {
				t.Fatalf("capabilities %s: %v", caps, err)
			}
			mount := []Mount{{Host: work, Guest: "/work"}}
			h, err := Open(context.Background(), Config{Timeout: 2 * time.Second})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { h.Close(context.Background()) }) //nolint:errcheck
			h.SetAgentConfig(AgentConfig{Enabled: true,
				Ceiling: Ceiling{Grant{FSRead: mount, FSWrite: mount, SQL: true}}})
			h.LoadGenerated(context.Background(), []Generated{{Name: name, Source: src, Declaration: decl}}, nil)
			if h.Get(name) == nil {
				t.Fatalf("pattern %s did not load: %+v", name, h.Status())
			}
			f := newFakeProcess()
			l, err := h.StartLive(context.Background(), name, json.RawMessage(`{}`), f)
			if err != nil {
				t.Fatal(err)
			}
			for range 2 {
				f.triggers <- Trigger{Kind: "clock", At: time.Now()}
			}
			time.Sleep(500 * time.Millisecond)
			select {
			case <-l.Done():
				_, err := l.Wait()
				t.Fatalf("pattern %s ended: %v", name, err)
			default:
			}
			l.Stop()
			if _, err := waitLive(t, l); !errors.Is(err, ErrStopped) {
				t.Errorf("pattern %s: %v", name, err)
			}
			if name == "heartbeat" {
				if b, _ := os.ReadFile(filepath.Join(work, "heartbeat.log")); strings.Count(string(b), "\n") != 2 {
					t.Errorf("heartbeat.log = %q, want two lines", b)
				}
			}
		})
	}
}

// A relative path in nine:fs is read against the workspace when the tool has
// it mounted at /work, as every other file tool reads one.
func TestRelativePathsResolveInTheWorkspace(t *testing.T) {
	work := t.TempDir()
	if err := os.MkdirAll(filepath.Join(work, "notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "notes", "inbox.md"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	mount := []Mount{{Host: work, Guest: "/work"}}
	h, err := Open(context.Background(), Config{Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close(context.Background()) }) //nolint:errcheck
	h.SetAgentConfig(AgentConfig{Enabled: true, Ceiling: Ceiling{Grant{FSRead: mount, FSWrite: mount}}})
	h.LoadGenerated(context.Background(), []Generated{{Name: "rel", Declaration: Declaration{FS: []string{"read", "write"}},
		Source: `import { readFileText, writeFile, exists } from "nine:fs";
export default () => {
  writeFile("notes/digest.md", readFileText("./notes/inbox.md") + " world");
  return String(exists("notes/digest.md"));
};`}}, nil)
	out, err := h.Call(context.Background(), "rel", json.RawMessage(`{}`))
	if err != nil || out != "true" {
		t.Fatalf("call = %q, %v", out, err)
	}
	if b, _ := os.ReadFile(filepath.Join(work, "notes", "digest.md")); string(b) != "hello world" {
		t.Errorf("digest.md = %q", b)
	}
}
