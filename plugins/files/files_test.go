package main_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"nine/internal/plugin"
)

var testBin string

func TestMain(m *testing.M) {
	tmp, _ := os.MkdirTemp("", "nine-files-test-*")
	defer os.RemoveAll(tmp)
	testBin = filepath.Join(tmp, "files")
	cmd := exec.Command("go", "build", "-o", testBin, "./plugins/files")
	cmd.Dir = moduleRoot()
	if b, err := cmd.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build: %v\n%s\n", err, b)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

func moduleRoot() string {
	dir, _ := os.Getwd()
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			panic("go.mod not found")
		}
		dir = parent
	}
}

func start(t *testing.T) (*plugin.Plugin, *plugin.Manager) {
	t.Helper()
	m := plugin.NewManager("")
	p, err := m.Start(testBin)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { m.Stop(p) })
	return p, m
}

func TestDescribe(t *testing.T) {
	p, _ := start(t)
	names := make(map[string]bool)
	for _, tool := range p.Tools {
		names[tool.Name] = true
	}
	for _, want := range []string{"read_file", "write_file"} {
		if !names[want] {
			t.Errorf("missing tool %q", want)
		}
	}
}

func TestWriteAndRead(t *testing.T) {
	p, m := start(t)
	path := filepath.Join(t.TempDir(), "test.txt")

	args, _ := json.Marshal(map[string]string{"path": path, "content": "hello files"})
	r, err := m.Call(context.Background(), p, "write_file", args)
	if err != nil {
		t.Fatalf("write_file: %v", err)
	}
	if r.Output != "ok" {
		t.Errorf("write_file output = %q", r.Output)
	}

	args, _ = json.Marshal(map[string]string{"path": path})
	r, err = m.Call(context.Background(), p, "read_file", args)
	if err != nil {
		t.Fatalf("read_file: %v", err)
	}
	if r.Output != "hello files" {
		t.Errorf("read_file = %q, want %q", r.Output, "hello files")
	}
}

func TestWriteCreatesParentDirs(t *testing.T) {
	p, m := start(t)
	path := filepath.Join(t.TempDir(), "a", "b", "c", "test.txt")
	args, _ := json.Marshal(map[string]string{"path": path, "content": "nested"})
	if _, err := m.Call(context.Background(), p, "write_file", args); err != nil {
		t.Fatalf("write_file nested: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("file not created: %v", err)
	}
}

func TestReadMissingFile(t *testing.T) {
	p, m := start(t)
	args, _ := json.Marshal(map[string]string{"path": "/nonexistent/file.txt"})
	_, err := m.Call(context.Background(), p, "read_file", args)
	if err == nil {
		t.Error("expected error reading missing file")
	}
}
