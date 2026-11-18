# FAQ and non-goals

**Does Protodex work with the normal `buf` CLI?** Yes; see the
[matrix](compatibility.md) (1.32.2 and newer).

**Can I keep using remote plugins (`buf.build/protocolbuffers/go`)?** Remote
plugins are hosted by the BSR, not by Protodex. Use local plugins; see the
[mapping table](migration.md#remote-to-local-plugin-mapping).

**Do I need to regenerate code after migrating?** No. Generated code does not
depend on the registry. Only [generated SDK import paths](migration.md#generated-sdks)
differ if you consumed BSR-hosted SDKs.

**Does it scale horizontally?** No. SQLite means one writer; run one replica
and use Litestream for durability.

**Why does `bootstrap` complain the database is locked?** The server is
running. Stop it first.

**Why does `protodex migrate` not migrate my repository?** `migrate` applies
database migrations. The repository helper is `migrate-buf`.

## Non-goals

- Copying, decompiling or deriving from any non-public Buf server code,
  container images or closed-source protos.
- Using Buf's name or logos as product branding.
- Feature parity with every hosted BSR product feature; the target is the
  registry API the open-source `buf` CLI uses (no hosted remote plugins,
  hosted SDKs, or Buf's policy/checks products).
