# Protodex

Protodex is an open-source (Apache-2.0), self-hostable Protobuf schema
registry that is wire-compatible with the public
[`bufbuild/registry-proto`](https://github.com/bufbuild/registry-proto) API,
so the stock `buf` CLI can push to and pull from it.

It is an independent clean-room implementation, not affiliated with or
endorsed by Buf Technologies, Inc.

## Where to start

- **[Quickstart](quickstart.md)**: run Protodex with Docker Compose or Helm and
  push your first module.
- **[Migrating from buf.build](migration.md)**: move an existing repository
  (hosts, `buf.lock`, plugins) and use `protodex migrate-buf`.
- **[Configuration](configuration.md)**, **[Operations](operations.md)**
  (backup, restore, upgrades) and **[Security](security.md)**.
- **[buf CLI compatibility](compatibility.md)**: the tested version matrix.
- **[FAQ and non-goals](faq.md)**.

## Status

Protodex is early software. Check the [compatibility matrix](compatibility.md)
and the [roadmap](plan.html) for what is implemented.
