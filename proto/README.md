# proto/

Protobuf definitions.

- `buf/registry/**` is a vendored, unmodified subset of
  [`bufbuild/registry-proto`](https://github.com/bufbuild/registry-proto)
  (Apache-2.0, license in `LICENSE.registry-proto`) pinned at commit
  `b3fdcc9c36fd4b8d8fea6336c62282587af4e631`: `module/v1`, `owner/v1`,
  `iam/v1` (token, scope) and the `resource/v1` and `priv/*` files they import.
  It is the wire contract the `buf` CLI speaks. To bump it, copy the same
  paths from the new commit, update this file and `NOTICE`, and run
  `make generate`.
- `buf.yaml` declares the `buf.build/bufbuild/protovalidate` dependency,
  pinned in `buf.lock`. Vendored files are excluded from linting.

Go and connect-go code is generated into `../gen` by `make generate` (see
`../buf.gen.yaml`) and checked in; CI's `generated` job fails on drift.

Only add protos of our own that satisfy the clean-room rules in
[CONTRIBUTING.md](../CONTRIBUTING.md).
