# ADR 0001: Naming and addressing

- Status: Accepted
- Date: 2026-11-17

## Context

The project began under the placeholder name "Openbuf" with module path
`example.com/openbuf`. "Buf" is a trademark of Buf Technologies, Inc., and the
project is an independent clean-room implementation that is not affiliated
with them (see README non-goals). Keeping "buf" in the product name risks
trademark problems, so we renamed before code accumulated.

## Decision

| Item | Value |
|------|-------|
| Project / binary name | `wireyard` |
| Go module path | `github.com/marijnkoesen/wireyard` |
| Container image | `ghcr.io/marijnkoesen/wireyard` |
| Helm chart | OCI: `oci://ghcr.io/marijnkoesen/charts/wireyard` (chart name `wireyard`) |
| Domain | None yet. The server host is always configuration, never hard-coded. |

The module path, image and chart live under the GitHub owner so they need no
domain purchase. They are chosen publication locations; nothing is published
there yet. If the project moves to an organisation or a custom domain, the
module path changes in a single mechanical PR. The GitHub repository itself
must be renamed from `openbuf` to `wireyard` (GitHub redirects the old name).

### Mirror addressing

Mirrored modules keep their upstream `owner/module` path under our host:

    <host>/googleapis/googleapis   (mirrors buf.build/googleapis/googleapis)

Migrating a consumer from `buf.build` is therefore a host rename in `buf.yaml`
dependencies (`deps:`) and `buf.lock`, with no path rewriting, provided the
mirror preserves upstream commit IDs, content digests and dependency
references, because `buf.lock` pins those, not just names. If a mirror cannot
guarantee that, consumers must regenerate `buf.lock` (`buf dep update`) after
switching hosts. Locally created modules use the same `<host>/<owner>/<module>`
form; mirrored owners are reserved so a local owner cannot shadow an upstream
one.

## Consequences

- One consistent name across code, image and chart.
- A later move to an organisation or domain is cheap.
