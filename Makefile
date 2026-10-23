SHELL := /bin/bash

.PHONY: dev clean

dev:
	./scripts/dev.sh

clean:
	go run ./build clean

GOLANGCI_LINT_VERSION := $(shell cat .golangci-lint-version)
GOLANGCI_LINT ?= .local/bin/golangci-lint

.PHONY: lint-install lint-version-check lint fmt fmt-check
lint-install:
	@set -o pipefail; curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/$(GOLANGCI_LINT_VERSION)/install.sh | sh -s -- -b .local/bin $(GOLANGCI_LINT_VERSION)

lint-version-check:
	@test "v$$($(GOLANGCI_LINT) version --short)" = "$(GOLANGCI_LINT_VERSION)" || { echo "Run make lint-install to install $(GOLANGCI_LINT_VERSION)." >&2; exit 1; }

lint: lint-version-check
	$(GOLANGCI_LINT) config verify
	$(GOLANGCI_LINT) run

fmt: lint-version-check
	$(GOLANGCI_LINT) fmt

# fmt --diff prints changes but does not fail when formatting is needed.
fmt-check: lint-version-check
	@diff=$$(mktemp); trap 'rm -f "$$diff"' EXIT; \
	$(GOLANGCI_LINT) fmt --diff > "$$diff" && { cat "$$diff"; test ! -s "$$diff"; }

# Pin the generator independently of runtime packages; output has no timestamp.
SWAG = go run github.com/swaggo/swag/cmd/swag@v1.16.6
.PHONY: swagger swagger-check
swagger:
	$(SWAG) init -g internal/api/server.go --parseInternal --packageName docs --propertyStrategy pascalcase --output internal/api/docs --outputTypes go,json,yaml

swagger-check:
	@tmp=$$(mktemp -d); trap 'rm -rf "$$tmp"' EXIT; \
	$(SWAG) init -g internal/api/server.go --parseInternal --packageName docs --propertyStrategy pascalcase --output "$$tmp" --outputTypes go,json,yaml && \
	diff -ru internal/api/docs "$$tmp"
