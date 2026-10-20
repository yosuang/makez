<div align="center">
  <h1>makez</h1>
  <p><b>Make building easy.</b> An AI-powered development platform for turning ideas into products.</p>
</div>

## Direction

- **Agent-agnostic** — Integrate existing and custom agents without depending on a single agent implementation.
- **Cloud execution** — Run agents beyond the constraints of local computing resources.
- **Creation first** — Build around product development and tangible outcomes.

## Quickstart

Host tools: Go 1.27.2+, Docker with Compose, jq, OpenSSL, and pnpm 12.14.0. The Web app needs Node.js 22.18+, 24.11+, or 26+.

```bash
# Go API, execution worker, Web, and local infrastructure
make dev
```

Open **http://127.0.0.1:3000** and connect with the workspace API key printed by `make dev`.

`make dev` runs `dev/dev.sh`. It installs Web dependencies from the lockfile, writes `.local/env` on first run (`MAKEZ_API_KEYS`, `MAKEZ_SANDBOX_API_KEY`, `MAKEZ_CONTROL_KEY`), creates the external `makez-postgres` volume, runs `docker compose -f dev/docker-compose.yaml up -d --wait`, and builds `makez-runtime:0.1`. It then starts `bin/makez all` and the Web dev server together. If either exits, the script stops the other. Ctrl+C stops Go and Web. Compose keeps running. `make clean` deletes build outputs only; it does not stop Compose or delete `.local`.

| Service | Address |
| --- | --- |
| Go API | http://127.0.0.1:8088 |
| Web | http://127.0.0.1:3000 |
| Temporal gRPC / UI | 127.0.0.1:17233 / http://127.0.0.1:18233 |
| OpenSandbox | http://127.0.0.1:18080 |
| PostgreSQL | 127.0.0.1:55432 |

After the first run, `source .local/env`, then `docker compose -f dev/docker-compose.yaml up -d --wait --wait-timeout 90`, `docker compose -f dev/docker-compose.yaml ps`, `docker compose -f dev/docker-compose.yaml logs -f`, or `docker compose -f dev/docker-compose.yaml down`. `down` removes containers and keeps the Postgres volume and `.local` data.

`bin/makez` takes `all` (default), `api`, or `worker`. Outbox dispatch and reconcile run only in the API process (`all` and `api`; reconcile runs every 15th dispatch tick). `worker` executes Temporal workflows and does not deliver the outbox or reconcile. The API uses a lazy Temporal client, so it can persist tasks while Temporal is down; they stay queued until an API process delivers them.

If `8088` is in use, stop the other listener or set `MAKEZ_LISTEN` and the same origin in `apps/web/.dev.vars` as `MAKEZ_API_ORIGIN`. Web setup: [apps/web/README.md](apps/web/README.md).

Unset settings default to database `postgres://makez:makez-local@127.0.0.1:55432/makez?sslmode=disable`, Temporal `127.0.0.1:17233`, OpenSandbox `http://127.0.0.1:18080`, and artifacts `.local/artifacts`.

## Codex credentials

`demo@1` needs no model credentials. To enable Codex, add the exports below to `.local/env` (sourced by `make dev`) or export them in the shell first. When `MAKEZ_CODEX_AGENT` is set, `make dev` builds and uses `makez-runtime-codex:0.1` (`@openai/codex@0.163.0`).

`MAKEZ_SECRETS_FILE` maps a workspace to versioned locations, never values: `name@version` → `{"env":"VAR"}` or `{"file":"/absolute/path"}`. A file source must be a regular file mode `0600` or stricter. The API only checks that the workspace may use the reference; the worker reads the value when the run starts.

API key. Keep `MAKEZ_OPENAI_API_KEY` in the private environment, not in git:

```json
{ "local": { "openai-key@1": { "env": "MAKEZ_OPENAI_API_KEY" } } }
```

```sh
export MAKEZ_SECRETS_FILE="$PWD/.local/secrets.json"
export MAKEZ_CODEX_AGENT='{"AuthMode":"api_key","SecretRef":"openai-key@1"}'
```

ChatGPT session. Snapshot the host Codex auth file without its refresh token, mode `0600`, and register that absolute path as `codex-session@1` with `{"file":"..."}`:

```sh
mkdir -p .local
umask 077
jq '{auth_mode:"chatgpt", OPENAI_API_KEY:null,
     tokens:{access_token:.tokens.access_token, id_token:.tokens.id_token,
             account_id:.tokens.account_id, refresh_token:""},
     last_refresh:.last_refresh}' \
  "${CODEX_HOME:-$HOME/.codex}/auth.json" > .local/codex-session-v1.json
```

```sh
export MAKEZ_CODEX_AGENT='{"AuthMode":"chatgpt_session","SecretRef":"codex-session@1"}'
```

`AuthMode` may also be `codex_access_token`. Do not put secret values in task JSON. Another CLI uses `MAKEZ_CLI_AGENT` (`ID`, `Version`, `Argv`, `Env`) and `MAKEZ_RUNTIME_IMAGE`.

## Tests

```sh
go build ./...
go vet ./...
go test ./...
```

PostgreSQL tests need the local database:

```sh
MAKEZ_TEST_DATABASE_URL='postgres://makez:makez-local@127.0.0.1:55432/makez?sslmode=disable' \
  go test ./internal/store ./internal/api ./internal/workflow
```

OpenSandbox integration needs the local server and `makez-runtime:0.1`. After `source .local/env`:

```sh
OPEN_SANDBOX_API_KEY="$MAKEZ_SANDBOX_API_KEY" MAKEZ_SANDBOX_INTEGRATION=1 \
  go test ./internal/sandbox -run TestOpenSandboxIntegration -v
```

Supervisor process tests stop every process owned by uid 1000, so plain `go test` skips them. Run the compiled test as root inside the runtime image:

```sh
mkdir -p .local
CGO_ENABLED=0 GOOS=linux go test -c -o .local/supervisor.test ./internal/sandbox/supervisor
docker run --rm --entrypoint /tests/supervisor.test \
  -e MAKEZ_SUPERVISOR_TEST=1 \
  -v "$PWD/.local/supervisor.test:/tests/supervisor.test:ro" \
  makez-runtime:0.1 -test.v
```

Web: `pnpm --dir apps/web test` and `pnpm --dir apps/web check`.
