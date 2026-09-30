# Container image

Nine publishes a runtime image to GHCR. Pulling it is the supported way to run
Nine without a source checkout.

The package is public, so a pull needs no credentials and no `docker login`:

```bash
docker run -d --name nine \
  -p 127.0.0.1:8080:8080 \
  --add-host host.docker.internal:host-gateway \
  -v nine-data:/data \
  ghcr.io/djordlucas/nine:latest

docker exec -it -u nine nine nine    # interactive TUI
```

The image ships a working `/nine.toml`, so nothing needs mounting for a first
run. It expects an Ollama on the host at port 11434; point it elsewhere with
`-e NINE_LLM_ENDPOINT=...`.

## Registries

| Registry | Reference | Status |
|----------|-----------|--------|
| GitHub Container Registry | `ghcr.io/djordlucas/nine` | Published, public |
| Docker Hub | `docker.io/djordlucas/nine` | Not published |

GHCR is the only registry in use. The release workflow can also push to Docker
Hub, and does so only when `DOCKERHUB_USERNAME` and `DOCKERHUB_TOKEN` are set —
setting them is the opt-in, and it means creating the Docker Hub repository
first, with the visibility you want, since a push to one that does not exist
creates it public.

The GHCR package is public, so anyone can pull it and no invitation is needed.
Write access follows the repository; another repository's workflow is granted
pull under the package's *Manage Actions access*.

## Tags

| Tag | Moves | Use |
|-----|-------|-----|
| `1.4.2` | Never | Production. An exact release. |
| `1.4` | On each patch release | Patch updates without a pin bump |
| `1` | On each minor release | Only past 1.0, and never for a `v0.x` tag |
| `latest` | On each stable release | Trying Nine out |
| `sha-<commit>` | Never | Tracing an image back to a commit |

`latest` skips prereleases: a tag containing a hyphen (`v1.5.0-rc1`) publishes
its version tags but does not move `latest`.

Pinning by digest is stronger than any tag, and the release summary prints the
digest:

```bash
docker pull ghcr.io/djordlucas/nine@sha256:<digest>
```

## Platforms

`linux/amd64` and `linux/arm64`, as one manifest list. `docker pull` selects the
right one. arm64 covers Apple Silicon and Graviton.

## What runs inside

| | |
|---|---|
| Init | s6-overlay, as root — it reaps orphaned children and forwards `docker stop`'s SIGTERM |
| Daemon | `nine daemon`, as uid 1000 (`nine`) |
| API | `nine api serve --host 0.0.0.0 --port 8080`, as uid 1000 |
| State | `/data` — the SQLite database and the workspace |
| Health | `nine status` every 30s, after a 20s start period, matching on the uptime line — the command exits 0 even with no daemon reachable |

The daemon and the API run unprivileged. Everything the agent reaches through
the `shell` plugin inherits uid 1000, so it cannot write outside `/data` or
install packages inside the container.

`docker exec` lands as root unless you pass `-u nine`. Use `-u nine` for
anything that touches `/data`, or a root-owned file will appear in it.

## Configuration

Three ways, in increasing order of control:

1. **Environment variables.** `NINE_LLM_PROVIDER`, `NINE_LLM_MODEL`,
   `NINE_LLM_ENDPOINT` cover the common case without a file.
2. **Mount your own config.** `-v ./my-nine.toml:/nine.toml:ro` shadows the
   baked one entirely.
3. **Derive an image.** `FROM ghcr.io/djordlucas/nine:1.4.2`, then add an MCP
   server's runtime or your own `tools.d`.

The baked config is narrower than the repo's `nine.toml`, which is a development
config. It is not narrower on the tool tiers: both are on, because an agent that
cannot read or write a file is not a working default, and because what bounds the
generated tier is its capability ceiling rather than its switch.

| Setting | Baked value | Why |
|---------|-------------|-----|
| `[tools] enabled` | `true` | The shipped sandboxed tools are the agent's filesystem — `read_file`, `write_file`, `edit_file` and the rest. Without them it cannot open a file. They are embedded in the binary, so nothing is mounted to get them. |
| `[tools] user_dir` | unset | Only the shipped, reviewed tools load. A directory of tools is not read. |
| `[tools.agent] enabled` | `true` | Nine writes its own tools, bounded by a ceiling that defaults to the workspace — the same directory the shipped tools reach and `shell` runs in. A generated tool is narrower than the `shell` this image also ships: wasm, one instance per call, no network, no environment. |
| `[tools.agent.deps]`, `allow_network_deps`, `allow_long_running`, `allow_standing` | off | Every switch that widens a generated tool beyond the workspace stays an operator's decision. `net.http` is likewise ungranted in the ceiling; `web_search`, `web_page_read` and `http_get` are shipped tools, so the agent reaches the web without one. |
| `[daemon] self_reflection` | `30m` | Every tick is an LLM call. At the 2m default an idle container bills a metered API around the clock. `off` removes it. |
| `[memory] path` | `/data/nine.db` | The container's writable layer is discarded when the container is replaced. |
| `[workspace] root` | `/data/workspace` | Scopes the workspace to the volume. |
| `[embeddings] provider` | `keyword` | Ranking works with no model and no network. |

Full reference: [configuration.md](configuration.md).

## Verifying an image

Every published image is signed with cosign, keylessly. The signature is bound
to the release workflow's OIDC identity and recorded in Rekor; there is no
public key to distribute and no private key to leak.

```bash
cosign verify ghcr.io/djordlucas/nine:latest \
  --certificate-identity-regexp '^https://github.com/djordlucas/nine/.github/workflows/release-image.yml@' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

The identity regexp is the check that matters. Without it, cosign accepts a
signature from any workflow in any repository.

Build provenance and the SBOM ride along on the manifest:

```bash
# What is inside it
docker buildx imagetools inspect ghcr.io/djordlucas/nine:latest \
  --format '{{ json .SBOM.SPDX }}'

# How it was built
docker buildx imagetools inspect ghcr.io/djordlucas/nine:latest \
  --format '{{ json .Provenance.SLSA }}'
```

`gh attestation verify` works against a published image. GitHub's attestation API
is limited to public repositories outside GitHub Enterprise Cloud, and this
repository is public, so the release workflow attests the pushed digest:

```bash
gh attestation verify oci://ghcr.io/djordlucas/nine:latest --repo djordlucas/nine
```

That is a third, independent check alongside the cosign signature and the buildx
attestations above — those two live on the image, this one lives in GitHub.

`make image-verify` runs the cosign command above.

## How an image is built

A release is a `v*` git tag. The workflow
(`.github/workflows/release-image.yml`) runs two jobs, and the order is the
point:

1. **verify** — build each published architecture, scan it with Trivy (a
   fixable CRITICAL or HIGH fails the job), and run the image contract tests
   from `tests/docker/`. A test asserts that every platform the publish job
   pushes is one the matrix scans.
2. **publish** — only if verify passed: build the multi-arch manifest from
   cache, push, sign, attest.

Nothing is pushed from the verify job, so an image that fails the scan or the
tests is never published.

Supply-chain properties:

| Property | How |
|----------|-----|
| Base images are immutable | Pinned by manifest-list digest, bumped by Dependabot |
| s6-overlay is verified | SHA-256 checked against `docker/s6-overlay.sha256` before unpacking — a tarball unpacks as root into `/` |
| Actions cannot be swapped | Every action pinned to a 40-character commit SHA, asserted by a test |
| No build secrets in layers | Build args carry only version, commit and date; a test checks the image environment for credential-shaped values |
| Reproducible binary | `CGO_ENABLED=0`, `-trimpath`, version injected by ldflags |

## Limits

| Limit | Detail |
|-------|--------|
| API auth is off unless you set it | The image runs `nine api serve --host 0.0.0.0`, so port 8080 answers anyone who reaches it until a token is configured. Set `NINE_API_AUTH_TOKEN` (or `[api] auth_token`), or publish to loopback only (`-p 127.0.0.1:8080:8080`). Nine logs a warning at startup in this state. The daemon's own socket is local-only. |
| Container isolation is the boundary | Only sandboxed tools run behind a capability boundary. The `shell` plugin runs commands as uid 1000 with that user's full reach inside the container. Do not point it at anything you do not trust. |
| uid 1000 is fixed | A bind-mounted `/data` owned by another uid is chowned at boot when the volume root is not already 1000. A read-only mount logs a warning and the daemon cannot write. |
| No package installs at runtime | Running unprivileged means the agent cannot `apt-get install`. Derive an image instead. |
| No browser, no Node | An MCP server needing either must come from a derived image or a hosted URL — [browser.md](browser.md). |
| arm64 is emulated at build time | The Go binary cross-compiles, but the Debian layers build under QEMU, so arm64 releases are slower to produce. The image itself is native. |
| Contract tests run on amd64 only | Both architectures are scanned, but the container tests drive real containers, and running them under QEMU would add emulation flakiness to a release gate. |
| `docker stop` exits 137 | s6-linux-init runs its shutdown in container mode and ends by SIGKILLing what remains, PID 1 included, so a clean stop still reports 137. Every service stops in dependency order first — check the logs, not the status. Orchestrators that read the exit code see a crash where there was none. |
| `latest` is a moving target | It changes on every stable release. Pin a version or a digest for anything that matters. |
| Signing is publicly logged | Keyless cosign records the repository name, workflow path and image digest in the public Rekor log. |
