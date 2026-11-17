# deploy/

Deployment assets for Protodex:

| Path                    | What                                              |
|-------------------------|---------------------------------------------------|
| `docker-compose.yaml`   | Local quickstart: protodex + MinIO + SQLite volume |
| `.env.example`          | Documented settings / dev defaults for the quickstart |
| `charts/<name>/`        | Helm chart (not yet; CI runs `helm lint` + `kubeconform` once a `Chart.yaml` exists) |

## Quickstart (docker compose)

```sh
cp deploy/.env.example deploy/.env      # optional; edit secrets before exposing anything
docker compose -f deploy/docker-compose.yaml up -d --build
curl -fsS http://localhost:8080/healthz  # -> ok
```

`--build` builds the image from your checkout. To run a published release
instead, set `PROTODEX_IMAGE=ghcr.io/marijnkoesen/protodex:<version>` in
`deploy/.env` and run `docker compose -f deploy/docker-compose.yaml up -d`
without `--build`.

The stack:

- **protodex** on `localhost:8080` (h2c/HTTP, Connect + gRPC). Read-only
  root filesystem, all capabilities dropped, `no-new-privileges`, uid 65532.
  The only writable paths are the `protodex-data` named volume at
  `/var/lib/protodex` (for the SQLite database) and a `/tmp` tmpfs.
- **minio** (S3 API on the compose network only; console on
  `127.0.0.1:9001`, log in with `MINIO_ROOT_USER` / `MINIO_ROOT_PASSWORD`).
- **bucket-init**: one-shot `mc` job that creates the private `protodex`
  bucket; protodex starts after it succeeds.

Tear down with `docker compose -f deploy/docker-compose.yaml down`
(add `-v` to delete the volumes and all data).

### What works today

Protodex is early. The quickstart is wired for the finished MVP, but only part
of it is functional on `main` right now:

| Setting / feature | Status |
|-------------------|--------|
| `PROTODEX_ADDR`, `PROTODEX_LOG_LEVEL`, `PROTODEX_LOG_FORMAT` (also `PROTODEX_MAX_REQUEST_BYTES`, `PROTODEX_SHUTDOWN_TIMEOUT`, `PROTODEX_CONFIG`) | **Functional** (`internal/config`) |
| `/healthz`, `/readyz` | **Functional** |
| Registry RPCs (`buf push` / `buf dep update` / `buf registry login`) | Not yet: every RPC returns `unimplemented` |
| `PROTODEX_DB_PATH` (SQLite on the `protodex-data` volume) | **Ignored** until the SQLite store lands ([#7](https://github.com/MarijnKoesen/protodex/issues/7)) |
| `PROTODEX_BLOB_BACKEND`, `PROTODEX_S3_*` (MinIO) | **Ignored** until the blob store lands ([#8](https://github.com/MarijnKoesen/protodex/issues/8)) |
| `PROTODEX_BOOTSTRAP_ADMIN_TOKEN` | **Ignored** until token auth lands |

The forward-looking variable names are provisional; the implementing issues
may rename them, and this file and the compose file will be updated with them.
Unknown `PROTODEX_*` variables are currently ignored, so the stack starts fine
today.

### Bootstrap admin token

`PROTODEX_BOOTSTRAP_ADMIN_TOKEN` defaults to `protodex-dev-admin-token` for
local use only. Generate a real one with `openssl rand -hex 32` and put it in
`deploy/.env` (git-ignored). Once auth is implemented the intended flow is:

```sh
buf registry login localhost:8080 --token-stdin <<<"$PROTODEX_BOOTSTRAP_ADMIN_TOKEN"
```

### MinIO images

MinIO stopped publishing new community container images in late 2025. The
compose file pins the last community releases from `quay.io/minio/*`; override
them with `MINIO_IMAGE` / `MC_IMAGE` (e.g. a mirror, or any S3-compatible
server that also works with `mc`).

## Container image

`ghcr.io/marijnkoesen/protodex` (built from the repository `Dockerfile`):

- static, pure-Go (`CGO_ENABLED=0`) binary on `gcr.io/distroless/static-debian13:nonroot`;
  no shell, runs as `65532:65532`; works with a read-only root filesystem.
- `linux/amd64` and `linux/arm64`. The Go stage runs on the build host and
  cross-compiles (`BUILDPLATFORM` → `TARGETARCH`), so no QEMU is needed:
  `docker buildx build --platform linux/amd64,linux/arm64 .`
- base images pinned by digest (bumped by dependabot); OCI labels
  (`org.opencontainers.image.*`) set, with `VERSION`, `REVISION`, `CREATED`
  build args.
- default command `serve`, listening on `:8080`.

PR CI builds the image (without pushing) and scans it with Trivy, failing on
HIGH/CRITICAL vulnerabilities that have a fix.

## Releases

Pushing a `v*` tag runs `.github/workflows/release.yml`:

1. Multi-arch image pushed to GHCR, tagged `vX.Y.Z`, `X.Y.Z`, `X.Y`, `X`
   (not for `v0.*`) and `sha-<commit>`; `latest` for non-prerelease tags.
2. SBOMs: BuildKit SBOM + SLSA provenance in the image index, and a syft SPDX
   SBOM attached as a cosign attestation.
3. Cosign keyless signature and a GitHub build-provenance attestation.
4. GitHub Release with generated changelog, cross-compiled binaries
   (linux/darwin/windows × amd64/arm64), `SHA256SUMS` (with provenance
   attestation) and a source SBOM.

Verify an image:

```sh
cosign verify ghcr.io/marijnkoesen/protodex:vX.Y.Z \
  --certificate-identity-regexp '^https://github.com/MarijnKoesen/protodex/.github/workflows/release.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
gh attestation verify oci://ghcr.io/marijnkoesen/protodex:vX.Y.Z -R MarijnKoesen/protodex
```

Verify a binary: `sha256sum -c SHA256SUMS --ignore-missing` and
`gh attestation verify protodex_X.Y.Z_linux_amd64.tar.gz -R MarijnKoesen/protodex`.
