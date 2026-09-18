# Container image

Nine publishes a runtime image to two registries. Pulling one is the supported
way to run Nine without a source checkout.

```bash
docker run -d --name nine \
  -p 8080:8080 \
  --add-host host.docker.internal:host-gateway \
  -v nine-data:/data \
  ghcr.io/djordlucas/nine:latest

docker exec -it -u nine nine nine    # interactive TUI
```

The image ships a working `/nine.toml`, so nothing needs mounting for a first
run. It expects an Ollama on the host at port 11434; point it elsewhere with
`-e NINE_LLM_ENDPOINT=...`.

## Registries

| Registry | Reference |
|----------|-----------|
| GitHub Container Registry | `ghcr.io/djordlucas/nine` |
| Docker Hub | `docker.io/djordlucas/nine` |

Both carry the same digest for a given release. GHCR is the primary: it is
pushed first and is where the build provenance attestation lives.

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
| Health | `nine status` every 30s, after a 20s start period |

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

The baked config is deliberately narrower than the repo's `nine.toml`, which is
a development config:

| Setting | Baked value | Why |
|---------|-------------|-----|
| `[tools] enabled` | `false` | The sandboxed-tool tier is opt-in. No environment variable turns it on. |
| `[tools.agent]` | absent | Nine does not write its own tools unless an operator asks for it. |
| `[memory] path` | `/data/nine.db` | The container's writable layer is discarded when the container is replaced. |
| `[workspace] root` | `/data/workspace` | Scopes the files plugin to the volume. |
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

Build provenance and the SBOM ride along as attestations:

```bash
# Who built it, from which commit, with which workflow
gh attestation verify oci://ghcr.io/djordlucas/nine:latest --repo djordlucas/nine

# What is inside it
docker buildx imagetools inspect ghcr.io/djordlucas/nine:latest \
  --format '{{ json .SBOM.SPDX }}'
```

`make image-verify` runs the cosign command above.

## How an image is built

A release is a `v*` git tag. The workflow
(`.github/workflows/release-image.yml`) runs two jobs, and the order is the
point:

1. **verify** — build `linux/amd64`, scan it with Trivy (fixable CRITICAL or
   HIGH fails the job), run the image contract tests from `tests/docker/`.
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
| No authentication on the API | Port 8080 speaks to anyone who reaches it. Bind it to localhost (`-p 127.0.0.1:8080:8080`) or put it behind a reverse proxy. The daemon's own socket is local-only. |
| Container isolation is the boundary | Only sandboxed tools run behind a capability boundary. The `shell` plugin runs commands as uid 1000 with that user's full reach inside the container. Do not point it at anything you do not trust. |
| uid 1000 is fixed | A bind-mounted `/data` owned by another uid is chowned at boot when the volume root is not already 1000. A read-only mount logs a warning and the daemon cannot write. |
| No package installs at runtime | Running unprivileged means the agent cannot `apt-get install`. Derive an image instead. |
| No browser, no Node | An MCP server needing either must come from a derived image or a hosted URL — [browser.md](browser.md). |
| arm64 is emulated at build time | The Go binary cross-compiles, but the Debian layers build under QEMU, so arm64 releases are slower to produce. The image itself is native. |
| `latest` is a moving target | It changes on every stable release. Pin a version or a digest for anything that matters. |
| Docker Hub pulls are rate-limited | Anonymous pulls hit Docker's limits. GHCR does not apply them. |
