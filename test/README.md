# test/

Integration and end-to-end tests (for example, black-box compatibility tests
against the public `buf` CLI). Unit tests live next to the code they test.

- `clitest/`: runs the real `buf` CLI (`registry module ...`, `registry
  module label ...`, `push`, `build`, `export`, `dep update`) against an in-process protodex; skipped unless `buf` is
  on `PATH` or `PROTODEX_TEST_BUF` is set. See [docs/modules.md](../docs/modules.md#cli-test).
- `conformance/`: the CLI conformance suite: every scenario (login, module,
  push, labels, dep update, build, export, lint, breaking, generate with a
  local plugin) × pinned, checksum-verified buf releases × store backend.
  `make conformance`; see [docs/compatibility.md](../docs/compatibility.md).
- `capture/`: records which registry RPCs each buf version calls.
