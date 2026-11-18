# deploy/

Deployment assets for Protodex:

| Path                    | What                                              |
|-------------------------|---------------------------------------------------|
| `docker-compose.yaml`   | Local quickstart: protodex + SeaweedFS + SQLite volume |
| `.env.example`          | Documented settings / dev defaults for the quickstart |
| `helm/protodex/`        | Helm chart: SQLite on a PVC + Litestream to S3, optional bundled SeaweedFS ([README](helm/protodex/README.md)) |

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

- **protodex** on `localhost:8080` (bound to 127.0.0.1 only; edit the `ports` mapping in `docker-compose.yaml` to expose it) (h2c/HTTP, Connect + gRPC). Read-only
  root filesystem, all capabilities dropped, `no-new-privileges`, uid 65532.
  The only writable paths are the `protodex-data` named volume at
  `/var/lib/protodex` (for the SQLite database) and a `/tmp` tmpfs.
- **seaweedfs** (single-process SeaweedFS with its S3 gateway on port 8333,
  compose network only). The admin identity comes from `S3_ACCESS_KEY` /
  `S3_SECRET_KEY`; data lives in the `seaweedfs-data` volume.
- **bucket-init**: one-shot job (`weed shell`) that creates the `protodex`
  bucket; protodex starts after it succeeds.

Tear down with `docker compose -f deploy/docker-compose.yaml down`
(add `-v` to delete the volumes and all data).

### What works today

Protodex is early. The quickstart is wired for the finished MVP, but only part
of it is functional on `main` right now:

| Setting / feature | Status |
|-------------------|--------|
| `PROTODEX_RATE_LIMIT_IP_RATE`, `PROTODEX_RATE_LIMIT_TOKEN_RATE` (requests/s, `0` disables; config file `rate_limit.{ip,token}_{rate,burst}`; defaults 50/s per IP and 100/s per token), `PROTODEX_RATE_LIMIT_TRUST_FORWARDED_FOR`, `PROTODEX_MAX_PUSH_BYTES` (total bytes per push, default 64 MiB) | **Functional** |
| `PROTODEX_ADDR`, `PROTODEX_LOG_LEVEL`, `PROTODEX_LOG_FORMAT` (also `PROTODEX_MAX_REQUEST_BYTES`, `PROTODEX_SHUTDOWN_TIMEOUT`, `PROTODEX_CONFIG`) | **Functional** (`internal/config`) |
| `/healthz`, `/readyz` | **Functional** |
| Registry RPCs (`buf push` / `buf dep update` / `buf registry login`) | Not yet: every RPC returns `unimplemented` |
| `PROTODEX_DB_PATH` (SQLite on the `protodex-data` volume) | **Functional**: `serve` opens the database and holds its lock (#7) |
| `PROTODEX_BLOB_BACKEND`, `PROTODEX_S3_*` (SeaweedFS) | **Ignored**: `serve` stores blobs on the filesystem (`PROTODEX_BLOB_DIR`); S3 wiring is pending ([#8](https://github.com/MarijnKoesen/protodex/issues/8)) |
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

### SeaweedFS image

The compose file uses `chrislusf/seaweedfs:latest` (the same image CI runs the
S3 contract tests against). Pin a release tag for reproducible deployments by
setting `SEAWEEDFS_IMAGE` in `deploy/.env`.

## Kubernetes (Helm)

[`helm/protodex/`](helm/protodex/README.md) deploys Protodex in SQLite mode: one
replica (`strategy: Recreate`, so rollouts have a short downtime) writing
SQLite on a `ReadWriteOnce` block volume, a Litestream sidecar replicating it
to S3, and a restore-on-start init container for empty volumes. It includes
Ingress / Gateway API HTTPRoute, NetworkPolicy, an optional ServiceMonitor and
an optional bundled SeaweedFS (the same S3 server as the compose quickstart).
Releases publish it as a signed OCI artifact:

```sh
helm install protodex oci://ghcr.io/marijnkoesen/charts/protodex --version X.Y.Z -n protodex -f my-values.yaml
```

See the chart README for install, upgrades, the restore drill and why the
database must not live on NFS/EFS. [`e2e/`](e2e/README.md) tests the chart on kind (conformance,
restart, Litestream restore, upgrade): `make kind-e2e`.

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
4. Helm chart packaged with the tag version, pushed to
   `oci://ghcr.io/marijnkoesen/charts/protodex`, cosign-signed and with a
   build-provenance attestation.
5. GitHub Release with generated changelog, cross-compiled binaries
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
