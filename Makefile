# SPDX-License-Identifier: Apache-2.0
#
# CI calls these same targets; keep them the single source of truth.

GO            ?= go
GOLANGCI_LINT ?= golangci-lint
BIN_DIR       ?= bin
VERSION       ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
IMAGE         ?= ghcr.io/marijnkoesen/openbuf:dev

# Pinned tool versions (bumped deliberately, not via @latest).
GOLANGCI_LINT_VERSION ?= v2.15.0
GOVULNCHECK_VERSION   ?= v1.9.0
GO_LICENSES_VERSION   ?= v2.0.1

# The race detector needs cgo + a C compiler; CI always has one.
RACE ?= $(shell CC=$$($(GO) env CC); command -v $$CC >/dev/null 2>&1 && echo -race)

LDFLAGS := -s -w -X github.com/MarijnKoesen/openbuf/internal/version.Version=$(VERSION)

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
build: ## Compile all packages and the openbuf binary
	$(GO) build ./...
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/openbuf ./cmd/openbuf

.PHONY: generate
generate: ## Regenerate code (go generate; buf generate once protos exist)
	$(GO) generate ./...
	@if [ -f buf.gen.yaml ]; then buf generate; fi

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

.PHONY: docker
docker: ## Build the container image (no push)
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE) .

.PHONY: tools
tools: ## Install golangci-lint at the pinned version
	$(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

.PHONY: clean
clean:
	rm -rf $(BIN_DIR) coverage.out

.PHONY: help
help:
	@grep -E '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "%-18s %s\n", $$1, $$2}'
