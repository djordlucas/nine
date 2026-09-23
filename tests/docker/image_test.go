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

// waitForDaemon blocks until the daemon answers inside the container, which is
// the same condition the image's HEALTHCHECK checks.
//
// It matches on "Uptime:" rather than on the exit status, because `nine status`
// prints "no daemon running" and exits 0 when it cannot reach a daemon — the
// query succeeded, it just found nothing. Waiting on the exit status alone
// would return immediately against a container whose daemon never started.
func waitForDaemon(t *testing.T, id string) {
	t.Helper()
	deadline := time.Now().Add(readyTimeout)
	var last string
	for time.Now().Before(deadline) {
		out, err := dockerRun(t, "exec", id, "nine", "status")
		if err == nil && strings.Contains(out, "Uptime:") {
			return
		}
		last = out
		if err != nil {
			last = err.Error()
		}
		time.Sleep(2 * time.Second)
	}
	logs, _ := dockerRun(t, "logs", id)
	t.Fatalf("daemon not ready within %s (last status output: %q)\ncontainer logs:\n%s",
		readyTimeout, last, logs)
}

// TestDaemonRunsAsNonRoot is the central claim of the hardened image. s6-overlay
// stays root so it can reap orphans and forward signals, but nothing that
// executes agent-reachable work may.
func TestDaemonRunsAsNonRoot(t *testing.T) {
	id := startContainer(t)

	// docker top reports the host-side view of every process in the container,
	// which is harder to fool than asking the container about itself.
	//
	// pid has to be in the format string even though nothing here reads it: the
	// daemon parses the ps output to find the PID column and map host pids back
	// to the container, and refuses the request outright without it.
	out := mustDocker(t, "top", id, "-eo", "pid,user,args")

	var sawDaemon bool
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "nine daemon") && !strings.Contains(line, "nine api") {
			continue
		}
		sawDaemon = true
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		user := fields[1]
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

// TestBuiltinPluginsStart asserts the shell built-in actually starts in the
// image. It is the agent's only filesystem capability there, because the
// sandboxed tier that carries write_file is off in the baked config.
//
// The regression this exists for: s6-setuidgid changes uid and gid and nothing
// else, so the daemon ran as uid 1000 with HOME still pointing at root's home.
// os.UserCacheDir() reads $HOME, the plugin host tried to create /root/.cache,
// and shell failed to start — while the daemon stayed up and healthy and every
// other assertion in this file still passed. A plugin that never loads is
// invisible from the outside; only the roster shows it.
func TestBuiltinPluginsStart(t *testing.T) {
	id := startContainer(t)

	out, err := dockerRun(t, "exec", "-u", "nine", id, "nine", "plugins")
	if err != nil {
		t.Fatalf("nine plugins: %v", err)
	}
	if !strings.Contains(out, "shell") {
		logs, _ := dockerRun(t, "logs", id)
		t.Fatalf("shell is not in the plugin roster:\n%s\n\ncontainer logs:\n%s", out, logs)
	}

	// The daemon logs the failure and carries on, so a started-then-died plugin
	// looks the same from the roster alone.
	logs, _ := dockerRun(t, "logs", id)
	if strings.Contains(logs, "built-in plugin start failed") {
		t.Errorf("a built-in plugin failed to start:\n%s", logs)
	}
}

// TestWorkspaceAliasResolves covers the /work alias, which the init-perms
// oneshot creates as root before either service starts. It cannot be made by
// the services themselves: they run as uid 1000 and the link lands at the
// filesystem root.
//
// The sandboxed file tools address the workspace as /work and `shell` sees
// /data/workspace, so a path one prints has to resolve for the other. A broken
// link fails no service and shows up only as an agent that cannot find a file
// it just wrote.
func TestWorkspaceAliasResolves(t *testing.T) {
	vol := fmt.Sprintf("nine-test-work-%d", time.Now().UnixNano())
	t.Cleanup(func() { _, _ = dockerRun(t, "volume", "rm", "-f", vol) })

	// A named volume mounted over /data is the case that matters: it shadows
	// whatever the image put there, so the alias has to survive a boot where
	// /data arrives empty and root-owned.
	id := startContainer(t, "-v", vol+":/data")

	target, err := dockerRun(t, "exec", id, "readlink", "-f", "/work")
	if err != nil {
		listing, _ := dockerRun(t, "exec", id, "ls", "-la", "/")
		t.Fatalf("/work does not resolve: %v\n/ holds:\n%s", err, listing)
	}
	if target != "/data/workspace" {
		t.Errorf("/work resolves to %q, want /data/workspace", target)
	}

	// Same directory, both names, and writable as the service user — the point
	// of the alias rather than just its existence.
	if _, err := dockerRun(t, "exec", "-u", "1000", id, "sh", "-c",
		"touch /work/.probe && test -f /data/workspace/.probe && rm /work/.probe"); err != nil {
		t.Errorf("uid 1000 cannot write through /work to /data/workspace: %v", err)
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
