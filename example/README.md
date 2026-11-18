# Example: publish and consume a module

Two `buf` projects that use a local protodex as their registry:

| Project | What it does |
|---|---|
| [`hello/`](hello) | Publishes `localhost:8443/acme/hello`, with one message, `hello.v1.Ping`. |
| [`app/`](app) | Depends on `hello`, uses `Ping` in its own `app.v1.Request`, generates Go code for both and prints a message as JSON. |

`buf.lock` and `gen/` are not committed: you produce them with the commands
below.

## Prerequisites

- The compose stack running and bootstrapped with organization `acme`, plus a
  TLS proxy on `localhost:8443`. See the [quickstart](../docs/quickstart.md).
- [`buf`](https://buf.build/docs/installation), Go, and `protoc-gen-go`:

  ```sh
  go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
  ```

If your registry host or organization differs, change `localhost:8443/acme`
in both `buf.yaml` files.

## 1. Log in

```sh
buf registry login localhost:8443 --token-stdin <<<"$TOKEN"
```

`$TOKEN` is the token printed by `protodex bootstrap`.

## 2. Publish `hello`

```sh
cd example/hello
buf registry module create localhost:8443/acme/hello --visibility private   # once
buf push
buf registry module label info localhost:8443/acme/hello:main   # shows the commit `main` points to
```

## 3. Consume it from `app`

```sh
cd ../app
buf dep update   # resolves localhost:8443/acme/hello and writes buf.lock
buf build        # compiles and type-checks; prints nothing on success
buf generate     # writes gen/app/v1/app.pb.go and gen/hello/v1/ping.pb.go
go run .
```

Output:

```json
{"ping":{"message":"hello from protodex"}}
```

`buf build` only validates by default. To look at what it compiled:

```sh
buf build -o '-#format=json' | jq '.file[].name'   # hello/v1/ping.proto, app/v1/app.proto
buf export localhost:8443/acme/hello -o /tmp/hello   # the dependency's .proto sources
```

## How `app` generates code

[`app/buf.gen.yaml`](app/buf.gen.yaml) runs the local `protoc-gen-go` plugin:

- `include_imports: true` also generates the dependency (`hello/v1/ping.proto`),
  which only exists in the registry, not in this directory.
- Managed mode sets `go_package` from `go_package_prefix`
  (`example.com/app/gen`), so neither `.proto` file needs a Go option.
- `opt: module=example.com/app/gen` strips that prefix from the output paths,
  so the code lands in `gen/` and matches the `example.com/app` Go module.

## Iterate

Change `hello/proto/hello/v1/ping.proto`, `buf push` from `hello/`, then in
`app/` run `buf dep update` to pick up the new commit and `buf generate`
again.
