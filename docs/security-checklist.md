# Security review checklist

Run before each release (and for changes touching auth, parsing or storage).

## Code
- [ ] New RPCs are in the policy table (`internal/server/permissions.go`) and have a permission test.
- [ ] Every request-controlled size or count is bounded (see `docs/threat-model.md`).
- [ ] No secret (token, client secret) is logged or returned after creation.
- [ ] Parsers of untrusted input have a fuzz target; run `make fuzz` (or `go test -fuzz` per target, ≥ 1 min each).
- [ ] `go vet ./...`, `golangci-lint run` and `govulncheck ./...` are clean.

## Dependencies
- [ ] Dependabot PRs for Go modules, GitHub Actions and Docker are merged or consciously deferred.
- [ ] `govulncheck` shows no reachable vulnerabilities.

## Container and deployment
- [ ] Image is distroless static, digest pinned, runs as UID 65532 (`Dockerfile`).
- [ ] Helm/compose: non-root, read-only root filesystem, dropped capabilities, NetworkPolicy enabled where supported.
- [ ] TLS terminates at the proxy; `rate_limit.trust_forwarded_for` is set only if the proxy sets `X-Forwarded-For`.
- [ ] `/metrics` is not exposed publicly.

## Process
- [ ] `SECURITY.md` points to a working private reporting channel (GitHub private vulnerability reporting enabled in repository settings).
- [ ] Threat model updated.
- [ ] Before 1.0: external penetration test completed and findings triaged.
