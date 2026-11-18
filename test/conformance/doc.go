// SPDX-License-Identifier: Apache-2.0

// Package conformance is the CLI conformance suite: it starts protodex on an
// ephemeral store backend and drives released buf CLI binaries through
// login, module, push, label, dep, build, export, lint, breaking and
// generate scenarios. See docs/compatibility.md for how to run it and the
// resulting version matrix.
//
// The suite skips unless a buf binary is available:
//
//	PROTODEX_CONFORMANCE_BUF_VERSIONS=pr|all|1.57.2,...  download pinned, checksum-verified versions
//	PROTODEX_CONFORMANCE_BUF=/path/to/buf[,/path/to/other]  use these binaries
//	PROTODEX_TEST_BUF=/path/to/buf, or buf on PATH          use that binary
//
// Other settings:
//
//	PROTODEX_CONFORMANCE_BACKENDS=sqlite,...  store backends to run (default: all available)
//	PROTODEX_CONFORMANCE_FAULT=digest         inject a server bug; the suite must fail
//	PROTODEX_CONFORMANCE_REPORT=out.md        write a Markdown result matrix
//	PROTODEX_CONFORMANCE_CACHE=dir            download cache (default: user cache dir)
package conformance
