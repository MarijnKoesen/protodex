# Security notes

- **TLS**: Protodex speaks plain HTTP/h2c. Terminate TLS at a reverse proxy or
  ingress; `buf` refuses non-TLS registries other than localhost. Set
  `rate_limit.trust_forwarded_for` only if your proxy sets `X-Forwarded-For`.
- **Tokens**: bearer tokens are shown once and stored hashed. Treat the
  bootstrap token like a root credential; rotate by creating a new token and
  deleting the old one. Roles and visibility are described in
  [permissions](permissions.md).
- **OIDC**: users are keyed by the stable `(issuer, subject)` pair, never by
  e-mail.
- **Network exposure**: do not expose `/metrics` publicly. The compose file
  binds to 127.0.0.1 by default.
- **Container**: distroless, non-root (65532), read-only root filesystem.
  Images and charts are cosign-signed; verification steps are in the
  [deploy README](https://github.com/MarijnKoesen/protodex/blob/main/deploy/README.md#releases).
- **Supply chain / limits**: push size limits and rate limits bound abuse; see
  the [threat model](threat-model.md) and the
  [release checklist](security-checklist.md).

Report vulnerabilities privately as described in
[SECURITY.md](https://github.com/MarijnKoesen/protodex/blob/main/SECURITY.md).
