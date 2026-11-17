# ADR 0002: Policy APIs (`policy/v1`) and breaking-change gates

- Status: Accepted (deferred; revisit on the triggers below)
- Date: 2026-11-17

## Context

Issue #33 asks us to evaluate demand for a `policy/v1` API and server-side
breaking-change checks on push, and to build them only if needed. Remote
plugins and plugin APIs are out of scope regardless.

What exists today:

- Protodex serves the vendored `module/v1`, `owner/v1` and `iam/v1` services,
  which are what the stock `buf` CLI needs to push and pull (see
  [push.md](../push.md)).
- `proto/` does not contain `policy/v1`. The vendored subset is pinned to one
  `registry-proto` commit and only changes by copying files from upstream
  (see [proto/README.md](../../proto/README.md)). Nothing in the current
  repository, plan or behaviour spec depends on it.
- The dependencies #26 (roles, visibility, contributors) and #12 are done, so
  per-module authorization is available if policy is ever enforced.

## Evaluation

- **Client-side checks already cover the main case.** `buf breaking` runs in
  CI against a registry module or git ref using only `ModuleService`,
  `CommitService` and `DownloadService`, which protodex serves. Teams get
  breaking-change gating without any server feature.
- **No demand signal.** No issue, user report or plan item asks for
  server-enforced policy. The issue itself is labelled "Later, optional".
- **Cost and risk are high.** A server-side gate needs a breaking-change
  engine (rule sets, FILE/PACKAGE/WIRE categories, per-module configuration),
  and every new rule is a compatibility and clean-room obligation. A wrong
  rejection blocks pushes, which makes false positives expensive.
- **Wire contract is unsettled.** `policy/v1` would have to be vendored from
  upstream or written fresh. A fresh proto risks diverging from what `buf`
  expects, which defeats the wire-compatibility goal.

## Decision

Do not implement `policy/v1` or server-side breaking-change gates now. Keep
`UploadService` unchanged. Document `buf breaking` against the registry in CI
as the supported way to gate breaking changes.

## Revisit when

Reopen this ADR (and file a scoped implementation issue) if any of these hold:

1. Multiple users ask for push-time enforcement that cannot be done in CI,
   for example because pushes come from untrusted or uncontrolled clients.
2. A `buf` CLI feature we want to support requires `policy/v1` endpoints.
3. A stable upstream `policy/v1` is available to vendor under the existing
   process.

A first implementation should then be limited to a per-module opt-in check
in `UploadService` that rejects a push with `failed_precondition` and reuses
the existing roles from [permissions.md](../permissions.md).
