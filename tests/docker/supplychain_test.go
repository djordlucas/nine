// Package docker_test asserts the contract of the published container image.
//
// It has two halves. This file holds the static checks — they read files in the
// repo, need no Docker, and run in the ordinary `go test ./...` gate, so a
// regression in the supply chain fails CI on the pull request that introduces
// it rather than at release time. image_test.go holds the checks that need a
// built image and are opt-in.
package docker_test

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// repoRoot is this package's path relative to the repository root.
const repoRoot = "../.."

func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

// TestBaseImagesPinnedByDigest asserts every FROM in the Dockerfile names a
// digest. A tag is mutable, so a tag-only base means rebuilding an old commit
// can produce an image that commit was never tested against, and it silently
// widens what the build trusts.
func TestBaseImagesPinnedByDigest(t *testing.T) {
	dockerfile := readRepoFile(t, "Dockerfile")

	fromLine := regexp.MustCompile(`(?m)^FROM\s+(\S+)`)
	matches := fromLine.FindAllStringSubmatch(dockerfile, -1)
	if len(matches) == 0 {
		t.Fatal("no FROM lines found in Dockerfile")
	}

	// Stage names declared by an earlier `AS <name>`; a FROM referring to one is
	// an internal reference, not a registry pull.
	stages := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?mi)^FROM\s+\S+\s+AS\s+(\S+)`).FindAllStringSubmatch(dockerfile, -1) {
		stages[strings.ToLower(m[1])] = true
	}

	for _, m := range matches {
		ref := m[1]
		if stages[strings.ToLower(ref)] {
			continue
		}
		if !strings.Contains(ref, "@sha256:") {
			t.Errorf("base image %q is not pinned by digest; use image:tag@sha256:...", ref)
		}
	}
}

// TestS6ChecksumsCoverDeclaredVersion asserts the checksum file and the
// Dockerfile's S6_OVERLAY_VERSION agree. Bumping the version without replacing
// the checksums would leave the build verifying the new tarballs against the
// old hashes, which fails loudly — but bumping the checksums without the
// version, or vice versa, is the quiet half this catches.
func TestS6ChecksumsCoverDeclaredVersion(t *testing.T) {
	dockerfile := readRepoFile(t, "Dockerfile")
	sums := readRepoFile(t, "docker/s6-overlay.sha256")

	m := regexp.MustCompile(`ARG S6_OVERLAY_VERSION=(\S+)`).FindStringSubmatch(dockerfile)
	if m == nil {
		t.Fatal("S6_OVERLAY_VERSION not found in Dockerfile")
	}
	version := m[1]

	if !strings.Contains(sums, "s6-overlay version: "+version) {
		t.Errorf("docker/s6-overlay.sha256 does not declare version %s; "+
			"bumping S6_OVERLAY_VERSION means replacing every checksum in that file", version)
	}

	// The Dockerfile maps TARGETARCH to an s6 arch name and then greps for
	// exactly two lines. Every arch it can select needs an entry, or a build for
	// that platform fails at the grep.
	for _, arch := range []string{"noarch", "x86_64", "aarch64", "arm", "i686", "powerpc64le", "s390x", "riscv64"} {
		want := "  s6-overlay-" + arch + ".tar.xz"
		if !strings.Contains(sums, want) {
			t.Errorf("no checksum for s6-overlay-%s.tar.xz", arch)
		}
	}
}

// TestS6TarballsAreVerified asserts the fetch stage runs sha256sum -c before
// unpacking. An s6-overlay tarball unpacks as root into /, so an unverified one
// is arbitrary code execution in the build with whatever the release URL served.
func TestS6TarballsAreVerified(t *testing.T) {
	dockerfile := readRepoFile(t, "Dockerfile")

	verifyIdx := strings.Index(dockerfile, "sha256sum -c")
	if verifyIdx < 0 {
		t.Fatal("Dockerfile does not verify the s6-overlay tarballs with sha256sum -c")
	}
	unpackIdx := strings.Index(dockerfile, "tar -C /out")
	if unpackIdx < 0 {
		t.Fatal("expected the s6-overlay unpack step in the Dockerfile")
	}
	if verifyIdx > unpackIdx {
		t.Error("s6-overlay tarballs are unpacked before they are verified")
	}
}

// TestWorkflowActionsPinnedBySHA asserts every third-party action is pinned to a
// full commit SHA. A version tag can be moved by whoever owns the action, so a
// tag pin trusts that they will not; a SHA pin does not require trusting it.
func TestWorkflowActionsPinnedBySHA(t *testing.T) {
	dir := filepath.Join(repoRoot, ".github", "workflows")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read workflows dir: %v", err)
	}

	usesLine := regexp.MustCompile(`(?m)^\s*-?\s*uses:\s*(\S+)`)
	sha40 := regexp.MustCompile(`^[0-9a-f]{40}$`)

	checked := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || (!strings.HasSuffix(name, ".yml") && !strings.HasSuffix(name, ".yaml")) {
			continue
		}
		body := readRepoFile(t, filepath.Join(".github", "workflows", name))
		for _, m := range usesLine.FindAllStringSubmatch(body, -1) {
			ref := strings.Trim(m[1], `"'`)
			// A local composite action lives in this repo and moves with it.
			if strings.HasPrefix(ref, "./") || strings.HasPrefix(ref, "docker://") {
				continue
			}
			at := strings.LastIndex(ref, "@")
			if at < 0 {
				t.Errorf("%s: action %q has no version pin at all", name, ref)
				continue
			}
			checked++
			if pin := ref[at+1:]; !sha40.MatchString(pin) {
				t.Errorf("%s: action %q is pinned to %q, not a 40-character commit SHA",
					name, ref[:at], pin)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no third-party actions found to check; the regex or the layout changed")
	}
}

// releaseWorkflow is the part of the release workflow these tests reason about.
type releaseWorkflow struct {
	Jobs map[string]struct {
		Needs    any `yaml:"needs"` // a string or a list, per Actions
		Strategy struct {
			Matrix struct {
				Platform []string `yaml:"platform"`
			} `yaml:"matrix"`
		} `yaml:"strategy"`
		Steps []map[string]any `yaml:"steps"`
	} `yaml:"jobs"`
}

func loadReleaseWorkflow(t *testing.T) releaseWorkflow {
	t.Helper()
	var wf releaseWorkflow
	if err := yaml.Unmarshal([]byte(readRepoFile(t, ".github/workflows/release-image.yml")), &wf); err != nil {
		t.Fatalf("parse release-image.yml: %v", err)
	}
	return wf
}

// needsOf normalises the `needs` key, which Actions accepts as either a single
// job name or a list of them.
func needsOf(v any) []string {
	switch n := v.(type) {
	case string:
		return []string{n}
	case []any:
		out := make([]string, 0, len(n))
		for _, e := range n {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// TestPublishDependsOnVerify asserts the publish job cannot run unless the
// scanning job passed. A scan that runs after the push has already shipped the
// artifact it was supposed to gate.
func TestPublishDependsOnVerify(t *testing.T) {
	wf := loadReleaseWorkflow(t)

	publish, ok := wf.Jobs["publish"]
	if !ok {
		t.Fatal("no publish job in release-image.yml")
	}
	needs := needsOf(publish.Needs)
	if !slices.Contains(needs, "verify") {
		t.Errorf("publish needs %v, which does not include verify, so it can push an unscanned image", needs)
	}
}

// TestEveryPublishedPlatformIsScanned asserts the gate covers each architecture
// that gets pushed. The Debian layers differ per arch, so scanning one and
// publishing two leaves the other ungated.
func TestEveryPublishedPlatformIsScanned(t *testing.T) {
	wf := loadReleaseWorkflow(t)

	scanned := wf.Jobs["verify"].Strategy.Matrix.Platform
	if len(scanned) == 0 {
		t.Fatal("the verify job declares no platform matrix")
	}

	// The platforms the publish job actually pushes. Only the build step counts:
	// the QEMU setup step carries a `platforms` key too, naming what it emulates.
	var pushed []string
	for _, step := range wf.Jobs["publish"].Steps {
		uses, _ := step["uses"].(string)
		if !strings.Contains(uses, "docker/build-push-action") {
			continue
		}
		with, ok := step["with"].(map[string]any)
		if !ok {
			continue
		}
		if plat, ok := with["platforms"].(string); ok {
			for _, p := range strings.Split(plat, ",") {
				pushed = append(pushed, strings.TrimSpace(p))
			}
		}
	}
	if len(pushed) == 0 {
		t.Fatal("no platforms found on the publish job's build step")
	}

	for _, p := range pushed {
		if !slices.Contains(scanned, p) {
			t.Errorf("platform %s is published but not scanned (verify scans %v)", p, scanned)
		}
	}
}

// TestReleaseGateIsAGate asserts the release pipeline keeps the properties that
// make a published image trustworthy: a failing scan, a signature, an SBOM.
func TestReleaseGateIsAGate(t *testing.T) {
	wf := readRepoFile(t, ".github/workflows/release-image.yml")

	for _, check := range []struct{ needle, why string }{
		{"exit-code: 1", "the Trivy scan does not fail the job, so it reports rather than gates"},
		{"cosign sign", "the release does not sign the published image"},
		{"sbom: true", "the release does not attach an SBOM"},
		{"provenance: mode=max", "the release does not attach build provenance"},
	} {
		if !strings.Contains(wf, check.needle) {
			t.Error(check.why)
		}
	}
}

// TestPublishedConfigIsConservative asserts the config baked into the image
// does not enable the tiers that let the agent run code it wrote or reach the
// host filesystem. A published default reaches people who never read it.
func TestPublishedConfigIsConservative(t *testing.T) {
	cfg := readRepoFile(t, "docker/nine.toml")

	if regexp.MustCompile(`(?m)^\s*\[tools\.agent\]`).MatchString(cfg) {
		t.Error("docker/nine.toml declares [tools.agent]; the generated-tool tier must stay off in the published image")
	}

	// The shipped sandboxed tools are on — they are the agent's filesystem, and
	// without them the image can neither read nor write a file. What must stay
	// off is user_dir: the shipped set is embedded in the binary and reviewed,
	// a directory of tools is neither. Match keys in the [tools] table only.
	toolsIdx := regexp.MustCompile(`(?m)^\s*\[tools\]`).FindStringIndex(cfg)
	if toolsIdx == nil {
		t.Fatal("docker/nine.toml has no [tools] section; the tier must be set explicitly")
	}
	rest := cfg[toolsIdx[1]:]
	if next := regexp.MustCompile(`(?m)^\s*\[`).FindStringIndex(rest); next != nil {
		rest = rest[:next[0]]
	}
	if !regexp.MustCompile(`(?m)^\s*enabled\s*=\s*true`).MatchString(rest) {
		t.Error("docker/nine.toml does not set [tools] enabled = true; the image would ship with no file tools")
	}
	if regexp.MustCompile(`(?m)^\s*user_dir\s*=`).MatchString(rest) {
		t.Error("docker/nine.toml sets [tools] user_dir; the published image loads only the shipped tools")
	}

	// Durable state belongs on the volume, not in the image's writable layer,
	// which is discarded when the container is replaced.
	for _, want := range []string{`path = "/data/nine.db"`, `root = "/data/workspace"`} {
		if !strings.Contains(cfg, want) {
			t.Errorf("docker/nine.toml is missing %s; state must live on the /data volume", want)
		}
	}
}

// TestRuntimeServicesDropPrivileges asserts the daemon and API run scripts hand
// off to an unprivileged user. s6 itself stays root to supervise and reap; the
// services must not.
func TestRuntimeServicesDropPrivileges(t *testing.T) {
	for _, svc := range []string{"nine", "api"} {
		run := readRepoFile(t, filepath.Join("docker/s6/runtime/s6-rc.d", svc, "run"))
		if !strings.Contains(run, "s6-setuidgid nine") {
			t.Errorf("runtime service %q does not drop privileges with s6-setuidgid", svc)
		}
	}

	dockerfile := readRepoFile(t, "Dockerfile")
	runtimeIdx := strings.Index(dockerfile, "AS runtime")
	if runtimeIdx < 0 {
		t.Fatal("no runtime stage in the Dockerfile")
	}
	runtimeStage := dockerfile[runtimeIdx:]
	if !strings.Contains(runtimeStage, "useradd") || !strings.Contains(runtimeStage, "--uid 1000") {
		t.Error("the runtime stage does not create the uid 1000 nine user the services drop to")
	}
}

// TestHealthcheckDetectsADeadDaemon guards a subtle failure. `nine status`
// prints "no daemon running" and exits 0 when it cannot reach a daemon, because
// the query itself succeeded. A HEALTHCHECK that only ran that command would
// report every container healthy, including one whose daemon never started, so
// the check has to inspect the output.
func TestHealthcheckDetectsADeadDaemon(t *testing.T) {
	dockerfile := readRepoFile(t, "Dockerfile")

	idx := strings.Index(dockerfile, "HEALTHCHECK")
	if idx < 0 {
		t.Fatal("the runtime image declares no HEALTHCHECK")
	}
	// The directive plus its continuation lines.
	block := dockerfile[idx:]
	if end := strings.Index(block, "\nENTRYPOINT"); end > 0 {
		block = block[:end]
	}

	if !strings.Contains(block, "nine status") {
		t.Error("the healthcheck does not probe the daemon with `nine status`")
	}
	if !strings.Contains(block, "Uptime:") {
		t.Error("the healthcheck does not inspect the status output; " +
			"`nine status` exits 0 with no daemon running, so a bare invocation " +
			"reports a dead container healthy")
	}
}
