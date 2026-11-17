# Traffic-capture harness

Records which registry RPCs the real `buf` CLI calls, per CLI version.
It records **behaviour only**; do not commit bulk buf.build content.

- `proxy.go`/`record.go`: logging reverse proxy; redacts `Authorization`,
  cookies and any JSON field named like token/secret/password.
- `matrix.go`: aggregates recordings into the RPC x version table.
- `scenarios/run.sh`: login, dep update, build, generate, registry
  module/label and (optionally) push, run inside a container per CLI version.
- `versions.txt`: CLI versions to test (edit; add newer ones as released).

## Usage

    make capture                       # all versions, read-only scenarios
    make capture VERSIONS="1.50.0"     # subset
    BUF_TOKEN=... PUSH_MODULE=me/mod make capture   # also push scenarios

Raw recordings go to `test/capture/out/<version>/<scenario>.jsonl`
(git-ignored; review/sanitise before promoting small fixtures to
`test/fixtures/`). `make capture-matrix` regenerates
`docs/behavior-spec/rpc-matrix.md` from the recordings.
Requires Docker and network access to buf.build.
