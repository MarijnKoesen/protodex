# Protodex

> Name, module path and addressing are recorded in
> [ADR 0001](docs/adr/0001-naming-and-addressing.md).

Protodex is an open-source (Apache-2.0), self-hostable Protobuf schema
registry that aims to be wire-compatible with the public
[`bufbuild/registry-proto`](https://github.com/bufbuild/registry-proto) API,
so the standard `buf` CLI can push to and pull from it.

It is an independent **clean-room** implementation, not affiliated with or
endorsed by Buf Technologies, Inc. See [CONTRIBUTING.md](CONTRIBUTING.md#clean-room-provenance-rules).

## Status

Early bootstrap. `protodex serve` runs a skeleton server: all MVP RPCs return `unimplemented`, plus `/healthz` and `/readyz`. The roadmap and analysis are in
[`docs/plan.html`](docs/plan.html).

## Quickstart

```sh
docker compose -f deploy/docker-compose.yaml up -d --build
curl -fsS http://localhost:8080/healthz
```

Runs protodex with MinIO and a SQLite volume. See
[`deploy/README.md`](deploy/README.md) for settings, which parts are not
functional yet, the container image and the signed release artifacts.

## Non-goals

- Copying, decompiling or deriving from any non-public Buf server code,
  container images or closed-source protos.
- Using Buf's name or logos as product branding.
- Feature parity with every hosted Buf Schema Registry product feature;
  the target is the registry API the open-source `buf` CLI uses.

## Development

Requires Go (version in `go.mod`), [golangci-lint](https://golangci-lint.run)
v2 (`make tools`), and Docker for `make docker`.

```sh
make lint test build   # same commands CI runs
make check-generated   # go mod tidy / go generate leave no diff
make help              # list targets
```

## Repository layout

| Path        | Contents                                       |
|-------------|------------------------------------------------|
| `cmd/`      | Binaries (`cmd/protodex`)                        |
| `internal/` | Private Go packages                             |
| `proto/`    | Protobuf definitions owned by this project      |
| `deploy/`   | Helm chart and deployment manifests             |
| `docs/`     | Design docs, plan, branch-protection policy     |
| `test/`     | Integration / black-box compatibility tests     |
| `scripts/`  | CI helper scripts (SPDX header, DCO)            |

## License

[Apache License 2.0](LICENSE). See also [NOTICE](NOTICE).
