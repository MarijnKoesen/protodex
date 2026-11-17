# Push and commits

With a database configured, protodex serves `buf.registry.module.v1.UploadService`
and `CommitService`, so `buf push` works with the stock CLI:

```sh
buf push                 # in a directory whose buf.yaml names registry.example.com/acme/weather
buf push --label=v1
buf registry commit info registry.example.com/acme/weather:<commit>
```

The module must exist (`buf registry module create`); push does not create it.

## How a push is stored

- File contents go to the blob store (`blob_dir`, default `<db_path>.blobs`;
  env `PROTODEX_BLOB_DIR`), keyed by SHA-256, so identical files are stored
  once across commits and modules. S3 is not wired into `serve` yet.
- Only module files are kept: `*.proto`, `LICENSE`, and the first of `buf.md`,
  `README.md`, `README.markdown`. Other files (`buf.yaml`, ...) are ignored.
- Commit, manifest, dependency edges and label moves are written in one
  database transaction. Concurrent pushes are serialised; a label always ends
  on the newest commit.
- The commit ID is a server-assigned 32-character dashless UUID. The server
  computes the commit's `b5` digest itself (see `internal/b5`) from the files
  and the digests of the dependency commits; it matches what the CLI computes.
- Pushing content identical to the module's latest commit returns that commit
  (idempotent) and only moves labels. Pushing older content again creates a
  new commit with the same digest, so labels never move backwards.
- Labels: the `--label` values, or the module's default label. Archived labels
  are unarchived by a push.
- Dependencies (`dep_commit_ids`) must be commits on this server that the caller
  can read; they are recorded as edges and folded into the digest. The CLI
  sends transitive dependencies too, and all of them are recorded.
- Pushed `source_control_url` is accepted but not stored yet.

## Limits and errors

`max_push_files` (default 5000) and `max_push_file_bytes` (default 10 MiB)
bound a module; the request as a whole is bounded by `max_request_bytes`.

| Condition | Code |
|-----------|------|
| No token | `Unauthenticated` |
| Unknown or hidden module, label id or dependency commit | `NotFound` |
| Writing another user's namespace or a mirror owner | `PermissionDenied` |
| Too many or too large files, duplicate paths, no module files, bad label name, same module twice, self- or duplicate-module dependency | `InvalidArgument` |

`CommitService.ListCommits` of a label or commit returns just that commit;
`id_query` is a case-insensitive substring match.

## Known limits

Modules uploaded together in one request are not made dependencies of each
other: only `dep_commit_ids` become edges (detecting local imports would need
parsing the files).

`buf push` of a module **with dependencies** also calls `GraphService` and
`DownloadService`; both are served now (see [reads.md](reads.md)), so the CLI
can push such modules once `buf dep update` has written `buf.lock`.
