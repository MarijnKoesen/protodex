# Behaviour spec

Written from observation of the real `buf` CLI only (clean-room). Fill in
after running `make capture`; see [rpc-matrix.md](rpc-matrix.md).

## To document from captures
- **Digests:** algorithm/type (b4/b5), where the CLI sends and verifies them.
- **Commit IDs:** format (dashless UUID vs. legacy), per CLI version.
- **Labels:** creation on push, default label, archive/unarchive semantics.
- **v1 vs v1beta1:** which service versions each CLI version calls.
