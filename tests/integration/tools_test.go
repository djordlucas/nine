package integration_test

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"
)

// TestTimeTool asks nine to use the time tool and checks the response contains
// the current year.
func TestTimeTool(t *testing.T) {
	out, err := nineQuery(t, "use the time tool to get the current time and tell me the year")
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	year := regexp.MustCompile(`202[0-9]`)
	if !year.MatchString(out) {
		t.Errorf("expected current year (202x) in response, got: %s", out)
	}
	t.Logf("response: %s", out)
}

// TestShellToolEcho gives nine a unique marker to echo via the shell tool and
// confirms the marker appears in the response.
func TestShellToolEcho(t *testing.T) {
	marker := fmt.Sprintf("NINE_INTTEST_%d", time.Now().UnixNano())
	out, err := nineQuery(t,
		fmt.Sprintf("use the shell tool to run this command and show me the output: echo %s", marker),
	)
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	if !strings.Contains(out, marker) {
		t.Errorf("marker %q not found in response: %s", marker, out)
	}
	t.Logf("response: %s", out)
}

// TestFileReadTool asks nine to read a known file inside the container.
func TestFileReadTool(t *testing.T) {
	out, err := nineQuery(t,
		"use the file read tool to read /data/src/nine.toml and tell me what LLM provider is configured",
	)
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	lower := strings.ToLower(out)
	if !strings.Contains(lower, "ollama") && !strings.Contains(lower, "llm") && !strings.Contains(lower, "provider") {
		t.Errorf("expected LLM/provider info in response, got: %s", out)
	}
	t.Logf("response: %s", out)
}

// TestMemoryPersistence stores a value in one conversation and retrieves it in
// the next, verifying cross-turn persistence via the memory plugin.
func TestMemoryPersistence(t *testing.T) {
	key := fmt.Sprintf("inttest_key_%d", time.Now().UnixNano())
	val := fmt.Sprintf("INTTEST_VAL_%d", time.Now().UnixNano())

	// Store in first conversation.
	_, err := nineQuery(t, fmt.Sprintf(
		"use memory_set to store key=%q value=%q then just say 'stored'",
		key, val,
	))
	if err != nil {
		t.Fatalf("store query failed: %v", err)
	}

	// Retrieve in a fresh conversation (new CLI invocation = new conversation).
	out, err := nineQuery(t, fmt.Sprintf(
		"use memory_get to retrieve the value for key=%q and tell me exactly what it is",
		key,
	))
	if err != nil {
		t.Fatalf("retrieve query failed: %v", err)
	}
	if !strings.Contains(out, val) {
		t.Errorf("expected stored value %q in response, got: %s", val, out)
	}
	t.Logf("response: %s", out)
}

// TestSkillsList checks that the default skills are visible inside the container.
func TestSkillsList(t *testing.T) {
	out, err := nineQuery(t, "list all available skills by name")
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	for _, skill := range []string{"git-workflow", "shell-usage", "web-research"} {
		if !strings.Contains(out, skill) {
			t.Errorf("skill %q not found in listing: %s", skill, out)
		}
	}
	t.Logf("response: %s", out)
}

// TestShellSafeCommandAllowed confirms a safe shell command runs successfully.
func TestShellSafeCommandAllowed(t *testing.T) {
	out, err := nineQuery(t, "use the shell tool to run: pwd")
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	// pwd should return a path starting with /
	if !strings.Contains(out, "/") {
		t.Errorf("expected a path in response, got: %s", out)
	}
	t.Logf("response: %s", out)
}
