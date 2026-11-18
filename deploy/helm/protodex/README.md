# protodex Helm chart

Runs [Protodex](https://github.com/marijnkoesen/protodex), a self-hostable
Protobuf schema registry for the `buf` CLI, on Kubernetes in **SQLite mode**
(the default and currently only mode):

```
            Ingress / Gateway API HTTPRoute (h2c to the backend)
                              │
                      Service :8080 (http, appProtocol kubernetes.io/h2c)
                              │
 ┌──────────── Deployment (replicas: 1, strategy: Recreate) ────────────┐
 │ init: bucket-init        (only with the bundled SeaweedFS)           │
 │ init: litestream-restore restore -if-db-not-exists -if-replica-exists│
 │ sidecar: litestream      replicate ──────────────► S3 bucket         │
 │ protodex                 SQLite (WAL) on /var/lib/protodex           │
 └───────────────────────── RWO PVC (block storage) ────────────────────┘
```

- One pod writes a SQLite database on a `ReadWriteOnce` PVC.
- [Litestream](https://litestream.io) streams the WAL to an S3 bucket
  continuously. When the pod starts on an **empty** volume (new PVC, lost node,
  disaster recovery) an init container restores the latest replica first.
- Optional bundled single-node SeaweedFS as the S3 endpoint for evaluation.

> **SeaweedFS, not MinIO.** Issue #19 originally asked for an optional bundled
> MinIO. The repository's compose quickstart (`deploy/docker-compose.yaml`)
> switched to SeaweedFS, so this chart bundles SeaweedFS too, configured the same
> way (single `weed server -s3` process, admin identity from the S3 credentials,
> bucket created by `weed shell`).

> **Status.** As of this chart version Protodex reads `PROTODEX_ADDR`,
> `PROTODEX_LOG_*`, `PROTODEX_MAX_REQUEST_BYTES`, `PROTODEX_SHUTDOWN_TIMEOUT`,
> `PROTODEX_DB_PATH` and the tracing settings. `PROTODEX_BLOB_BACKEND`,
> `PROTODEX_S3_*` and `PROTODEX_BOOTSTRAP_ADMIN_TOKEN` are wired ahead of the
> blob store (#8) and token auth, using the names from `deploy/docker-compose.yaml`;
> see [`deploy/README.md`](../../README.md#what-works-today). Litestream itself
> uses the S3 settings today.

## Requirements

- Kubernetes **1.29+** for the default native sidecar (`litestream.nativeSidecar: true`,
  an init container with `restartPolicy: Always`, so Litestream starts before
  Protodex and keeps running until Protodex has exited, flushing the final WAL
  frames). On older clusters set `litestream.nativeSidecar=false`.
- A StorageClass backed by **block storage** with `ReadWriteOnce` (EBS, GCE PD,
  Azure Disk, Ceph RBD, Longhorn, local-path, ...). See
  [Why not NFS/EFS](#why-not-nfs--efs--other-network-filesystems).
- An S3-compatible bucket (or `seaweedfs.enabled=true` for evaluation).
- Helm 3.8+ (OCI support).

## Install

The chart is published as a signed OCI artifact next to the image:

```sh
VERSION=X.Y.Z   # chart version == release tag without the "v"

# Optional: verify the cosign keyless signature first.
cosign verify ghcr.io/marijnkoesen/charts/protodex:$VERSION \
  --certificate-identity-regexp '^https://github.com/MarijnKoesen/protodex/.github/workflows/release.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com

kubectl create namespace protodex
# The chart is compatible with the "restricted" Pod Security Standard:
kubectl label namespace protodex pod-security.kubernetes.io/enforce=restricted

helm install protodex oci://ghcr.io/marijnkoesen/charts/protodex \
  --version "$VERSION" -n protodex -f my-values.yaml
```

From a checkout: `helm install protodex deploy/helm/protodex -n protodex -f my-values.yaml`
(appVersion `latest` there; set `image.tag`).

### Evaluation: bundled SeaweedFS

```sh
helm install protodex oci://ghcr.io/marijnkoesen/charts/protodex --version "$VERSION" -n protodex \
  --set seaweedfs.enabled=true \
  --set s3.accessKeyId=protodex \
  --set s3.secretAccessKey="$(openssl rand -hex 24)" \
  --set auth.bootstrapAdminToken="$(openssl rand -hex 32)"
kubectl -n protodex port-forward svc/protodex 8080:8080
curl -fsS http://localhost:8080/healthz   # -> ok
```

The bundled SeaweedFS is one pod with one PVC: the replica then lives in the
same cluster as the primary, so it does not protect against losing the cluster
or its storage. Use a managed bucket in production.

### Production: AWS S3 with IRSA / Pod Identity

```yaml
s3:
  bucket: my-protodex-backups
  region: eu-west-1
serviceAccount:
  annotations:
    eks.amazonaws.com/role-arn: arn:aws:iam::123456789012:role/protodex
existingSecret: protodex          # holds bootstrap-admin-token
persistence:
  storageClass: gp3
  size: 20Gi
ingress:
  enabled: true
  className: nginx
  hosts:
    - host: buf.example.com
      paths: [{ path: /, pathType: Prefix }]
  tls:
    - secretName: protodex-tls
      hosts: [buf.example.com]
```

With no static S3 credentials the containers fall back to the standard AWS
credential chain (IRSA, EKS Pod Identity, instance profile). The IAM policy
needs `s3:GetObject`, `s3:PutObject`, `s3:DeleteObject` and `s3:ListBucket` on
the bucket.

### Secrets

By default the chart creates a Secret from `s3.accessKeyId`,
`s3.secretAccessKey` and `auth.bootstrapAdminToken` (empty values are left
out). To manage secrets yourself (External Secrets, Sealed Secrets, SOPS, ...)
set `existingSecret` to a Secret with these keys (names configurable under
`secretKeys`):

| Key | Used as |
|-----|---------|
| `s3-access-key-id` | `PROTODEX_S3_ACCESS_KEY_ID`, `LITESTREAM_ACCESS_KEY_ID`, SeaweedFS admin identity |
| `s3-secret-access-key` | `PROTODEX_S3_SECRET_ACCESS_KEY`, `LITESTREAM_SECRET_ACCESS_KEY`, SeaweedFS admin identity |
| `bootstrap-admin-token` | `PROTODEX_BOOTSTRAP_ADMIN_TOKEN` |

```sh
kubectl -n protodex create secret generic protodex \
  --from-literal=bootstrap-admin-token="$(openssl rand -hex 32)"
```

All keys are optional (missing S3 keys mean ambient credentials) except when
the bundled SeaweedFS is enabled, which needs both S3 keys.

## Upgrades and downtime

```sh
helm upgrade protodex oci://ghcr.io/marijnkoesen/charts/protodex --version "$NEW" -n protodex -f my-values.yaml
```

SQLite allows exactly one writer, and Protodex takes an exclusive lock on the
database file. The Deployment therefore runs **one replica with
`strategy: Recreate`**: on every rollout (upgrade, config change, `kubectl
rollout restart`, node drain) the old pod is stopped (draining in-flight
requests for up to `config.shutdownTimeout`, and Litestream syncs the last
WAL frames) **before** the new pod starts. Expect a few seconds to a minute of
unavailability per rollout: pod termination, volume detach/attach (slower when
the pod moves to another node), image pull and startup. Schedule upgrades in a
maintenance window, and pre-pull images if this matters. A RollingUpdate would
not help: the new pod could not mount the RWO volume or take the database lock
while the old pod still runs. For the same reason the chart has no
PodDisruptionBudget (it would only block node drains) and `replicas` is not a
value. `buf` clients retry idempotent reads; pushes during the window fail and
need to be retried.

Changing the ConfigMap or chart-managed Secret triggers a rollout (checksum
annotations).

The PVC carries `helm.sh/resource-policy: keep`, so `helm uninstall` keeps the
database; delete the PVC explicitly to drop it.

## Why not NFS / EFS / other network filesystems

Do **not** put the SQLite volume on NFS, Amazon EFS, Azure Files, CephFS,
GlusterFS or any other shared/network filesystem (`ReadWriteMany`):

- SQLite relies on POSIX advisory locks (`fcntl`) and, in WAL mode, on a
  shared-memory index (`-shm` file mapped with `mmap`). Network filesystems
  implement locking unreliably or not at all and do not support shared memory
  across clients, which leads to **silent database corruption**
  ([sqlite.org/howtocorrupt.html](https://www.sqlite.org/howtocorrupt.html),
  [sqlite.org/wal.html](https://www.sqlite.org/wal.html)).
- Protodex's single-instance guard (`flock` on `protodex.db.lock`) is not
  reliable across NFS clients either, so two pods could both believe they are
  the only writer.
- Latency per `fsync` is much higher, which makes every write slow.

Use a block-storage `ReadWriteOnce` StorageClass. Durability beyond the
volume comes from Litestream, not from the filesystem.

## Restore drill

> On EKS Pod Identity with the default NetworkPolicy, add an egress rule for
> `169.254.170.23/32` TCP 80 via `networkPolicy.egress.extraRules`, otherwise
> credentials cannot be fetched and restore/replication fail.

Practise this regularly; a backup you have not restored is not a backup.

**1. Check replication is healthy.**

```sh
kubectl -n protodex logs deploy/protodex -c litestream --tail=50
# List the LTX files in the replica (most recent last):
kubectl -n protodex exec deploy/protodex -c litestream -- \
  litestream ltx -config /etc/litestream.yml /var/lib/protodex/protodex.db
```

With the ServiceMonitor enabled, alert on Litestream's metrics, e.g. any
increase of `litestream_sync_error_count` or
`litestream_replica_operation_errors_total`, or no increase of
`litestream_replica_operation_total{operation="PUT"}` while Protodex is
receiving writes.

**2. Non-destructive restore test** (restores the replica to a scratch file and
runs a full integrity check, without touching the live database):

```sh
DRILL=/tmp/drill-$(date +%s).db   # unique name: the Litestream image has no rm
kubectl -n protodex exec deploy/protodex -c litestream -- \
  litestream restore -config /etc/litestream.yml \
    -o "$DRILL" -integrity-check full /var/lib/protodex/protodex.db
```

The scratch file stays in the emptyDir until the next pod restart (the
Litestream image is scratch-based and contains no `rm`).

`/tmp` is an emptyDir limited by `tmpSizeLimit` (64Mi); raise it if the
database is larger.

**3. Full disaster-recovery rehearsal** (destroys the PVC; do it on a staging
release that replicates to its own bucket/path, never on a second release
pointing at production's replica path, which would create two writers):

```sh
NS=protodex-staging
kubectl -n $NS scale deploy/protodex --replicas=0          # stop the writer
kubectl -n $NS wait --for=delete pod -l app.kubernetes.io/component=server --timeout=120s
kubectl -n $NS delete pvc protodex                          # simulate volume loss
helm upgrade protodex oci://ghcr.io/marijnkoesen/charts/protodex --version "$VERSION" \
  -n $NS --reuse-values                                     # recreates PVC, replicas back to 1
kubectl -n $NS logs deploy/protodex -c litestream-restore   # shows the restore
kubectl -n $NS rollout status deploy/protodex
```

The `litestream-restore` init container only restores when the database file
is missing (`-if-db-not-exists`) and does nothing when the bucket holds no
replica yet (`-if-replica-exists`, first install). To restore to a point in
time, run the restore manually with `-timestamp 2026-01-02T15:04:05Z` into an
empty volume (scale to 0, mount the PVC in a one-off pod with the Litestream
image, run `litestream restore -config ... -timestamp ...`, then scale up).

## Exposing the service

Protodex serves Connect (HTTP/1.1 and HTTP/2), gRPC and gRPC-Web on one
cleartext port. `buf push` uploads whole modules, so proxies must allow large
bodies: keep their limit at least `config.maxRequestBytes` (64 MiB default).

**Ingress** (`ingress.enabled`). The default annotations target ingress-nginx:
`proxy-body-size: 64m` and 300 s read/send timeouts. `buf` speaks Connect, which
works over HTTP/1.1 to the backend. For native gRPC clients through
ingress-nginx add `nginx.ingress.kubernetes.io/backend-protocol: GRPC` (that
makes nginx use HTTP/2 to the backend for all requests). For other controllers
replace the annotations with their equivalents (Traefik: a `buffering`
middleware with `maxRequestBodyBytes`; HAProxy: `haproxy.org/...`).

**Gateway API** (`httpRoute.enabled`). Renders a `gateway.networking.k8s.io/v1`
HTTPRoute to the Service. The Service port declares
`appProtocol: kubernetes.io/h2c`, so conformant implementations proxy HTTP/2
(h2c) to Protodex and gRPC works end to end. Gateway API has no portable body
size field; set it with the implementation's policy, for example Envoy Gateway
`ClientTrafficPolicy`/`BackendTrafficPolicy` (`connection.bufferLimit`),
NGINX Gateway Fabric `ClientSettingsPolicy` (`body.maxSize: 64m`), or
annotations via `httpRoute.annotations`. `httpRoute.timeouts.request` sets the
per-request timeout.

## Security

Every container runs as UID/GID 65532 with `runAsNonRoot`, a read-only root
filesystem, `allowPrivilegeEscalation: false`, all capabilities dropped and the
`RuntimeDefault` seccomp profile. The only writable paths are the data volume
and an emptyDir at `/tmp`. The ServiceAccount token is not mounted. This
satisfies the `restricted` Pod Security Standard.

`networkPolicy.enabled` (default on) adds a NetworkPolicy that allows ingress
only to the HTTP and Litestream metrics ports (restrict the sources with
`networkPolicy.ingressFrom` / `metricsFrom`, e.g. your ingress controller's
namespace), and egress only to DNS and S3 (`networkPolicy.egress.s3To` /
`s3Ports`, default any address on 443; the bundled SeaweedFS pods when
enabled). Add rules for an OTLP collector etc. under
`networkPolicy.egress.extraRules`, or set `networkPolicy.egress.allowAll=true`.
`/metrics` is unauthenticated (see `docs/observability.md`); keep
`ingressFrom` tight or block `/metrics` at the edge.

## Monitoring

- Probes: startup and liveness on `GET /healthz`, readiness on `GET /readyz`
  (port 8080).
- `metrics.serviceMonitor.enabled=true` creates a Prometheus Operator
  ServiceMonitor scraping Protodex `/metrics` on the `http` port and, with
  `metrics.serviceMonitor.litestream`, Litestream's metrics on `ls-metrics`
  (`litestream.metrics.port`, 9090). Add `metrics.serviceMonitor.labels` to
  match your Prometheus `serviceMonitorSelector`.
- Grafana dashboard: `deploy/grafana/protodex.json`.

## Scheduling

| Value | Purpose |
|-------|---------|
| `resources`, `litestream.resources`, `seaweedfs.resources` | Requests/limits (defaults: protodex 100m/128Mi req, 512Mi limit; Litestream 10m/32Mi, 256Mi limit). |
| `priorityClassName` | Keep the registry from being preempted by batch work. |
| `nodeSelector`, `tolerations`, `affinity` | Pin to a node pool. |
| `topologySpreadConstraints` | With one replica mostly a zone preference for the first schedule. Once the PVC is bound, zonal block storage pins the pod to the volume's zone; use a StorageClass with `volumeBindingMode: WaitForFirstConsumer`. |
| `terminationGracePeriodSeconds` | Must exceed `config.shutdownTimeout` (default 45 s vs 30 s). |

## Values

The full list with defaults and comments is in [`values.yaml`](values.yaml);
[`values.schema.json`](values.schema.json) validates it (unknown top-level keys
are rejected, so typos fail fast). Most important keys:

| Key | Default | Description |
|-----|---------|-------------|
| `mode` | `sqlite` | Only `sqlite` is implemented. |
| `image.repository` / `tag` / `digest` | `ghcr.io/marijnkoesen/protodex` / appVersion / – | Protodex image. |
| `config.logLevel`, `config.logFormat` | `info`, `json` | Logging. |
| `config.maxRequestBytes` | `67108864` | Request body limit. |
| `config.shutdownTimeout` | `30s` | Graceful shutdown. |
| `config.tracing.enabled`, `.otlpEndpoint` | `false`, `""` | OpenTelemetry tracing. |
| `config.dbPath` | `/var/lib/protodex/protodex.db` | Database file; its directory is the volume mount. |
| `s3.endpoint` / `region` / `bucket` / `forcePathStyle` | `""` / `us-east-1` / `protodex` / `false` | S3 target (empty endpoint = AWS). |
| `s3.accessKeyId`, `s3.secretAccessKey` | `""` | Static credentials (else ambient). |
| `auth.bootstrapAdminToken` | `""` | Bootstrap admin token. |
| `existingSecret`, `secretKeys.*` | `""` | Use your own Secret. |
| `persistence.enabled` / `existingClaim` / `storageClass` / `size` | `true` / `""` / `""` / `10Gi` | RWO data volume. |
| `litestream.enabled` | `true` | Replicate to S3 and restore on empty volume. |
| `litestream.path` | `litestream/protodex.db` | Replica key prefix in the bucket. |
| `litestream.nativeSidecar` | `true` | Native sidecar (k8s 1.29+). |
| `litestream.restore.enabled` | `true` | Restore init container. |
| `seaweedfs.enabled` | `false` | Bundled single-node SeaweedFS. |
| `ingress.*` | disabled | Ingress with large-body annotations. |
| `httpRoute.*` | disabled | Gateway API HTTPRoute. |
| `metrics.serviceMonitor.enabled` | `false` | Prometheus Operator ServiceMonitor. |
| `networkPolicy.enabled` | `true` | Ingress/egress restrictions. |

## Development

```sh
helm lint --strict deploy/helm/protodex
for f in "" deploy/helm/protodex/ci/*-values.yaml; do
  helm template ci deploy/helm/protodex ${f:+-f "$f"} | kubeconform -strict -summary \
    -schema-location default \
    -schema-location 'https://raw.githubusercontent.com/datreeio/CRDs-catalog/main/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json'
done
```

CI (`helm` job in `.github/workflows/ci.yml`) runs the same checks.
`make kind-e2e` installs the chart into a kind cluster and runs the buf CLI
conformance suite, a pod-kill restart test, the restore drill below
(PVC deleted, Litestream restore from the bundled SeaweedFS) and an upgrade
from the previous version against it; see
[`deploy/e2e/README.md`](../../e2e/README.md) (CI job `kind-e2e`). The
release workflow packages the chart with the tag's version, pushes it to
`oci://ghcr.io/marijnkoesen/charts` and signs it with cosign.
