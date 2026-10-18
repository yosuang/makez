SHELL := /bin/bash

.PHONY: dev infra-up infra-down infra-logs infra-ps test test-integration image image-codex build

dev:
	./scripts/dev.sh

infra-up:
	@source .local/env && docker volume create makez-postgres >/dev/null && docker compose up -d --wait --wait-timeout 90

infra-down:
	@source .local/env && docker compose down

infra-logs:
	@source .local/env && docker compose logs -f

infra-ps:
	@source .local/env && docker compose ps

build:
	go build ./cmd/makez

image:
	docker build --target sandbox -t makez-runtime:0.1 .

image-codex:
	docker build --target codex -t makez-runtime-codex:0.1 .

test:
	go test -race ./...

test-integration:
	MAKEZ_TEST_DATABASE_URL='postgres://makez:makez-local@127.0.0.1:55432/makez?sslmode=disable' go test -race ./internal/store ./internal/api ./internal/workflow
	MAKEZ_SANDBOX_INTEGRATION=1 go test ./internal/sandbox -run TestOpenSandboxIntegration -v
