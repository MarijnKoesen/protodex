# Operations

## State to protect

- the SQLite database (`db_path`, plus `-wal`/`-shm` files while running);
- the blob directory (`blob_dir`, default `<db_path>.blobs`).

Both are needed: the database references blobs by digest. Keep them on a
local block volume, never NFS/EFS (SQLite corruption risk).

## Backup and restore

- **Kubernetes (Helm)**: Litestream replicates the database to S3 and an init
  container restores it onto an empty volume. Follow the *Restore drill* in the
  [chart README](https://github.com/MarijnKoesen/protodex/blob/main/deploy/helm/protodex/README.md#restore-drill)
  regularly.
- **Compose / bare metal**: stop the server (or snapshot the volume) and copy
  the database and blob directory together; alternatively run
  `sqlite3 protodex.db ".backup backup.db"` against the live database and copy
  the blobs *after* the database so that every referenced blob exists. To
  restore, stop protodex, put both back, start it. Blobs without a database
  row are removed by [garbage collection](gc.md) after the grace period; a
  database row without a blob is a corrupt backup.

Test restores before you need them.

## Upgrades

1. Read the release notes.
2. Back up (above).
3. Stop the server: `migrate` needs the exclusive database lock.
4. Run `protodex migrate` when the release notes say the schema changed
   (compose: `docker compose run --rm --no-deps protodex migrate`). Roll back
   one step with `migrate down`.
5. Start the new image/chart. Helm uses `Recreate`, so expect a short
   downtime.

Protodex is a single-writer system; there is no rolling upgrade.

## Monitoring

`/healthz`, `/readyz`, Prometheus `/metrics` and the Grafana dashboard are
covered in [observability](observability.md). `/metrics` is unauthenticated:
restrict it at the network edge.
