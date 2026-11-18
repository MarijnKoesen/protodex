# SPDX-License-Identifier: Apache-2.0
#
# CI calls these same targets; keep them the single source of truth.

GO            ?= go
GOLANGCI_LINT ?= golangci-lint
BIN_DIR       ?= bin
VERSION       ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
IMAGE         ?= ghcr.io/marijnkoesen/protodex:dev

# Pinned tool versions (bumped deliberately, not via @latest).
GOLANGCI_LINT_VERSION ?= v2.15.0
GOVULNCHECK_VERSION   ?= v1.9.0
GO_LICENSES_VERSION   ?= v2.0.1
BUF_VERSION           ?= v1.57.0
PROTOC_GEN_GO_VERSION ?= v1.36.12
PROTOC_GEN_CONNECT_GO_VERSION ?= v1.21.1
SQLC_VERSION          ?= v1.30.0

# buf and the protoc plugins are built into $(TOOLS_DIR) at the pinned versions.
TOOLS_DIR ?= $(CURDIR)/bin/tools

# The race detector needs cgo + a C compiler; CI always has one.
RACE ?= $(shell CC=$$($(GO) env CC); command -v $$CC >/dev/null 2>&1 && echo -race)

LDFLAGS := -s -w -X github.com/marijnkoesen/protodex/internal/version.Version=$(VERSION)

.PHONY: all
all: lint test build

.PHONY: lint
lint: license-headers ## Run vet, golangci-lint and the SPDX header check
	$(GO) vet ./...
	$(GOLANGCI_LINT) run ./...

.PHONY: license-headers
license-headers:
	./scripts/check-license-headers.sh

.PHONY: test
test: ## Run unit tests with the race detector and coverage
	@if [ -z "$(RACE)" ]; then echo "warning: no C compiler found, running tests without -race" >&2; fi
	$(GO) test $(RACE) -covermode=atomic -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out | tail -n 1

.PHONY: build
build: ## Compile all packages and the protodex binary
	$(GO) build ./...
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/protodex ./cmd/protodex

.PHONY: generate-tools
generate-tools:
	GOBIN=$(TOOLS_DIR) $(GO) install github.com/bufbuild/buf/cmd/buf@$(BUF_VERSION)
	GOBIN=$(TOOLS_DIR) $(GO) install google.golang.org/protobuf/cmd/protoc-gen-go@$(PROTOC_GEN_GO_VERSION)
	GOBIN=$(TOOLS_DIR) $(GO) install connectrpc.com/connect/cmd/protoc-gen-connect-go@$(PROTOC_GEN_CONNECT_GO_VERSION)
	GOBIN=$(TOOLS_DIR) $(GO) install github.com/sqlc-dev/sqlc/cmd/sqlc@$(SQLC_VERSION)

.PHONY: sqlc
sqlc: generate-tools 
	rm -rf internal/store/sqlite/db
	PATH="$(TOOLS_DIR):$$PATH" sqlc generate

.PHONY: generate
generate: generate-tools ## Regenerate code (go generate + buf generate into gen/)
	$(GO) generate ./...
	$(MAKE) sqlc
	rm -rf gen
	PATH="$(TOOLS_DIR):$$PATH" buf generate proto

.PHONY: check-generated
check-generated: ## Fail if go mod tidy / generate leave a diff
	$(GO) mod tidy
	$(MAKE) generate
	@if [ -n "$$(git status --porcelain)" ]; then \
	  git status --short; git --no-pager diff; \
	  echo "generated files are stale: run 'go mod tidy && make generate' and commit" >&2; exit 1; \
	fi

.PHONY: licenses
licenses: ## Check dependency licenses are Apache-2.0 compatible
	$(GO) run github.com/google/go-licenses/v2@$(GO_LICENSES_VERSION) check ./... \
	  --allowed_licenses=Apache-2.0,BSD-2-Clause,BSD-3-Clause,MIT,ISC,MPL-2.0

.PHONY: vuln
vuln: ## Scan for known vulnerabilities
	$(GO) run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

.PHONY: fuzz
fuzz: ## Fuzz digest parsing (FUZZTIME=30s per target)
	go test ./internal/b5 -run '^$$' -fuzz FuzzDigest -fuzztime $${FUZZTIME:-30s}
	go test ./internal/b5 -run '^$$' -fuzz FuzzBytes -fuzztime $${FUZZTIME:-30s}

.PHONY: docker
docker: ## Build the container image (no push)
	docker build --build-arg VERSION=$(VERSION) \
	  --build-arg REVISION=$$(git rev-parse HEAD 2>/dev/null || echo unknown) \
	  -t $(IMAGE) .

.PHONY: compose-config
compose-config: ## Validate deploy/docker-compose.yaml (needs docker compose)
	docker compose -f deploy/docker-compose.yaml config -q

VERSIONS ?= $(shell cat test/capture/versions.txt)

.PHONY: capture
capture: ## Record buf CLI traffic per CLI version (needs docker + network)
	@for v in $(VERSIONS); do \
	  docker build -q -f test/capture/Dockerfile --build-arg BUF_VERSION=$$v -t protodex-capture:$$v . && \
	  mkdir -p test/capture/out && \
	  docker run --rm -e BUF_TOKEN -e PUSH_MODULE -v "$$PWD/test/capture/out:/out" protodex-capture:$$v || exit 1; \
	done
	$(MAKE) capture-matrix

.PHONY: capture-matrix
capture-matrix: ## Regenerate docs/behavior-spec/rpc-matrix.md from recordings
	@{ cat docs/behavior-spec/rpc-matrix.header.md; $(GO) run ./test/capture/cmd/capture matrix -dir test/capture/out; } > docs/behavior-spec/rpc-matrix.md

# buf CLI versions for `make conformance`: pr (the per-PR subset), all, or a
# comma-separated list. See docs/compatibility.md.
CONFORMANCE_VERSIONS ?= pr

.PHONY: conformance
conformance: ## Run the buf CLI conformance suite (downloads pinned, checksum-verified buf; CONFORMANCE_VERSIONS=pr|all|1.57.2,...)
	PROTODEX_CONFORMANCE_BUF_VERSIONS=$(CONFORMANCE_VERSIONS) PROTODEX_CONFORMANCE_REPORT=$(CURDIR)/conformance-report.md \
	  $(GO) test -count=1 -timeout 30m ./test/conformance/...
	@cat conformance-report.md

# Command for `make kind-e2e`: all (default), up, upgrade, conformance,
# restart, restore, diagnostics or down. See deploy/e2e/README.md.
E2E_ARGS ?= all

.PHONY: kind-e2e
kind-e2e: ## Helm chart e2e on kind: conformance, restart, Litestream restore, upgrade (needs docker, kind, kubectl, helm)
	./deploy/e2e/e2e.sh $(E2E_ARGS)

.PHONY: kind-e2e-down
kind-e2e-down: ## Delete the kind e2e cluster
	./deploy/e2e/e2e.sh down

.PHONY: tools
tools: ## Install golangci-lint at the pinned version
	$(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

.PHONY: clean
clean:
	rm -rf $(BIN_DIR) coverage.out conformance-report.md .e2e-kind

.PHONY: help
help:
	@grep -E '^[a-z0-9-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "%-18s %s\n", $$1, $$2}'

.PHONY: docs
docs: ## Build the docs site with strict link checking (pip: mkdocs, mkdocs-material)
	python3 -m venv $(BIN_DIR)/docs-venv
	$(BIN_DIR)/docs-venv/bin/pip install -q -r requirements-docs.txt
	$(BIN_DIR)/docs-venv/bin/mkdocs build --strict -d $(BIN_DIR)/site
