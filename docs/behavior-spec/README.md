# Behaviour spec

Written from observation of the real `buf` CLI only (clean-room). Fill in
after running `make capture`; see [rpc-matrix.md](rpc-matrix.md).

## To document from captures
- **Digests:** algorithm/type (b4/b5), where the CLI sends and verifies them.
- **Commit IDs:** format (dashless UUID vs. legacy), per CLI version.
- **Labels:** creation on push, default label, archive/unarchive semantics.
- **v1 vs v1beta1:** which service versions each CLI version calls.

## Observed against protodex (buf 1.57.0, `test/clitest`)
- The CLI accepts `127.0.0.1:<port>/<owner>/<module>` remotes over TLS
  (trusting the server via `SSL_CERT_FILE`); `BUF_TOKEN` is sent as a bearer
  token.
- `registry module create` on an existing name reports
  `a module named "<remote>/<owner>/<module>" already exists`, so the CLI
  maps `AlreadyExists` itself.
- `registry module info --format=json` prints id, owner, name, create time,
  state and default label name, but not visibility or description.
- `registry module label list` on a module without commits prints nothing.
