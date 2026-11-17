# Reading modules: download, resources, graph, descriptor sets

With a database and blob store configured, protodex serves the read side of
`buf.registry.module.v1`, so the stock CLI can consume pushed modules:

```sh
buf build registry.example.com/acme/weather            # default label
buf build registry.example.com/acme/weather:v1         # a label
buf build registry.example.com/acme/weather:<commit>   # a commit
buf export registry.example.com/acme/weather -o out/
buf dep update                                          # with deps on this host in buf.yaml
```

`buf build`/`buf export` of a remote module call `CommitService/GetCommits`,
`GraphService/GetGraph`, `ModuleService/GetModules`, `OwnerService/GetOwners`
and, unless the commit is in the CLI's cache, `DownloadService/Download`
(observed with buf 1.57.0, `test/clitest`). They do not call `FileDescriptorSetService`; it is
for API clients that want compiled descriptors.

## Resolving references

All read services resolve a `ResourceRef` the same way (as `CommitService`):

| Reference | Resolves to |
|-----------|-------------|
| `owner/module` | the commit of the module's default label |
| `owner/module` + `label_name` | that label's commit |
| `owner/module` + `ref` | the commit with that id in the module, else the label with that name |
| `id` | the commit, label or module with that id (tried in that order) |

So `:main`, `:<label>` and `:<commit-id>` select identical content when they
name the same commit. A module without commits (no default label yet) is
`NotFound` for Download, Graph and FileDescriptorSet; `GetResources` returns
the module itself. Digest references (`b5:...`) are not a `ResourceRef` form
and are not accepted.

## Services

- **DownloadService.Download** returns the files of each resolved commit
  (`.proto`, `LICENSE`, documentation). `file_types` and `paths` narrow the
  result; a path also selects the files below it (`foo/bar` matches
  `foo/bar/baz.proto`). A path matching no file (after the type filter) is
  `NotFound` unless `paths_allow_not_exist` is set.
- **ResourceService.GetResources** returns, per reference, the module, label
  or commit it names.
- **GraphService.GetGraph** returns the resolved commits, their transitive
  dependency commits (as recorded at push time) and the edges between them.
  Only dependencies on this registry exist (see
  [open-questions.md](behavior-spec/open-questions.md)).
- **FileDescriptorSetService.GetFileDescriptorSet** compiles the commit and
  its dependency commits (with `bufbuild/protocompile`; well-known types are
  built in) and returns the files in topological order. `exclude_imports`,
  `include_source_code_info` and `include_source_retention_options` are
  honoured; `include_types`/`exclude_types` return `Unimplemented`. A commit
  that does not compile is `FailedPrecondition`. Results are cached in memory
  (LRU, 64 MiB) by commit digest and options: a b5 digest pins the files and
  the dependency digests, so it pins the whole input.

## Access

Every read goes through the same visibility check as `ModuleService`
(`canRead` in `internal/server/modules.go`): public modules are readable
anonymously, private ones only by callers with access, and a hidden module,
label or commit is reported as `NotFound`. Dependencies reached through the
graph are checked too. When the permissions model (issue #26) lands, it
replaces that one function. The cache is consulted only after the check.

## Errors

| Condition | Code |
|-----------|------|
| Unknown or hidden module, label or commit; module without commits; missing path | `NotFound` |
| Module does not compile, or two modules of the graph share a path | `FailedPrecondition` |
| Type filters; cross-registry dependency | `Unimplemented` |
