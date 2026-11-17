# Mirroring upstream modules

Published modules can depend on Google's protos by mirroring them onto this
registry, under the **same `owner/module` path as upstream**:

| Module | Upstream | Pin |
|--------|----------|-----|
| `googleapis/googleapis` | `github.com/googleapis/googleapis` (`google/`) | commit |
| `bufbuild/protovalidate` | `github.com/bufbuild/protovalidate` (`proto/protovalidate`) | tag |
| `grpc-ecosystem/grpc-gateway` | `github.com/grpc-ecosystem/grpc-gateway` (`protoc-gen-openapiv2/`) | tag |

A module then names `<host>/googleapis/googleapis` in the `deps` of its
`buf.yaml`; `buf dep update`, `buf build` and `buf push` work as usual.

## Running it

```sh
protodex mirror run    -db protodex.db -config mirror.yaml   # import / update
protodex mirror status -db protodex.db                       # provenance
```

The command opens the database exclusively, like `bootstrap` and `owner`, so
stop the server first. It needs `git` and network access to the upstreams.
The mirror owners (`googleapis`, `bufbuild`, `grpc-ecosystem`) are created on
demand; reserve them beforehand with `protodex bootstrap -mirror-owners`.

## mirror.yaml

```yaml
upstreams:
  - name: googleapis/googleapis    # owner/module here == upstream path
    url: https://github.com/googleapis/googleapis.git
    ref: 82983a28bdca6e289b1e283fa3aa309929509ba4   # tag or full commit hash
    license: Apache-2.0            # SPDX id, must be approved (see below)
    roots: ["."]                   # proto roots; paths are stored relative to them
    include: [google]              # optional: only .proto files under these prefixes
    labels: []                     # extra labels for the imported commit
    deps: []                       # other upstreams of this file this one depends on
```

- **Pins only.** `ref` is a tag or a full commit hash, never a branch. A commit
  pin is verified against the commit git resolved.
- **Exact content.** `.proto` files (and the upstream `LICENSE`) are stored
  byte for byte at their path relative to the root, so `go_package` and every
  other option are unchanged. Other files are not part of a module.
- **Labels.** The imported commit gets `main` (the default label, so an
  unpinned dependency resolves), the tag for a tag pin, and any `labels`.
  Bumping the pin creates a new commit and moves these labels.
- **Dependencies.** `deps` are imported first; the dependency commit is
  recorded as an edge of the new commit and folded into its digest. The
  upstreams above have no inter-dependencies (protovalidate only imports
  `google/protobuf`, which `buf` provides itself).
- **Idempotent.** The commit digest is computed from the content. When the
  latest commit already has that digest, nothing is written; a re-run with
  unchanged pins is a no-op.

## Provenance

Each mirrored commit stores its upstream URL, the pinned ref, the resolved
upstream commit, the license and the import time. `protodex mirror status`
lists them, and the API serves the upstream commit link as
`Commit.source_control_url` (visible with `buf registry commit info`).

## Access

Mirror modules are public, so everyone can read them. The permission model
(see [permissions.md](permissions.md)) caps every caller's permission on a
mirror owner's modules at **read**, instance admins and contributor grants
included, so pushes, label moves, settings changes and deletes over the API are
refused (`PermissionDenied`). Only `protodex mirror run`, which writes to the
database directly, changes them.

## Licenses

An upstream is only accepted if its SPDX license is in the approved list
(`Apache-2.0`, `BSD-2-Clause`, `BSD-3-Clause`, `MIT`) and each module ships
the upstream `LICENSE`. See [mirror-licenses.md](mirror-licenses.md) for the
review of each upstream.
