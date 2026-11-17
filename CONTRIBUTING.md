# Contributing to Protodex

Thanks for helping! Please read this whole file before opening a PR. The
clean-room rules are **mandatory**: a PR that breaks them gets rejected no
matter how good the code is.

## Clean-room provenance rules

Protodex re-implements a registry API compatible with the `buf` CLI. To keep
the project legally clean, **only these public inputs may be used**:

1. [`bufbuild/registry-proto`](https://github.com/bufbuild/registry-proto):
   the public API definitions (Apache-2.0).
2. The [`bufbuild/buf`](https://github.com/bufbuild/buf) client source code
   (Apache-2.0).
3. Public documentation (for example buf.build/docs).
4. Black-box behaviour of the real, publicly released `buf` CLI
   (requests/responses you observe yourself against your own server or
   public endpoints).

**Not allowed**, directly or as "reference":

- Code, fixtures, schemas or configs from private, leaked or otherwise
  non-public Buf server code.
- Anything obtained by decompiling or disassembling Buf container images
  or binaries.
- Closed-source protos or code, including protos.rip-derived code.
- Buf's name or logos in product naming, branding or binaries. Describing
  compatibility ("works with the `buf` CLI") is fine.

If you have ever had access to non-public Buf server code, say so in the
PR and do not work on the affected areas. If you are unsure whether a
source is allowed, ask in an issue **before** using it.

Each PR template includes a provenance checkbox. Ticking it states that
your change follows these rules.

## Developer Certificate of Origin (DCO)

All commits must be signed off under the
[Developer Certificate of Origin 1.1](https://developercertificate.org/):

```sh
git commit -s -m "feat: add thing"
```

This adds `Signed-off-by: Your Name <you@example.com>` to the commit. It
certifies that you wrote the change or otherwise have the right to submit
it under Apache-2.0. The `dco` check fails if any commit in the PR lacks
the trailer. To fix existing commits: `git rebase --signoff origin/main`.

## License headers

Every source file (`*.go`, `*.proto`, `*.sh`, `Makefile`, `Dockerfile`)
must start with an SPDX header in its first five lines:

```go
// SPDX-License-Identifier: Apache-2.0
```

`make license-headers` checks this. New dependencies must use an
Apache-2.0-compatible license (Apache-2.0, BSD-2/3-Clause, MIT, ISC,
MPL-2.0), enforced by `make licenses` and dependency review.

## Local workflow

CI runs the same Makefile targets you run locally:

| Command                | What it does                                            |
|------------------------|---------------------------------------------------------|
| `make lint`            | SPDX header check, `go vet`, `golangci-lint`            |
| `make test`            | `go test -race` with coverage (`coverage.out`)          |
| `make build`           | `go build ./...` and `bin/protodex`                      |
| `make generate`        | `go generate` (+ `buf generate` once configured)        |
| `make check-generated` | fails if `go mod tidy` / `make generate` change files   |
| `make licenses`        | dependency license check (go-licenses)                  |
| `make vuln`            | `govulncheck`                                           |
| `make docker`          | builds the container image locally                     |

`make tools` installs the pinned golangci-lint version.

## Pull requests

- PR titles follow [Conventional Commits](https://www.conventionalcommits.org/)
  (`feat:`, `fix:`, `docs:`, `ci:`, `build:`, `chore:`, `refactor:`,
  `test:`, `perf:`…), which the `pr-title` check enforces. PRs are squash-merged,
  so the title becomes the commit message.
- Keep PRs focused, add tests, and update docs.
- At least one approving review from a CODEOWNER is required.

## Required status checks

Branch protection on `main` requires these checks (GitHub job names) to
pass. See [docs/branch-protection.md](docs/branch-protection.md).

| Check               | Workflow | Verifies                                                         |
|---------------------|----------|------------------------------------------------------------------|
| `build-test`        | ci       | `make build`, `make test` (race + coverage report)               |
| `lint`              | ci       | `make lint` (SPDX headers, `go vet`, golangci-lint)              |
| `generated`         | ci       | `go mod tidy` / `go generate` produce no diff                    |
| `licenses`          | ci       | LICENSE/NOTICE present, SPDX headers, go-licenses                |
| `proto`             | ci       | `buf lint`, `buf format --diff` (no-op until `.proto` files)     |
| `docker`            | ci       | hadolint + Docker build (no push)                                |
| `helm`              | ci       | `helm lint`, kubeconform (no-op until a chart exists)            |
| `security`          | ci       | gitleaks secret scan, govulncheck                                |
| `dependency-review` | ci       | PR dependency checks (private repos require opt-in; see branch-protection.md) |
| `dco`               | pr       | every commit has `Signed-off-by`                                 |
| `pr-title`          | pr       | PR title follows Conventional Commits                            |

If you rename a job, update this table and the branch protection settings
in the same PR.
