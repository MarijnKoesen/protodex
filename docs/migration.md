# Migrating from buf.build

Moving a repository to Protodex touches the registry host in `buf.yaml`,
`buf.lock` and your plugin setup. Generated code is unaffected.

## 1. Get the modules onto Protodex

- Your own modules: `buf registry module create` then `buf push` against the
  new host (see the [quickstart](quickstart.md)).
- Public dependencies such as `googleapis`: [mirror](mirror.md) them, which
  imports upstream modules with provenance.

Users and organizations must exist first (`protodex owner create`).

## 2. Rename hosts (`protodex migrate-buf`)

The helper rewrites `buf.build/` to your host in every `buf.yaml` (`deps` and
module `name`), lists remote plugins in `buf.gen*.yaml`, and re-runs
`buf dep update`. It is a **dry run** unless you pass `-apply`.

```sh
protodex migrate-buf -to registry.example.com .            # preview
protodex migrate-buf -to registry.example.com -apply .     # write + buf dep update
```

| Flag | Default | |
|---|---|---|
| `-to` | required | Protodex `host[:port]` |
| `-from` | `buf.build` | host to replace |
| `-apply` | off | write files and run `buf dep update` |
| `-skip-dep-update` | off | with `-apply`, do not run buf |
| `-buf` | `buf` | buf binary |

By hand: replace the host in `deps:` and module `name:` of `buf.yaml`, and the
module name of v1 `buf.yaml`.

## 3. `buf.lock`

Never edit `buf.lock` by hand. `buf dep update` regenerates it against the new
registry; expect changed `name`, `commit` and `digest` values (commit IDs are
assigned by the server that stores them). Commit the new lock file. If a
dependency was mirrored, its digests match upstream because the digest is
computed from content.

## 4. Log in

```sh
buf registry login registry.example.com --username <user> --token-stdin
```

## Remote to local plugin mapping

Protodex does not host plugins. Replace `remote:`/`plugin: buf.build/...`
entries by local plugins:

| Remote plugin | Local plugin | Pin with |
|---|---|---|
| `buf.build/protocolbuffers/go` | `protoc-gen-go` | Go: `go get -tool google.golang.org/protobuf/cmd/protoc-gen-go@vX` |
| `buf.build/grpc/go` | `protoc-gen-go-grpc` | Go: `go get -tool google.golang.org/grpc/cmd/protoc-gen-go-grpc@vX` |
| `buf.build/connectrpc/go` | `protoc-gen-connect-go` | Go: `go get -tool connectrpc.com/connect/cmd/protoc-gen-connect-go@vX` |
| `buf.build/community/mfridman-go-json` | `protoc-gen-go-json` | Go: `go get -tool github.com/mfridman/protoc-gen-go-json@vX` |
| `buf.build/grpc-ecosystem/gateway` | `protoc-gen-grpc-gateway` | Go: `go get -tool github.com/grpc-ecosystem/grpc-gateway/v2/protoc-gen-grpc-gateway@vX` |
| `buf.build/bufbuild/es` | `protoc-gen-es` | npm: `npm i -D -E @bufbuild/protoc-gen-es` |
| `buf.build/connectrpc/es` | `protoc-gen-connect-es` | npm: `npm i -D -E @connectrpc/protoc-gen-connect-es` |

With the Go `tool` directive (Go 1.24+) plugins are pinned in `go.mod`; run
them through `go tool`:

```yaml
# buf.gen.yaml
version: v2
plugins:
  - local: ["go", "tool", "protoc-gen-go"]
    out: gen
    opt: paths=source_relative
  - local: ["go", "tool", "protoc-gen-go-grpc"]
    out: gen
```

For npm, pin exact versions and call the binary from `node_modules`:

```yaml
plugins:
  - local: node_modules/.bin/protoc-gen-es
    out: gen
    opt: target=ts
```

Pick plugin versions that match the ones the remote plugin used (check
`buf.gen.yaml` `remote: ...:vX`) to keep generated output identical.
`migrate-buf` only reports remote plugins; it never rewrites them.

## Generated SDKs

BSR-hosted SDKs (`buf.build/gen/go/...`, `@buf/...` npm packages) are not
served by Protodex. Generate code locally (above) and change imports to your
own packages; the generated *message and wire formats* are unchanged, but Go
import paths and npm package names differ. Services and clients keep
interoperating on the wire.
