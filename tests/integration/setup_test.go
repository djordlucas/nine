// Package integration_test runs end-to-end tests against nine running inside
// Docker with a real LLM (Ollama). Tests are opt-in:
//
//	NINE_INTEGRATION=1 go test ./tests/integration/...
//
// Prerequisites: Docker running, Ollama listening on localhost:11434 with the
// model named by NINE_LLM_MODEL (default gemma4:e4b).
package integration_test

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

const (
	imageTag     = "nine-inttest"
	llmModel     = "gemma4:e4b"
	queryTimeout = 120 * time.Second
)

var containerID string

// TestMain builds the Docker image, starts a container, waits for the nine
// daemon to be ready, runs all tests, then tears down.
func TestMain(m *testing.M) {
	if os.Getenv("NINE_INTEGRATION") != "1" {
		fmt.Println("integration tests skipped (set NINE_INTEGRATION=1 to run)")
		os.Exit(0)
	}

	checkPrereqs()

	log.Println("building Docker image...")
	mustRun("docker", "build", "-t", imageTag, repoRoot())

	containerID = startContainer()
	log.Printf("container started: %s", containerID[:12])

	log.Println("waiting for nine daemon...")
	if err := waitForDaemon(90 * time.Second); err != nil {
		dumpLogs()
		stopContainer()
		log.Fatalf("daemon not ready: %v", err)
	}
	log.Println("daemon ready")

	code := m.Run()

	stopContainer()
	os.Exit(code)
}

// nineQuery runs nine inside the container with the given message and returns
// the trimmed combined output. The query times out after queryTimeout.
func nineQuery(t *testing.T, message string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx,
		"docker", "exec", containerID, "nine", message,
	).CombinedOutput()
	s := strings.TrimSpace(string(out))
	if err != nil {
		t.Logf("nineQuery error: %v\noutput: %s", err, s)
	}
	return s, err
}

// containerExec runs an arbitrary command inside the container and returns stdout.
func containerExec(args ...string) (string, error) {
	all := append([]string{"exec", containerID}, args...)
	out, err := exec.Command("docker", all...).Output()
	return strings.TrimSpace(string(out)), err
}

// ── setup helpers ─────────────────────────────────────────────────────────────

func checkPrereqs() {
	if _, err := exec.LookPath("docker"); err != nil {
		log.Fatal("docker not found in PATH")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		log.Fatal("docker daemon not reachable")
	}

	ollamaURL := "http://localhost:11434"
	resp, err := http.Get(ollamaURL)
	if err != nil || resp.StatusCode != 200 {
		log.Fatalf("ollama not reachable at %s — start it before running integration tests", ollamaURL)
	}
	resp.Body.Close()
}

func startContainer() string {
	model := os.Getenv("NINE_LLM_MODEL")
	if model == "" {
		model = llmModel
	}

	args := []string{
		"run", "-d",
		"-e", "NINE_LLM_PROVIDER=ollama",
		"-e", "NINE_LLM_MODEL=" + model,
		"-e", "NINE_LLM_ENDPOINT=http://host.docker.internal:11434",
		"-e", "NINE_LOG_FILE=off", // logs → stderr → docker logs
		// No nine.toml is mounted, so these two must come from the environment
		// or every plugin silently fails to start (config.DatabasePath's sibling
		// pluginBinPath falls back to a stale "/data/bin" that has never existed
		// in any image layout — see docs/single-container.md; plugins live under
		// /opt/nine/bin, the workspace under /data/workspace).
		"-e", "NINE_PLUGINS_BIN=/opt/nine/bin",
		"-e", "NINE_WORKSPACE_ROOT=/data/workspace",
	}
	if runtime.GOOS == "linux" {
		args = append(args, "--add-host=host.docker.internal:host-gateway")
	}
	args = append(args, imageTag)

	out := mustRun("docker", args...)
	return strings.TrimSpace(out)
}

func waitForDaemon(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, err := exec.Command("docker", "exec", containerID, "nine", "status").Output()
		if err == nil && strings.Contains(string(out), "Plugins") {
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("daemon not ready after %v", timeout)
}

func stopContainer() {
	exec.Command("docker", "stop", containerID).Run() //nolint:errcheck
	exec.Command("docker", "rm", containerID).Run()   //nolint:errcheck
}

func dumpLogs() {
	out, _ := exec.Command("docker", "logs", containerID).CombinedOutput()
	log.Printf("container logs:\n%s", out)
}

func mustRun(name string, args ...string) string {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		log.Fatalf("%s %v failed: %v\n%s", name, args, err, out) //nolint:gosec
	}
	return string(out)
}

func repoRoot() string {
	// Walk up from the test file's package directory to find go.mod.
	dir, _ := os.Getwd()
	for {
		if _, err := os.Stat(dir + "/go.mod"); err == nil {
			return dir
		}
		parent := dir[:strings.LastIndex(dir, "/")]
		if parent == dir {
			log.Fatal("go.mod not found")
		}
		dir = parent
	}
}
