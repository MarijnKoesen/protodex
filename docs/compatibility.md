# buf CLI compatibility

Protodex is tested against released `buf` CLI binaries by the conformance
suite in [`test/conformance`](https://github.com/MarijnKoesen/protodex/blob/main/test/conformance). Each scenario starts a
fresh protodex (ephemeral store + filesystem blob store, a bootstrapped
`admin` token, organization `acme`) over TLS and drives the real CLI.

## Matrix

> The suite trusts its ephemeral TLS certificate via `SSL_CERT_FILE`, which
> Go binaries on macOS ignore (older buf releases). Run it on Linux; macOS
> is not supported by the harness.

Result of the full matrix on the SQLite backend (2026-11-17, linux/amd64,
`make conformance CONFORMANCE_VERSIONS=all`):

| backend | buf | login | module | organization | push | labels | dep-update | build | export | lint | breaking | generate |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| sqlite | 1.32.2 | pass | n/a | n/a | pass | n/a | pass | pass | pass | pass | pass | pass |
| sqlite | 1.35.1 | pass | n/a | n/a | pass | n/a | pass | pass | pass | pass | pass | pass |
| sqlite | 1.40.1 | pass | pass | pass | pass | pass | pass | pass | pass | pass | pass | pass |
| sqlite | 1.45.0 | pass | pass | pass | pass | pass | pass | pass | pass | pass | pass | pass |
| sqlite | 1.50.1 | pass | pass | pass | pass | pass | pass | pass | pass | pass | pass | pass |
| sqlite | 1.55.1 | pass | pass | pass | pass | pass | pass | pass | pass | pass | pass | pass |
| sqlite | 1.57.2 | pass | pass | pass | pass | pass | pass | pass | pass | pass | pass | pass |
| sqlite | 1.60.0 | pass | pass | pass | pass | pass | pass | pass | pass | pass | pass | pass |
| sqlite | 1.65.0 | pass | pass | pass | pass | pass | pass | pass | pass | pass | pass | pass |
| sqlite | 1.70.0 | pass | pass | pass | pass | pass | pass | pass | pass | pass | pass | pass |
| sqlite | 1.75.0 | pass | pass | pass | pass | pass | pass | pass | pass | pass | pass | pass |

`n/a`: the CLI version has no such command (`buf registry module ...`,
`buf registry organization ...` and label commands arrived in the 1.40 line). On those versions the other
scenarios create modules directly in the store instead.

**Minimum supported version: 1.32.2** (the first CLI line with v2
`buf.yaml` and the `buf.registry.*.v1` API protodex implements). Older CLIs
speak the `v1alpha1` registry API and are not supported. The latest tested
release is 1.75.0. The nightly CI run refreshes this matrix; update the table
from its job summaries when it changes.

## Scenarios

| Scenario | What the CLI does |
|----------|-------------------|
| `login` | unauthenticated `push` is refused; `registry login --token-stdin` writes `.netrc`; `registry whoami` (where available); `push` with the stored credentials; `registry logout`; push refused again |
| `module` | `registry module create` / `info --format=json` / duplicate create (`already exists`) / `settings update` (or `update`) / `deprecate` / `undeprecate` / `delete` |
| `organization` | `registry organization create` / duplicate create and clash with an existing owner fail / `info --format=json` / `update --description --url` / `delete` is refused while the organization owns a module, then works |
| `push` | push, idempotent re-push, `--label`, changed content → new commit, `commit info`, push to a missing module fails |
| `labels` | `label list`, `info`, `archive` (hidden from list, shown with `--archive-status=archived`), default label cannot be archived, `unarchive` |
| `dep-update` | `dep update` writes commit + b5 digest to `buf.lock`; a new upstream commit is picked up; build from a cold cache; push records the dep; remote build includes it |
| `build` | `build` of a remote module by name, `:main`, `:label` and `:commit` gives identical images; unknown label / module fail; local workspace with remote dep builds from a cold cache |
| `export` | `export` by name and commit; exporting a dependent module includes its dependency; missing module fails |
| `lint` | local module importing a remote module with a lint violation lints clean (deps are not linted); a local violation is reported; `buf lint <remote>` reports the remote violation |
| `breaking` | `breaking --against <remote>` passes for unchanged content and reports a deleted field |
| `generate` | `buf generate` with a **local** plugin (`protoc-gen-conformance`, built from this repo): local files only by default, remote dependency with `--include-imports`, and a remote module as input |

Each scenario adapts to the CLI by probing `buf <command> --help`, not by
hard-coded version ranges, except the lint category: `STANDARD` from 1.40,
`DEFAULT` before.

## `buf registry` command support

Every `buf registry` subcommand that talks to the registry API, as of buf
1.75.0:

| Command | Status |
|---------|--------|
| `login`, `logout`, `whoami` | supported (`login`, `whoami` scenarios) |
| `module create`, `info`, `update` / `settings update`, `deprecate`, `undeprecate`, `delete` | supported (`module` scenario) |
| `module label list`, `info`, `archive`, `unarchive` | supported (`labels` scenario) |
| `module commit info` | supported (`push` scenario) |
| `module commit list`, `resolve`, `add-label` | served by implemented RPCs (`ListCommits`, `GetCommits`, `CreateOrUpdateLabels`) but not covered by a scenario yet |
| `organization create`, `info`, `update` (`--description`, `--url`), `delete` | supported (`organization` scenario). Instance admins only. `delete` is refused while the organization owns modules, and for mirror owners |
| `plugin ...`, `policy ...`, `sdk ...` | **not supported**: protodex implements no plugin, policy or generated-SDK services; the CLI gets `unimplemented` |
| `cc` | local only (clears the CLI cache) |

There is no `buf registry user` or member command: users are provisioned by
an instance admin (`UserService.CreateUsers` / `DeleteUsers`, or
`protodex owner`) or through OIDC, and organization membership and
organization settings (`GetOrganizationSettings`, `UpdateOrganizationSettings`)
are not modeled (see [owners.md](owners.md)).

## Running it

```sh
make conformance                                   # PR subset: 1.32.2, 1.57.2, 1.75.0
make conformance CONFORMANCE_VERSIONS=all          # full matrix
make conformance CONFORMANCE_VERSIONS=1.60.0       # one version
PROTODEX_CONFORMANCE_BUF=/usr/local/bin/buf go test ./test/conformance/   # your own binary
```

Without any of these settings the suite uses `PROTODEX_TEST_BUF` or `buf` on
`PATH`, and **skips** if neither exists, so `go test ./...` never needs the
network. Settings (see also the [package doc](https://github.com/MarijnKoesen/protodex/blob/main/test/conformance/doc.go)):

| Variable | Meaning |
|----------|---------|
| `PROTODEX_CONFORMANCE_BUF_VERSIONS` | `pr`, `all` or `1.57.2,1.75.0`: download those releases |
| `PROTODEX_CONFORMANCE_BUF` | comma-separated buf binaries to test |
| `PROTODEX_CONFORMANCE_BACKENDS` | store backends to run (default: all registered) |
| `PROTODEX_CONFORMANCE_REPORT` | write the Markdown result table to this file |
| `PROTODEX_CONFORMANCE_FAULT` | `digest`: inject a server bug (the suite must fail) |
| `PROTODEX_CONFORMANCE_CACHE` | download cache (default `<user cache dir>/protodex/buf`) |

Test names are `TestConformance/<backend>/buf-<version>/<scenario>`, so
`-run 'TestConformance/sqlite/buf-1.75.0/push'` selects one cell.

### Downloads are checksum-verified

`go run ./test/conformance/cmd/fetchbuf get 1.75.0` (used by the suite)
downloads `buf-<OS>-<arch>` from the GitHub release and refuses it unless its
SHA-256 matches [`bufdl/checksums.txt`](https://github.com/MarijnKoesen/protodex/blob/main/test/conformance/bufdl/checksums.txt),
which is committed. To add a release:

1. `go run ./test/conformance/cmd/fetchbuf pin 1.76.0` (records the release's
   published `sha256.txt` entries for Linux/macOS, x86_64/arm64).
2. Add the version to [`bufdl/versions.txt`](https://github.com/MarijnKoesen/protodex/blob/main/test/conformance/bufdl/versions.txt)
   (mark it `pr` if it becomes the latest; move the marker off the old one).
3. `make conformance CONFORMANCE_VERSIONS=1.76.0`, then update the table above.

### Failure output

On failure the test prints the CLI command, its stdout and stderr, and the
server's request log for that scenario: every `buf` invocation interleaved
with the access log (procedure, Connect code, duration) and the bodies of
error responses.

## Store backends

The suite is parametrised by metadata store backend
([`backend_test.go`](https://github.com/MarijnKoesen/protodex/blob/main/test/conformance/backend_test.go)). SQLite is the
only one today. A new backend (Postgres, #23) registers a `Backend` from an
`init` function in `test/conformance/backend_<name>_test.go`, with a `Skip`
func that opts out when its database is not configured (for example no DSN
in the environment), and the whole matrix runs against it as
`TestConformance/<name>/...`.

## Catching digest bugs

`TestDigestFaultIsCaught` runs the `dep-update` and `build` scenarios with a
deliberate digest bug: the server stores correct data but sends every
module digest (in `Upload`, `GetCommits`, `GetGraph`, `Download` responses)
with its last byte changed. It passes only if those scenarios fail **because
of the digest**; the CLI reports `Digest verification failed ... Expected
digest (from buf.lock) ... Actual digest ...`. This runs on every suite run.
To see the whole matrix go red by hand:

```sh
PROTODEX_CONFORMANCE_FAULT=digest make conformance CONFORMANCE_VERSIONS=1.57.2
```

(verified 2026-11-17 with buf 1.57.2: `dep-update`, `build`, `export`, `lint`,
`breaking` and `generate` fail; `login`, `module`, `push` and `labels` do not
read digests back and pass.)

## Golden fixtures

The traffic-capture harness (`test/capture`, #3) has not produced committed
fixtures yet (`docs/behavior-spec/rpc-matrix.md` is unpopulated and there is
no `test/fixtures/`), so the suite checks behaviour end to end instead of
comparing RPC traffic against recordings. Once fixtures exist, they can be
asserted from the request log the harness already keeps.

## Observed CLI behaviour

- `buf push --label=X` moves only `X`, not the default label; the first push
  without `--label` sets the default label.
- `buf registry login <host:port> --token-stdin` writes an entry for the
  host (without the port) to `$HOME/.netrc`, which later commands use when
  `BUF_TOKEN` is unset.
- The CLI verifies downloaded module content against the b5 digest the
  server reported, so a wrong digest fails `dep update` and `build` with
  `Digest verification failed`, and with them every scenario that reads a
  pushed module back.
- `buf registry label ...` (1.40 line) became `buf registry module label ...`
  later; the old path still exists, hidden, in 1.57.
