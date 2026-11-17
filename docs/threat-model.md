# Threat model

Scope: a single `protodex serve` process (SQLite metadata, filesystem or S3
blobs) behind a TLS-terminating reverse proxy, used through `buf` and the web UI.

## Assets
- Schema contents of private modules; token secrets; OIDC client secret.
- Integrity of commits and their b5 digests (consumers pin them).
- Availability of the registry and its storage.

## Trust boundaries
| Boundary | Notes |
|---|---|
| Internet/clients → proxy → protodex | TLS is terminated by the proxy; protodex speaks plain HTTP/h2c. |
| Anonymous vs. authenticated | Anonymous callers may read public modules only (`docs/permissions.md`). |
| protodex → blob store / IdP | Credentials come from the environment; the IdP is trusted for identity and groups. |

## Threats and mitigations
| Threat | Mitigation | Status |
|---|---|---|
| Token brute force / credential stuffing | 256-bit random secrets, stored hashed, constant-time compare; per-IP and per-token rate limits | done |
| Request flooding / scraping of anonymous reads | Per-IP token bucket on every request (`rate_limit.ip_*`) | done |
| Oversized or many-file pushes | `max_request_bytes`, `max_push_files`, `max_push_file_bytes`, `max_push_bytes` | done |
| Slowloris / idle connections | `ReadHeaderTimeout`, `IdleTimeout`, `MaxHeaderBytes` | done |
| Malicious paths in pushed files (traversal) | Paths must be clean, relative, slash-separated; blobs are content-addressed, never named by clients | done |
| Digest/manifest parser bugs | Go fuzz targets `FuzzDigest`, `FuzzBytes` in `internal/b5` | done |
| Privilege escalation across modules/owners | Role + visibility + contributor checks in the interceptor chain; tests in `internal/server/permissions_test.go` | done |
| Spoofed client IP to evade limits | `X-Forwarded-For` ignored unless `rate_limit.trust_forwarded_for`; then only the last (proxy-appended) entry is used | done |
| Storage exhaustion by one owner | Per-push byte cap only; no per-owner storage quota yet | **open** |
| Distributed floods | Limits are per client and per process; use proxy/WAF limits for volumetric attacks | out of scope |
| Compromised dependency / image | Dependabot, digest-pinned distroless nonroot image, signed releases | see checklist |

## Residual risks / non-goals
- Rate limiter state is in memory and per replica; restarts reset it.
- Clients behind one NAT/proxy share an IP bucket; tune `rate_limit.ip_rate`.
- No external penetration test has been done; schedule one before 1.0.
