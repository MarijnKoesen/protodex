# Configuration

Settings are layered; later wins: built-in defaults, JSON config file
(`-config` / `PROTODEX_CONFIG`), `PROTODEX_*` environment variables, flags of
`protodex serve`. Unknown JSON keys are rejected.

| Key | Env | Default | Description |
|---|---|---|---|
| `addr` | `PROTODEX_ADDR` | `:8080` | Listen address (h2c; terminate TLS at a proxy). |
| `log_level` | `PROTODEX_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error`. |
| `log_format` | `PROTODEX_LOG_FORMAT` | `text` | `text` or `json`. |
| `max_request_bytes` | `PROTODEX_MAX_REQUEST_BYTES` | 64 MiB | Upper bound for a request body. |
| `db_path` | `PROTODEX_DB_PATH` | | SQLite database. **Required** for registry RPCs. Holds an exclusive lock: one server per database. |
| `blob_dir` | `PROTODEX_BLOB_DIR` | `<db_path>.blobs` | Blob directory (filesystem backend). |
| `max_push_files`, `max_push_file_bytes`, `max_push_bytes` | `PROTODEX_MAX_PUSH_BYTES` | 5000 / 10 MiB / 64 MiB | Push limits, see [push](push.md). |
| `rate_limit.*` | `PROTODEX_RATE_LIMIT_*` | 50/s per IP, 100/s per token | Rate limiting; `0` disables. `trust_forwarded_for` only behind a trusted proxy. |
| `gc_interval`, `gc_grace_period`, `gc_dry_run` | `PROTODEX_GC_*` | `1h`, `24h`, `false` | See [garbage collection](gc.md). |
| `shutdown_timeout` | `PROTODEX_SHUTDOWN_TIMEOUT` | `30s` | Grace for in-flight requests on SIGTERM. |
| `tracing_enabled`, `otlp_endpoint` | `PROTODEX_TRACING_ENABLED`, `PROTODEX_OTLP_ENDPOINT` | off | OpenTelemetry tracing, see [observability](observability.md). |
| `oidc.*` | `PROTODEX_OIDC_*` | off | Single sign-on; see the [README](https://github.com/MarijnKoesen/protodex#oidc-login) and [permissions](permissions.md). The client secret is env-only. |

The authoritative list is `internal/config/config.go`; the compose and Helm
settings are in the `deploy/` READMEs.

## CLI commands

| Command | Purpose |
|---|---|
| `protodex serve` | Run the server. |
| `protodex bootstrap` | Create the first admin, instance organization and mirror owners; prints the first token once. |
| `protodex migrate [up\|down]` | Apply or roll back database migrations. Run after upgrading. |
| `protodex owner`, `protodex contributor` | Manage users, organizations, roles and per-module grants ([owners](owners.md)). |
| `protodex mirror` | Import upstream modules ([mirroring](mirror.md)). |
| `protodex migrate-buf` | Rewrite a repository's `buf.yaml` hosts ([migration](migration.md)). |
