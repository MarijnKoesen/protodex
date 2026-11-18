# Quickstart

You need Docker with Compose (or a Kubernetes cluster with Helm) and a `buf`
CLI of version 1.32.2 or newer (see [compatibility](compatibility.md)).

## Docker Compose

```sh
git clone https://github.com/MarijnKoesen/protodex && cd protodex
cp deploy/.env.example deploy/.env      # optional; change the secrets

# 1. Create the admin user, an organization and the first token.
#    The server must not be running: bootstrap takes the database lock.
docker compose -f deploy/docker-compose.yaml run --rm --no-deps protodex \
  bootstrap -org acme
# The token is printed ONCE. Save it.

# 2. Start the stack (protodex + SeaweedFS + SQLite volume).
docker compose -f deploy/docker-compose.yaml up -d --build
curl -fsS http://localhost:8080/healthz   # -> ok
```

`bootstrap` prints the admin token only once; to choose it yourself, set
`PROTODEX_BOOTSTRAP_TOKEN`. More settings are in
[`deploy/README.md`](https://github.com/MarijnKoesen/protodex/blob/main/deploy/README.md).

## Kubernetes (Helm)

```sh
helm install protodex oci://ghcr.io/marijnkoesen/charts/protodex \
  --version X.Y.Z -n protodex --create-namespace -f my-values.yaml
```

Run `protodex bootstrap -org acme` once inside the pod before first use
(scale to zero first, or run it as a Job on the same volume, because it needs
the database lock). Values, ingress, Litestream backups and the restore drill
are described in the
[chart README](https://github.com/MarijnKoesen/protodex/blob/main/deploy/helm/protodex/README.md).

## Push your first module

`buf` only speaks TLS to registries, including `localhost`, while protodex
serves plain HTTP (h2c). Pointing `buf` straight at `localhost:8080` fails
with `the server hosted at that remote is unavailable`. Put a TLS proxy in
front, also locally. For example, with [Caddy](https://caddyserver.com):

```sh
# Serves https://localhost:8443 with a certificate from Caddy's local CA.
# On first run Caddy asks for your password to add that CA to the system
# trust store, which buf uses (Go ignores SSL_CERT_FILE on macOS).
caddy reverse-proxy --from localhost:8443 --to localhost:8080
```

Below, `localhost:8443` is the proxy.

```sh
# Log in with the token printed by bootstrap.
buf registry login localhost:8443 --token-stdin <<<"$TOKEN"

# Create a module and push.
mkdir hello && cd hello
cat > buf.yaml <<'YAML'
version: v2
modules:
  - path: proto
    name: localhost:8443/acme/hello
YAML
mkdir proto && echo 'syntax = "proto3"; package hello.v1; message Ping {}' > proto/ping.proto
buf registry module create localhost:8443/acme/hello --visibility private
buf push
buf registry module label info localhost:8443/acme/hello:main   # shows the commit `main` points to
```

Consume it from another repository. `buf` needs at least one `.proto` file
of its own there, otherwise `buf dep update` fails with `had no .proto files`:

```sh
mkdir app && cd app
cat > buf.yaml <<'YAML'
version: v2
modules:
  - path: proto
deps:
  - localhost:8443/acme/hello
YAML
mkdir proto && cat > proto/app.proto <<'PROTO'
syntax = "proto3";
package app.v1;
import "ping.proto";
message Request { hello.v1.Ping ping = 1; }
PROTO
buf dep update   # writes buf.lock
buf build
```

Next: [migrating an existing repository](migration.md).
