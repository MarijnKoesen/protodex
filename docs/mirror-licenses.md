# Mirror license review

An upstream is mirrored (listed in `mirror.yaml`) only after its license has
been checked to permit redistribution of the files. `protodex mirror run`
refuses licenses outside the approved list (`internal/mirror`,
`AllowedLicenses`); adding one requires an entry here first.

Every mirrored module carries the upstream `LICENSE` file, which satisfies the
notice requirements of the licenses below.

| Upstream | License | Checked | Redistribution |
|----------|---------|---------|----------------|
| `googleapis/googleapis` | Apache-2.0 | 2026-11-17, upstream `LICENSE` at the pinned commit | Permitted (Apache-2.0 §4: provide the license text, keep notices) |
| `bufbuild/protovalidate` | Apache-2.0 | 2026-11-17, upstream `LICENSE` at `v1.2.2` | Permitted (same terms) |
| `grpc-ecosystem/grpc-gateway` | BSD-3-Clause | 2026-11-17, upstream `LICENSE` at `v2.34.0` ("Copyright (c) 2015, Gengo, Inc.") | Permitted (retain the copyright notice and conditions; no endorsement use of the names) |

This is an engineering review of the license texts, not legal advice;
operators mirroring other upstreams are responsible for their own review.
Files are not modified, so no "modified" notices are needed.
