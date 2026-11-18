# Helm chart e2e on kind

[`e2e.sh`](e2e.sh) installs the [Helm chart](../helm/protodex/README.md) into
a throwaway [kind](https://kind.sigs.k8s.io) cluster and tests the deployment
the way an operator runs it:

| Step | What it checks |
|------|----------------|
| `up` | Image build + `kind load`, `helm install --wait` with the bundled SeaweedFS ([`values.yaml`](values.yaml)) in a namespace enforcing the `restricted` Pod Security Standard, then `protodex bootstrap -org acme` |
| `upgrade` | Installs the **previous version** first (see below), pushes data, `helm upgrade`s to this checkout and runs `protodex migrate up`, then verifies the data and that the CLI keeps working |
| `conformance` | The [buf CLI conformance suite](../../docs/compatibility.md) (PR subset of buf versions) against the release, through its Service |
| `restart` | Pushes modules, commits and labels, kills the server pod (1s grace period), waits for the replacement and verifies nothing was lost, that the same CLI configuration recovers and that pushes work again |
| `restore` | Pushes data, stops the writer, **deletes the PVC**, lets Helm recreate it and the `litestream-restore` init container restore the database from SeaweedFS (S3), then verifies modules, commits and labels |

The lifecycle steps are one Go test, `TestDisruption` in
[`test/conformance/remote_test.go`](../../test/conformance/remote_test.go):
it seeds data, runs `e2e.sh kill-pod|wipe-volume|upgrade-release`, polls until
the server answers again (failing at once if it answers but lost the data),
compares modules, label targets, commit ids and digests through the API, and
lists/exports/builds with the buf CLI. Every wait is a `kubectl rollout
status`/`helm --wait` or a poll with a deadline (`E2E_TIMEOUT`, default 300s),
never a fixed sleep.

## Running it

Needs Docker, [kind](https://kind.sigs.k8s.io/docs/user/quick-start/#installation),
`kubectl`, `helm` (v3), Go and network access (images, buf releases). Run it
on Linux, like the conformance suite (see
[docs/compatibility.md](../../docs/compatibility.md#running-it): older buf
releases on macOS ignore `SSL_CERT_FILE`, which the test's TLS proxy needs).

```sh
make kind-e2e                         # everything, on a fresh cluster, then delete it
E2E_KEEP=1 make kind-e2e              # keep the cluster afterwards (or on failure)
make kind-e2e E2E_ARGS=up             # just a cluster with the release
make kind-e2e E2E_ARGS=conformance    # ...then single steps against it
make kind-e2e E2E_ARGS=restart
make kind-e2e E2E_ARGS=diagnostics    # pods, events, logs
make kind-e2e-down
```

The script uses its own kubeconfig (`.e2e-kind/kubeconfig`), so it never
touches your current kubectl context:
`export KUBECONFIG=$PWD/.e2e-kind/kubeconfig` to look around. The release is
reachable on `http://127.0.0.1:30080` through a NodePort Service the script
adds (kind maps the node port to localhost; it does not depend on an ingress
controller and survives pod restarts). The buf CLI needs TLS, so the test
puts a local HTTPS proxy with a test certificate in front of it. The admin
token is in `.e2e-kind/token`.

Settings (environment): `E2E_UPGRADE_FROM` (see below), `E2E_BUF_VERSIONS`
(`pr`, `all` or a list), `E2E_PORT` (host port, 30080), `E2E_CLUSTER`,
`E2E_NAMESPACE`, `E2E_RELEASE`, `E2E_KIND_IMAGE` (node image), `E2E_TIMEOUT`,
`E2E_STATE_DIR`. See the header of [`e2e.sh`](e2e.sh).

### Previous version for the upgrade test

`E2E_UPGRADE_FROM` is a git ref whose chart and image (built from that
commit, so no registry credentials are needed) are installed first. `auto`
(default) picks the latest `v*` tag; while there are no releases it falls back
to the merge base with the base branch (`origin/$GITHUB_BASE_REF`, or
`origin/main`), else `HEAD^`. `none` skips the upgrade test. CI checks out the
full history for this (`fetch-depth: 0`).

## Operating notes the e2e encodes

- **Bootstrap and migrations are offline steps.** `protodex serve` neither
  migrates the database nor creates the first user, and SQLite takes an
  exclusive lock, so `bootstrap` and `migrate up` run in a one-off pod on the
  release's PVC while the Deployment is scaled to 0 (`e2e.sh admin <args>`).
  Run `migrate up` after every upgrade.
- **A volume restore brings back metadata only.** Blobs (file contents) are
  stored next to the database on the PVC until the S3 blob store (#8) is wired
  into `serve`; Litestream replicates only the database. The restore test
  therefore checks modules, commits and labels, and skips downloading content
  (`PROTODEX_E2E_CHECK_CONTENT=false`); turn that on once blobs live in S3.
- The restore relies on Litestream flushing the WAL when the pod stops
  (native sidecar). The test deletes the PVC right after a graceful scale to
  0, so a missing final sync fails it.

## CI

[`.github/workflows/kind-e2e.yml`](../../.github/workflows/kind-e2e.yml), job
`kind-e2e`, runs `make kind-e2e` on pushes to `main`, on demand, and on pull
requests that touch `deploy/`, `internal/`, `cmd/`, `test/conformance/`,
`go.mod`/`go.sum`, the `Dockerfile`, the `Makefile` or the workflow. Other
PRs get a successful no-op run, so the job can be a required check; see
[docs/branch-protection.md](../../docs/branch-protection.md#kind-e2e-and-path-filters).
On failure the log ends with pod descriptions, events and container logs.
