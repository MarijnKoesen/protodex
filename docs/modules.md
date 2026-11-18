# Modules and labels

A module lives at `<host>/<owner>/<module>`; see [owners.md](owners.md) for
owners. With a database configured, protodex serves
`buf.registry.module.v1.ModuleService` and `LabelService`, so these work with
the stock `buf` CLI:

```sh
buf registry module create registry.example.com/acme/weather --visibility=public
buf registry module info registry.example.com/acme/weather
buf registry module settings update registry.example.com/acme/weather --description=... --default-label-name=trunk
buf registry module deprecate|undeprecate registry.example.com/acme/weather
buf registry module delete registry.example.com/acme/weather
buf registry module label list|info|archive|unarchive registry.example.com/acme/weather[:label]
```

## Rules

- **Module names**: 2-100 characters; lowercase letters, digits and hyphens,
  starting with a letter and not ending with a hyphen. Unique per owner.
- **Visibility** defaults to `private`. Public modules are readable
  anonymously; private modules only by callers with a permission on them.
  A private module the caller cannot read is reported as `NotFound`.
- **Writes** need a token. Pushing and label changes need *write*
  permission on the module; settings changes (`UpdateModules`) and deletion
  need *admin*; creating needs a `member` role in one's own namespace or an
  organization's (or the instance `admin` role). Mirror owners' modules are
  read-only over the API (`PermissionDenied`). Roles, contributor grants
  and the full matrix are in [permissions.md](permissions.md).
- **Default label**: `main` unless set on creation. It is a name only: labels
  point at commits, so the label itself appears with the first push. It may
  name a label that does not exist yet, but not an archived one.
- **Labels**: 1-250 characters, no whitespace or control characters
  (`v1.2.0`, `feature/x` are fine). A label can only move to a commit of its
  own module that is not older than its current commit. Moving an archived
  label unarchives it. The default label cannot be archived. Label ids stay
  stable when a label moves.
- Batch RPCs (`CreateModules`, `UpdateModules`, `DeleteModules`,
  `CreateOrUpdateLabels`, `ArchiveLabels`, `UnarchiveLabels`) are atomic.
- List RPCs page with `page_size` (max 250) and an opaque `page_token`, and
  order by create or update time (newest first by default).
  `ListLabels` excludes archived labels unless `archive_filter` says
  otherwise; its `resource_ref` may name a module, a label or a commit.
  Commit check status is always reported as `DISABLED` (no policy checks).

## Error codes

| Condition | Code |
|-----------|------|
| No token on a write | `Unauthenticated` |
| Unknown or hidden owner/module/label/commit | `NotFound` |
| Module name taken | `AlreadyExists` |
| Invalid module or label name | `InvalidArgument` |
| Missing write/admin permission on a readable module, creating in another user's namespace, a mirror owner, read-only role | `PermissionDenied` |
| Moving a label backwards, archiving the default label, archived default label | `FailedPrecondition` |

`ListLabelHistory` returns `Unimplemented` (label history is not recorded
yet). Only `module.v1` is served: no captured CLI command uses `v1beta1`
(see [open-questions.md](behavior-spec/open-questions.md#3-minimum-cli-version-and-v1-vs-v1beta1-partial)).

## CLI test

`test/clitest` drives the real `buf` CLI against an in-process server over
TLS (`<127.0.0.1:port>/owner/module` names). It skips unless `buf` is on
`PATH` or `PROTODEX_TEST_BUF` points at it:

```sh
GOBIN=$PWD/bin/tools go install github.com/bufbuild/buf/cmd/buf@v1.57.0
PROTODEX_TEST_BUF=$PWD/bin/tools/buf go test ./test/clitest/ -v
```
