package docker_test

// Image contract tests. These need a built image and a running Docker daemon,
// so they are opt-in:
//
//	NINE_IMAGE_TEST=1 NINE_TEST_IMAGE=nine:verify go test ./tests/docker/...
//
// The release workflow sets both, and also NINE_TEST_VERSION, before it will
// push anything. `make image-test` runs them locally.
//
// What they assert is the contract a published image owes its users: it runs
// unprivileged, it carries no build toolchain or credentials, its durable state
// lands on the volume, and the version it reports is the version on the tag.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

const (
	dockerTimeout = 90 * time.Second
	readyTimeout  = 90 * time.Second
)

func imageRef(t *testing.T) string {
	t.Helper()
	if os.Getenv("NINE_IMAGE_TEST") != "1" {
		t.Skip("image tests skipped (set NINE_IMAGE_TEST=1 and NINE_TEST_IMAGE to run)")
	}
	ref := os.Getenv("NINE_TEST_IMAGE")
	if ref == "" {
		t.Fatal("NINE_IMAGE_TEST=1 but NINE_TEST_IMAGE is empty")
	}
	return ref
}

// dockerRun runs a docker command and returns trimmed stdout.
func dockerRun(t *testing.T, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), dockerTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "docker", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		return strings.TrimSpace(stdout.String()),
			fmt.Errorf("docker %s: %w: %s", strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimSpace(stdout.String()), nil
}

func mustDocker(t *testing.T, args ...string) string {
	t.Helper()
	out, err := dockerRun(t, args...)
	if err != nil {
		t.Fatalf("%v", err)
	}
	return out
}

// startContainer boots the image and returns its id, registering cleanup.
func startContainer(t *testing.T, extraArgs ...string) string {
	t.Helper()
	ref := imageRef(t)

	args := []string{"run", "-d", "--rm"}
	args = append(args, extraArgs...)
	args = append(args, ref)

	id, err := dockerRun(t, args...)
	if err != nil {
		t.Fatalf("start container: %v", err)
	}
	t.Cleanup(func() {
		_, _ = dockerRun(t, "rm", "-f", id)
	})

	waitForDaemon(t, id)
	return id
}

// waitForDaemon blocks until `nine status` succeeds inside the container, which
// is the same check the image's HEALTHCHECK runs.
func waitForDaemon(t *testing.T, id string) {
	t.Helper()
	deadline := time.Now().Add(readyTimeout)
	var lastErr error
	for time.Now().Before(deadline) {
		if _, err := dockerRun(t, "exec", id, "nine", "status"); err == nil {
			return
		} else {
			lastErr = err
		}
		time.Sleep(2 * time.Second)
	}
	logs, _ := dockerRun(t, "logs", id)
	t.Fatalf("daemon not ready within %s: %v\ncontainer logs:\n%s", readyTimeout, lastErr, logs)
}

// TestDaemonRunsAsNonRoot is the central claim of the hardened image. s6-overlay
// stays root so it can reap orphans and forward signals, but nothing that
// executes agent-reachable work may.
func TestDaemonRunsAsNonRoot(t *testing.T) {
	id := startContainer(t)

	// docker top reports the host-side view of every process in the container,
	// which is harder to fool than asking the container about itself.
	out := mustDocker(t, "top", id, "-eo", "user,args")

	var sawDaemon bool
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "nine daemon") && !strings.Contains(line, "nine api") {
			continue
		}
		sawDaemon = true
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		user := fields[0]
		if user == "root" || user == "0" {
			t.Errorf("a nine process runs as root: %s", line)
		}
	}
	if !sawDaemon {
		t.Fatalf("no nine daemon or api process found in:\n%s", out)
	}
}

// TestServiceUserIsUID1000 pins the uid rather than just "not root", because a
// bind-mounted /data from a host user is only writable if the uid matches what
// the docs tell people to expect.
func TestServiceUserIsUID1000(t *testing.T) {
	ref := imageRef(t)
	out := mustDocker(t, "run", "--rm", "--entrypoint", "id", ref, "-u", "nine")
	if out != "1000" {
		t.Errorf("nine user is uid %q, want 1000 (docs/docker-image.md documents 1000)", out)
	}
}

// TestDataIsWritableByServiceUser catches the failure the init-perms oneshot
// exists to prevent: a volume mounted over the image's /data arrives root-owned,
// and the daemon cannot write its database.
func TestDataIsWritableByServiceUser(t *testing.T) {
	vol := fmt.Sprintf("nine-test-vol-%d", time.Now().UnixNano())
	t.Cleanup(func() { _, _ = dockerRun(t, "volume", "rm", "-f", vol) })

	id := startContainer(t, "-v", vol+":/data")

	if _, err := dockerRun(t, "exec", "-u", "1000", id, "sh", "-c", "touch /data/.probe && rm /data/.probe"); err != nil {
		owner, _ := dockerRun(t, "exec", id, "stat", "-c", "%u:%g", "/data")
		t.Fatalf("uid 1000 cannot write /data (owned %s): %v", owner, err)
	}

	// The database must land on the volume, not in the container's writable
	// layer, which is discarded when the container is replaced.
	if _, err := dockerRun(t, "exec", id, "test", "-f", "/data/nine.db"); err != nil {
		listing, _ := dockerRun(t, "exec", id, "ls", "-la", "/data")
		t.Errorf("no database at /data/nine.db; /data holds:\n%s", listing)
	}
}

// TestNoBuildToolchainInImage asserts the runtime image ships what Nine needs
// and nothing that would let it — or anything that compromises it — build code.
func TestNoBuildToolchainInImage(t *testing.T) {
	ref := imageRef(t)

	for _, bin := range []string{"go", "gcc", "cc", "git", "node", "npm", "npx", "swag", "make", "apt-get-source"} {
		bin := bin
		t.Run(bin, func(t *testing.T) {
			out, err := dockerRun(t, "run", "--rm", "--entrypoint", "sh", ref, "-c",
				"command -v "+bin+" 2>/dev/null || true")
			if err != nil {
				t.Fatalf("probe for %s: %v", bin, err)
			}
			if out != "" {
				t.Errorf("%s is present in the runtime image at %s", bin, out)
			}
		})
	}
}

// TestNoSourceTreeInImage asserts the image carries the binary, not the repo.
func TestNoSourceTreeInImage(t *testing.T) {
	ref := imageRef(t)
	for _, path := range []string{"/nine-src", "/go", "/usr/local/go"} {
		out, err := dockerRun(t, "run", "--rm", "--entrypoint", "sh", ref, "-c",
			"test -e "+path+" && echo present || true")
		if err != nil {
			t.Fatalf("probe %s: %v", path, err)
		}
		if strings.TrimSpace(out) == "present" {
			t.Errorf("%s exists in the runtime image", path)
		}
	}
}

// TestNoSecretsInImageConfig checks the image's baked-in environment for
// anything credential-shaped. Build args become layer history, and an ARG that
// carried a token would be readable by anyone who pulls the image.
func TestNoSecretsInImageConfig(t *testing.T) {
	ref := imageRef(t)
	raw := mustDocker(t, "image", "inspect", ref)

	var inspected []struct {
		Config struct {
			Env  []string `json:"Env"`
			User string   `json:"User"`
		} `json:"Config"`
	}
	if err := json.Unmarshal([]byte(raw), &inspected); err != nil {
		t.Fatalf("parse docker image inspect: %v", err)
	}
	if len(inspected) != 1 {
		t.Fatalf("expected one image, got %d", len(inspected))
	}

	suspicious := []string{"TOKEN", "SECRET", "PASSWORD", "APIKEY", "API_KEY", "CREDENTIAL", "PRIVATE_KEY"}
	for _, kv := range inspected[0].Config.Env {
		name, value, _ := strings.Cut(kv, "=")
		if value == "" {
			continue
		}
		upper := strings.ToUpper(name)
		for _, marker := range suspicious {
			if strings.Contains(upper, marker) {
				t.Errorf("image environment carries a credential-shaped variable with a value: %s", name)
			}
		}
	}
}

// TestImageLabels asserts the OCI metadata a registry and a scanner read is
// present and, where the release sets it, correct.
func TestImageLabels(t *testing.T) {
	ref := imageRef(t)
	raw := mustDocker(t, "image", "inspect", ref)

	var inspected []struct {
		Config struct {
			Labels map[string]string `json:"Labels"`
		} `json:"Config"`
	}
	if err := json.Unmarshal([]byte(raw), &inspected); err != nil {
		t.Fatalf("parse docker image inspect: %v", err)
	}
	labels := inspected[0].Config.Labels

	for _, key := range []string{
		"org.opencontainers.image.title",
		"org.opencontainers.image.description",
		"org.opencontainers.image.source",
		"org.opencontainers.image.licenses",
		"org.opencontainers.image.version",
		"org.opencontainers.image.revision",
	} {
		if labels[key] == "" {
			t.Errorf("missing label %s", key)
		}
	}

	if want := os.Getenv("NINE_TEST_VERSION"); want != "" {
		if got := labels["org.opencontainers.image.version"]; got != want {
			t.Errorf("image version label = %q, want %q", got, want)
		}
	}
}

// TestReportedVersionMatchesBuild asserts `nine version` agrees with the version
// the image was built with. A mismatch means the ldflags injection broke, and
// every bug report from that image would carry the wrong version.
func TestReportedVersionMatchesBuild(t *testing.T) {
	ref := imageRef(t)
	want := os.Getenv("NINE_TEST_VERSION")
	if want == "" {
		t.Skip("NINE_TEST_VERSION not set")
	}

	out, err := dockerRun(t, "run", "--rm", "--entrypoint", "nine", ref, "version")
	if err != nil {
		t.Fatalf("nine version: %v", err)
	}
	if !strings.Contains(out, want) {
		t.Errorf("nine version printed %q, which does not contain the build version %q", out, want)
	}
}

// TestDefaultConfigIsPresent asserts the image ships a usable config, which is
// what lets someone run it without cloning the repository.
func TestDefaultConfigIsPresent(t *testing.T) {
	ref := imageRef(t)
	out, err := dockerRun(t, "run", "--rm", "--entrypoint", "cat", ref, "/nine.toml")
	if err != nil {
		t.Fatalf("no /nine.toml in the image: %v", err)
	}
	for _, want := range []string{`path = "/data/nine.db"`, `root = "/data/workspace"`} {
		if !strings.Contains(out, want) {
			t.Errorf("baked /nine.toml is missing %s", want)
		}
	}
}

// TestHealthcheckReportsHealthy exercises the HEALTHCHECK as Docker runs it,
// rather than trusting that the command in the Dockerfile would work.
func TestHealthcheckReportsHealthy(t *testing.T) {
	id := startContainer(t)

	deadline := time.Now().Add(readyTimeout)
	var status string
	for time.Now().Before(deadline) {
		status = mustDocker(t, "inspect", "--format", "{{.State.Health.Status}}", id)
		if status == "healthy" {
			return
		}
		if status == "unhealthy" {
			logs, _ := dockerRun(t, "logs", id)
			t.Fatalf("container reported unhealthy\nlogs:\n%s", logs)
		}
		time.Sleep(3 * time.Second)
	}
	t.Fatalf("healthcheck still %q after %s", status, readyTimeout)
}
