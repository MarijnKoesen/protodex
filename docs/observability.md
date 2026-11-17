# Observability

## Metrics

Prometheus text format at `GET /metrics` (unauthenticated; restrict at the network edge).

| Metric | Type | Labels | Description |
|---|---|---|---|
| `protodex_rpc_requests_total` | counter | `procedure`, `code` | Completed RPCs (`code` is the Connect code, `ok` on success). |
| `protodex_rpc_duration_seconds` | histogram | `procedure`, `code` | RPC latency. |
| `protodex_upload_bytes_total` | counter | – | Request bytes received on `UploadService` procedures. |
| `protodex_store_operation_duration_seconds` | histogram | `operation`, `status` | Metadata store latency (`status` = `ok`/`error`). |
| `protodex_blob_operation_duration_seconds` | histogram | `operation`, `status` | Blob storage latency. |
| `protodex_http_in_flight_requests` | gauge | – | HTTP requests being served. |
| `go_*`, `process_*` | – | – | Standard Go/process collectors. |

Store and blob code should wrap calls with `Observability.Store` / `Observability.Blob`,
which record the histogram and a span in one step.

## Tracing

Off by default. Enable with `tracing_enabled` (`PROTODEX_TRACING_ENABLED`, `-tracing`).
Spans are exported over OTLP/HTTP to `otlp_endpoint` (`PROTODEX_OTLP_ENDPOINT`,
`-otlp-endpoint`, e.g. `http://collector:4318`); if empty the standard
`OTEL_EXPORTER_OTLP_*` variables apply. Incoming W3C `traceparent` headers are honored.
Spans: one server span per RPC (named by procedure), plus `protodex.store.<op>` and
`protodex.blob.<op>`.

## Access logs

One structured line (`msg=access`) per HTTP request with `request_id`, `method`, `path`,
`status`, `bytes_out`, `duration`, `remote` and, for RPCs, `procedure` and `code`.
Headers, query strings and bodies are never logged, so tokens and file contents cannot leak.
`X-Request-Id` is accepted from clients if it matches `[A-Za-z0-9._-]{1,64}`; otherwise one is
generated. It is echoed in the response header and attached to spans.

## Dashboard

`deploy/grafana/protodex.json` — import into Grafana (Prometheus datasource variable `DS_PROMETHEUS`).
