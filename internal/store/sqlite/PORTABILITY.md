# Portable SQL subset

Queries and migrations are written so a Postgres backend can share them.

- Placeholders: `?` (the Postgres backend rebinds to `$n`).
- No `INSERT OR REPLACE`, `INSERT OR IGNORE`, `REPLACE INTO`, `AUTOINCREMENT`,
  `rowid`, or SQLite-only functions. Upserts use
  `INSERT ... ON CONFLICT (cols) DO UPDATE/DO NOTHING`, which both support.
- Timestamps are `BIGINT` unix microseconds; booleans are `INTEGER` 0/1.
- IDs are application-generated `TEXT` (random hex), never DB-generated.
- Binary data is `BLOB` (map to `BYTEA` on Postgres).
- Foreign keys are declared; SQLite enables them with `PRAGMA foreign_keys=ON`.

Queries live in `queries/*.sql` and are compiled by sqlc (`make sqlc`, engine
`sqlite`) into `db/`; `queries.go` adapts the generated code to `store.Querier`.
A Postgres backend can reuse the `.sql` files (sqlc `postgresql` engine,
`$n` placeholders) as long as they stay within this subset.
