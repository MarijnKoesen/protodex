# Blob garbage collection

File contents live in the blob store, shared across commits and modules.
Contents nobody references any more are left behind when a module is deleted
or when a push stores its blobs and then fails before committing. The blob
garbage collector removes them.

## What it does

Each run is a mark-and-sweep (`internal/gc`):

1. **Mark**: list the blob store and the blob rows, and read the set of
   digests that some commit manifest references.
2. **Sweep**: every unreferenced blob whose stored object is older than the
   grace period is claimed, re-checked against the database and only then
   deleted (the row first, then the object). Blob rows that have no stored
   object are swept without an age check. A deletion that fails is counted
   and retried on the next run.

## Concurrent pushes

A push may reuse content that already sits in the store as an old,
unreferenced orphan. Writing that blob is a no-op, so its age doesn't change,
and the commit that references it is written slightly later. To prevent the
collector from deleting the blob in that window, a push **pins** its blob
digests from before the blob writes until its metadata transaction has ended.
The collector skips pinned blobs, and a push waits while the collector holds
a claim on a digest it needs. Every new reference is created under a pin, so
a blob that is claimed and found unreferenced stays unreferenced until it is
deleted. The grace period is defence in depth.

## Configuration

| Key | Env | Default | |
|---|---|---|---|
| `gc_interval` | `PROTODEX_GC_INTERVAL` | `1h` | Time between runs; `0s` disables the collector. The first run happens one interval after start. |
| `gc_grace_period` | `PROTODEX_GC_GRACE_PERIOD` | `24h` | Minimum age of an unreferenced blob before deletion. |
| `gc_dry_run` | `PROTODEX_GC_DRY_RUN` | `false` | Log and count what would be deleted, without deleting it. |

Each run logs a summary (`blob gc finished`, component `gc`). In dry-run mode
each blob that would be deleted is logged at info level.

## Metrics

| Metric | Type | Labels | |
|---|---|---|---|
| `protodex_gc_runs_total` | counter | `result` (`ok`/`error`) | Completed runs. |
| `protodex_gc_duration_seconds` | histogram | – | Run duration. |
| `protodex_gc_last_success_timestamp_seconds` | gauge | – | Time of the last successful run. |
| `protodex_gc_blobs_total` | counter | `action` | Unreferenced blobs by outcome: `deleted`, `dry_run` (would be deleted), `in_flight` (spared: pinned by a push), `referenced` (spared: referenced again before deletion), `failed`. |
| `protodex_gc_bytes_total` | counter | `action` (`deleted`/`dry_run`) | Stored bytes of those blobs. |

## Deployment modes

In SQLite mode the collector runs inside `protodex serve`. The database's
process lock guarantees there is only one server, so the in-process pins
(`gc.Pins`) and an always-leading `gc.SingleProcess` leader are enough.

A multi-replica Postgres deployment will need two pieces, behind interfaces
that already exist:

- a `gc.Leader` (for example a Postgres advisory lock), so that only one
  replica, or a separate worker, collects;
- a database-backed `gc.Leases` shared by all replicas, because a push on one
  replica must be visible to the collector on another.

Collection needs a blob store that can list its contents (`blob.Lister`).
Only the filesystem store implements it so far; S3 listing will arrive with
the S3 wiring in `serve`.
