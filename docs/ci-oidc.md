# CI trust credentials: push from CI without long-lived tokens

A CI job can push to protodex without a registry token stored as a CI
secret. The job gets a short-lived **identity token** from its platform
(GitHub Actions OIDC, or a Kubernetes service-account token), and protodex
exchanges it for a short-lived **registry token** if the identity token
matches a **trust credential** configured for the module.

```
CI job ── identity token (JWT, aud=<registry>) ──▶ POST /auth/ci-token
       ◀── registry token (pdx_…, 15 min, one module) ──
       ── buf push with BUF_TOKEN=<registry token> ──▶ registry
```

A trust credential says: *tokens from issuer I, for audience A, whose claims
match these rules, may act as user U on module OWNER/MODULE with role R*.

- The minted token belongs to user **U** (a "service user", for example
  `ci-acme`). It never has more access than U has: give U a contributor
  grant (usually `write`) on the module.
- It is **restricted to that one module** and to the credential's role
  (`read` or `write`, never `admin`). Every other module it sees as an
  anonymous caller would (public modules only). It cannot create modules,
  change module settings, delete modules, manage tokens or call admin RPCs.
- It expires after the credential's TTL (default `ci_trust.token_ttl`,
  15 minutes; at most 1 hour). Expired CI tokens are deleted on later
  exchanges.

## Server configuration

The exchange endpoint is off by default. Enable it and say which issuers
the server trusts (it needs `db_path`):

```json
{
  "db_path": "protodex.db",
  "ci_trust": {
    "enabled": true,
    "github_issuer": "https://token.actions.githubusercontent.com",
    "kubernetes": [
      {"issuer": "https://oidc.eks.eu-west-1.amazonaws.com/id/EXAMPLE"},
      {"issuer": "https://kubernetes.default.svc.cluster.local",
       "jwks_url": "https://k8s-keys.internal.example.com/openid/v1/jwks"}
    ],
    "token_ttl": "15m"
  }
}
```

| Setting | Env var | Default |
|---------|---------|---------|
| `ci_trust.enabled` | `PROTODEX_CI_TRUST_ENABLED` | `false` |
| `ci_trust.github_issuer` (change only for GitHub Enterprise Server) | `PROTODEX_CI_TRUST_GITHUB_ISSUER` | `https://token.actions.githubusercontent.com` |
| `ci_trust.kubernetes[].issuer` | `PROTODEX_CI_TRUST_KUBERNETES_ISSUERS` (comma-separated, discovery only) | none |
| `ci_trust.kubernetes[].jwks_url` | config file only | OIDC discovery at the issuer |
| `ci_trust.token_ttl` | `PROTODEX_CI_TRUST_TOKEN_TTL` | `15m` (max `1h`) |

Keys are fetched from the issuer's OIDC discovery document
(`<issuer>/.well-known/openid-configuration`) or from `jwks_url`, cached,
and refetched when a token is signed with an unknown key. For Kubernetes,
the issuer must match the cluster's `--service-account-issuer` and its keys
must be reachable from protodex (managed clusters such as EKS and GKE
publish them; otherwise expose `/openid/v1/jwks` and set `jwks_url`).

With the Helm chart, set the env vars through `extraEnv`.

## Creating trust credentials

Credentials are managed on the server host, like owners and contributors
(the commands take the database lock, so stop the server first):

```sh
# The service user the CI tokens act as, with write access to the module.
protodex owner create -db protodex.db -role viewer user ci-acme
protodex contributor add -db protodex.db acme/weather ci-acme write

# GitHub Actions: pushes from the main branch of github.com/acme/weather.
protodex trust add -db protodex.db \
  -audience https://registry.example.com \
  -repository acme/weather \
  -ref refs/heads/main \
  weather-main acme/weather ci-acme

# Kubernetes: the "pusher" service account in namespace "ci", read only.
protodex trust add -db protodex.db -provider kubernetes \
  -issuer https://oidc.eks.eu-west-1.amazonaws.com/id/EXAMPLE \
  -audience https://registry.example.com \
  -subject system:serviceaccount:ci:pusher \
  -role read -ttl 5m \
  weather-k8s acme/weather ci-acme

protodex trust list   -db protodex.db [acme/weather]
protodex trust remove -db protodex.db weather-main
```

`owner create` prints an initial token for the service user; it is not
needed for CI and can be deleted.

Flags of `trust add`:

| Flag | Meaning |
|------|---------|
| `-provider` | `github` (default) or `kubernetes` |
| `-issuer` | the `iss` claim; defaults to the GitHub Actions issuer for `github`, required for `kubernetes`. The server must trust it (see above). |
| `-audience` | required `aud` claim. Use something unique to this registry, such as its URL, so tokens requested for other services are useless here. |
| `-repository` | `github`: the exact `repository` claim, `OWNER/REPO` (**required**, no wildcards) |
| `-subject` | pattern for the `sub` claim (**required** for `kubernetes`: `system:serviceaccount:NAMESPACE:NAME`) |
| `-ref` | `github`: pattern for the `ref` claim, e.g. `refs/heads/main`, `refs/tags/v*` |
| `-role` | `write` (default) or `read` |
| `-ttl` | lifetime of minted tokens (whole seconds, `1s` to `1h`); default: the server's `ci_trust.token_ttl` |

Rules fail closed: every configured rule must match, a missing claim never
matches, a GitHub credential must pin the repository and a Kubernetes
credential the subject, and patterns that match everything are refused.
In patterns `*` matches any run of characters (including `/` and `:`), `?`
exactly one character; everything else is literal and matching is
case-sensitive. Useful GitHub `sub` values (see GitHub's documentation on
OIDC claims) are `repo:OWNER/REPO:ref:refs/heads/main`,
`repo:OWNER/REPO:environment:production` and
`repo:OWNER/REPO:pull_request`.

## GitHub Actions workflow

The job needs `permissions: id-token: write` to request an identity token.

```yaml
name: push protos
on:
  push:
    branches: [main]

permissions:
  contents: read
  id-token: write   # lets the job request an OIDC identity token

env:
  REGISTRY: https://registry.example.com
  MODULE: acme/weather

jobs:
  push:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: bufbuild/buf-action@v1
        with:
          setup_only: true
      - name: Get a registry token
        id: registry
        run: |
          set -euo pipefail
          # 1. Ask GitHub for an identity token for our registry audience.
          id_token=$(curl -sSf -H "Authorization: bearer $ACTIONS_ID_TOKEN_REQUEST_TOKEN" \
            "$ACTIONS_ID_TOKEN_REQUEST_URL&audience=$REGISTRY" | jq -r .value)
          # 2. Exchange it for a short-lived registry token.
          token=$(jq -n --arg t "$id_token" --arg m "$MODULE" '{token: $t, module: $m}' |
            curl -sSf -X POST -H 'Content-Type: application/json' --data @- \
              "$REGISTRY/auth/ci-token" | jq -r .token)
          echo "::add-mask::$token"
          echo "token=$token" >> "$GITHUB_OUTPUT"
      - name: Push
        env:
          BUF_TOKEN: ${{ steps.registry.outputs.token }}
        run: buf push
```

`buf.yaml` names the module on this registry, e.g.
`name: registry.example.com/acme/weather`. If `buf` talks to several
registries, use `BUF_TOKEN=<token>@registry.example.com`.

## Kubernetes

Mount a projected service-account token with the registry audience and
exchange it the same way:

```yaml
spec:
  serviceAccountName: pusher
  containers:
    - name: push
      image: bufbuild/buf
      volumeMounts:
        - name: registry-id
          mountPath: /var/run/secrets/registry
  volumes:
    - name: registry-id
      projected:
        sources:
          - serviceAccountToken:
              path: token
              audience: https://registry.example.com
              expirationSeconds: 600
```

```sh
curl -sSf -X POST -H 'Content-Type: application/json' \
  -d "{\"token\":\"$(cat /var/run/secrets/registry/token)\",\"module\":\"acme/weather\"}" \
  https://registry.example.com/auth/ci-token
```

## Exchange API

`POST /auth/ci-token` with a JSON body:

```json
{"token": "<identity token (JWT)>", "module": "acme/weather"}
```

On success (`200`):

```json
{"token": "pdx_…", "expires_at": "2026-11-17T10:15:00Z", "module": "acme/weather", "role": "write"}
```

| Status | Meaning |
|--------|---------|
| `400` | malformed body |
| `401` | `{"error": "token exchange denied"}`: whatever the reason |
| `429` | rate limited: per client address, a burst of 30 requests, then 1 per second |

The client is never told *why* an exchange was denied (unknown module, no
credential, untrusted issuer, bad signature, wrong audience, expired token,
or which claim rule failed). The server logs the reason at `WARN`
(`ci token exchange denied`) and each issued token at `INFO`
(`ci token issued`, with credential, `sub`, repository and ref). The
minted token appears among the service user's tokens as `ci:<credential>`.

## Security notes

- **Pin tightly.** Pin the repository and, for pushes, the branch or tag
  (`-ref`) or a protected GitHub environment (`-subject
  repo:OWNER/REPO:environment:production`). Without a ref rule, any branch
  of the repository, including unreviewed ones, can push. Workflows triggered
  by pull requests from forks get no identity token from GitHub.
- **Use a dedicated service user** with only the grants CI needs, rather
  than a person's account; the minted token is capped by the user's own
  permission *and* the credential's role.
- **Unique audience.** Tokens for another audience are rejected, so an
  identity token the job obtained for a different service cannot be
  replayed here.
- **Short lifetimes.** Identity tokens are verified for signature, issuer,
  audience and expiry. Registry tokens live 15 minutes by default (1 hour at
  most). An identity token can be exchanged more than once until it expires,
  so keep its lifetime short too (Kubernetes `expirationSeconds`).
- **Revocation.** `protodex trust remove` stops new exchanges immediately;
  already minted tokens remain valid until they expire. Deleting the module
  or the service user revokes them as well.
- Only issuers listed in the server configuration are contacted, so adding a
  credential cannot make the server fetch keys from arbitrary URLs.
