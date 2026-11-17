# Open compatibility questions

Status of each assumption from the plan (sections 3, 6, 10), with evidence.
Evidence comes from running the real `buf` CLI (1.32.0, 1.40.0 and, partly,
1.50.0/1.55.0) through the capture harness (`test/capture`) against
`buf.build`, run natively (no Docker) with `/etc/hosts` pointing `buf.build`
at the proxy, plus direct CLI runs and string inspection of the release
binaries. Raw recordings are git-ignored and not committed.

**Legend:** VERIFIED = observed; PARTIAL = observed in part; UNVERIFIED = could
not be tested yet, with the test that will settle it.

## Limits of this run

- There is no protodex server yet, so nothing was tested *against* a
  protodex host. Questions 1 and 2 need one (see "How to settle").
- No `BUF_TOKEN`/`PUSH_MODULE` was available, so no `push` was recorded.
- 1.50.0 and 1.55.0 runs were mostly 502s or empty from the proxy path
  (upstream flakiness / timeouts), so the per-RPC matrix is only trustworthy
  for 1.32.0 and 1.40.0. Re-run `make capture` with Docker to fill it in.

## 1. Cross-registry dependencies on push - UNVERIFIED

No evidence either way. The claim (from protos.rip) is that all transitive
deps must live on the module's own host. The `buf.lock` format supports it
structurally: each dep is `name: <host>/<owner>/<module>` + `commit` +
`digest`, so a lock can mention foreign hosts, but whether the *server*
rejects it is server behaviour.
**Scope decision:** assume same-host-only (the safe case). The mirror design
in ADR 0001 already keeps deps on our host. Do not implement cross-host
resolution; revisit only if a test shows the CLI sends such deps.
**How to settle:** once protodex accepts `UploadService/Upload`, push a module
whose `buf.lock` references `buf.build/...` and record the response.

## 2. `remote:` plugins with deps from our host - PARTIAL

`buf generate` with `remote: buf.build/protocolbuffers/go` calls
`buf.alpha.registry.v1alpha1.CodeGenerationService/GenerateCode` on the
*plugin's* host (observed in 1.32.0 and 1.40.0; response 400 with a dummy
token/unauthenticated). The host comes from the plugin reference, not from
the module's deps, so the call goes to buf.build regardless of where the
module lives. Whether buf.build accepts an image whose deps came from
another host was not tested (400 was returned before reaching that point).
**Scope decision:** protodex does not implement `CodeGenerationService`;
remote plugins keep working only as long as the plugin host (buf.build) is
reachable by the client and accepts the image. Treat as out of scope for MVP.

## 3. Minimum CLI version and v1 vs v1beta1 - PARTIAL

- 1.32.0 and 1.40.0 both use `buf.registry.module.v1` for `buf dep update`:
  `CommitService/GetCommits`, `GraphService/GetGraph` (x2),
  `DownloadService/Download`, `ModuleService/GetModules`. 1.40.0 also uses
  `LabelService/ListLabels` for `buf registry label list`.
- `buf registry module info` does not exist in 1.32.0 (unknown subcommand);
  it is present in 1.40.0+.
- No `v1beta1` call was observed in any recorded scenario. The binaries
  (1.32.0 and 1.55.0) contain client stubs for both
  `buf.registry.module.v1` and `v1beta1` (Commit, Download, Graph, Label,
  Module, Upload), 1.55.0 additionally `v1.ResourceService` and
  `buf.registry.plugin.v1beta1.*`, `buf.registry.owner.v1.*`. Which
  command triggers `v1beta1` is not established.
- 1.32.0 is the oldest version that calls `buf.registry.module.v1`
  (`buf.lock` v2 / `version: v2` config accepted, and `dep update`
  works), consistent with the target of >= 1.32.
**Scope decision:** keep minimum CLI = 1.32.0; implement `module.v1` services
first (Commit, Graph, Download, Module, Label, Upload). Defer `v1beta1`
until a capture shows a command that needs it.

## 4. Host remap / mirror setting - VERIFIED (none found)

`buf --help-tree` (1.55.0) shows no mirror/remap command, and the binary's
env vars are only `BUF_TOKEN`, `BUF_CACHE_DIR`, `BUF_ALPHA/BETA_SUPPRESS_WARNINGS`,
`BUF_INPUT_*`, `BUF_BETA_COPY_FILES_TO_MEMORY`, `BUF_REGISTRATION_CONFLICT`
and testing-only `BUF_TESTING_PUBLIC_REGISTRY` /
`BUF_TESTING_LEGACY_FEDERATION_REGISTRY`. The registry host is taken from the
module name, so consumers must rename the host in `buf.yaml`/`buf.lock`
(matches ADR 0001). The harness itself works by DNS override plus a trusted
cert (`SSL_CERT_FILE`), which is a test technique, not a supported setting.
The CLI dials port 443.

## 5. Commit IDs and digests - PARTIAL

Both 1.32.0 and 1.55.0 wrote an identical `buf.lock` for
`buf.build/bufbuild/protovalidate`:

```yaml
version: v2
deps:
  - name: buf.build/bufbuild/protovalidate
    commit: 511051f7f4374c3ca873b53ae68a9288
    digest: b5:a4a2d4d8...e3c8   # b5: + 128 hex chars (SHA-512)
```

- Commit ID: 32 lowercase hex chars (dashless UUID).
- Digest type: `b5` for v2 locks. `b4` was not observed here (it appears in
  legacy v1 locks; not produced by these versions).
- Whether the CLI *validates* the digest on `dep update`/`build`, and what it
  checks on `push`, was not tested (needs a push or a tampered mirror).
**Scope decision:** mirrors must preserve upstream commit IDs and `b5`
digests (as in ADR 0001). Generating a b5 digest ourselves for locally
pushed modules requires a clean-room implementation checked against the
values above.

## 6. Non-`buf.registry.*` endpoints - VERIFIED for observed commands

Observed non-`buf.registry.*` calls:
- `buf registry login` -> `buf.alpha.registry.v1alpha1.AuthnService/GetCurrentUser`
  (401 with a dummy token).
- `buf generate` with remote plugins ->
  `buf.alpha.registry.v1alpha1.CodeGenerationService/GenerateCode`.

`dep update`, `build`, `label list` and `module info` used only
`buf.registry.module.v1`. No well-known paths were requested.
**Scope decision:** MVP must serve `AuthnService/GetCurrentUser` (v1alpha1)
for `buf registry login` to succeed against protodex, in addition to
`module.v1`. Remote-plugin codegen stays out of scope (see 2).

## Plan impact

`docs/plan.html` is unchanged: no assumption was found wrong, but the login
dependency on the v1alpha1 AuthnService (6) should be added to the plan's
API list if it is not there yet.

## How to settle the open items

1. Re-run `make capture` with Docker on a networked machine; include
   `BUF_TOKEN`/`PUSH_MODULE` to record `push` (questions 3, 5).
2. When protodex serves `module.v1`, push a module with a foreign-host dep
   (1) and run `buf generate` with a `remote:` plugin against it (2).
