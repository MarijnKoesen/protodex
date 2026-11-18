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

`buf` only speaks TLS to registries (except for `localhost`), so put a TLS
proxy in front of any non-local deployment. Below, `localhost:8080` is used.

```sh
# Log in with the token from bootstrap.
buf registry login localhost:8080 --username admin --token-stdin <<<"$TOKEN"

# Create a module and push.
mkdir hello && cd hello
cat > buf.yaml <<'YAML'
version: v2
modules:
  - path: proto
    name: localhost:8080/acme/hello
YAML
mkdir proto && echo 'syntax = "proto3"; package hello.v1; message Ping {}' > proto/ping.proto
buf registry module create localhost:8080/acme/hello --visibility private
buf push
buf registry commit info localhost:8080/acme/hello:main
```

Consume it from another repository:

```yaml
# buf.yaml
version: v2
deps:
  - localhost:8080/acme/hello
```

then run `buf dep update`. Next: [migrating an existing repository](migration.md).
