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
	"strings"
	"testing"
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

// TestReleaseWorkflowScansBeforePushing asserts the publish job depends on the
// verify job. A scan that runs after the push has already shipped the artifact
// it was supposed to gate.
func TestReleaseWorkflowScansBeforePushing(t *testing.T) {
	wf := readRepoFile(t, ".github/workflows/release-image.yml")

	if !strings.Contains(wf, "needs: verify") {
		t.Error("the publish job does not declare `needs: verify`, so it can push an unscanned image")
	}
	// exit-code 1 is what turns the scan from a report into a gate.
	if !strings.Contains(wf, "exit-code: 1") {
		t.Error("no failing Trivy gate in the release workflow")
	}
	if !strings.Contains(wf, "cosign sign") {
		t.Error("the release workflow does not sign the published image")
	}
	if !strings.Contains(wf, "sbom: true") {
		t.Error("the release workflow does not attach an SBOM")
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

	// [tools] enabled must be false. Match the key in the [tools] table only.
	toolsIdx := regexp.MustCompile(`(?m)^\s*\[tools\]`).FindStringIndex(cfg)
	if toolsIdx == nil {
		t.Fatal("docker/nine.toml has no [tools] section; sandboxed tools must be explicitly off")
	}
	rest := cfg[toolsIdx[1]:]
	if next := regexp.MustCompile(`(?m)^\s*\[`).FindStringIndex(rest); next != nil {
		rest = rest[:next[0]]
	}
	if !regexp.MustCompile(`(?m)^\s*enabled\s*=\s*false`).MatchString(rest) {
		t.Error("docker/nine.toml does not set [tools] enabled = false")
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
