# makez

AI development platform: submit a task, run an agent in a sandbox, and keep the artifacts.

## Layout

- `cmd/makez` — API and Temporal worker (`all`, `api`, `worker`). `cmd/sandboxd` supervises a sandbox. `cmd/codex-runner` launches Codex.
- `internal/core` — states, provider, and adapter contracts. `internal/api` — HTTP. `internal/store` — PostgreSQL. `internal/workflow` — workflows, activities, outbox dispatch, and reconcile.
- `internal/sandbox` — OpenSandbox provider and `supervisor`. `internal/agent` — demo, CLI, and Codex adapters. `internal/secret` — SecretRefs. `internal/artifact` — local archive.
- `apps/web` — TanStack Start console. `build/` — `dev.sh` and packaging. `configs/` — OpenSandbox. Root: `compose.yaml`, `Dockerfile`, `Makefile`.

## Commands

- `make dev` — Compose, runtime image, then `bin/makez all`. Ctrl+C stops makez; `docker compose down` stops infrastructure.
- `go build ./...`, `go vet ./...`, `go test ./...`. PostgreSQL, OpenSandbox, and in-container supervisor tests are in `README.md`.
- `golangci-lint run` — config is `.golangci.yml`.
- Web: `pnpm --dir apps/web check` and `pnpm --dir apps/web test`.

## Conventions

- One task has one run (`makez_runs.task_id` is unique). Change a run state only through `allowedRunTransition`. Terminal task states are `SUCCEEDED`, `FAILED`, and `CANCELED`.
- Accept a task and enqueue its outbox row in the same transaction. The same idempotency key and body returns 200; a different body returns 409. Outbox rows are unique on `(task_id, command)`.
- Sandbox create and agent start record a `DISPATCHING` intent first. Reconcile an uncertain result; do not send it again. `SecretValues` are `json:"-"` and must stay out of hashes, Temporal history, and logs. Redact injected literals before spooling output, and reject artifacts that contain them.
- `Dispatch` (outbox delivery and reconcile) runs only in `all` and `api`. `worker` does not deliver or reconcile.

## Repository

Do not add design docs, implementation plans, or dev logs. When behavior changes, update the code, tests, and code comments, and keep `README.md` and `AGENTS.md` accurate. Keep private notes out of this public repo.
