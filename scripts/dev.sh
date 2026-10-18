#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

for dependency in go docker jq openssl; do
  command -v "$dependency" >/dev/null || {
    echo "Missing dependency: $dependency" >&2
    exit 1
  }
done
docker compose version >/dev/null

if [[ -e .local/temporal.db || -e .local/opensandbox.db ]]; then
  echo "Move legacy SQLite data before starting Compose; see docs/implementation-v0.1.md (旧开发环境迁移)." >&2
  exit 1
fi

umask 077
mkdir -p .local/temporal .local/opensandbox
if [[ ! -f .local/env ]]; then
  workspace_key=$(openssl rand -hex 24)
  sandbox_key=$(openssl rand -hex 24)
  control_key=$(openssl rand -hex 32)
  {
    printf 'export MAKEZ_API_KEYS=%q\n' "$(jq -cn --arg key "$workspace_key" '{($key): "local"}')"
    printf 'export MAKEZ_SANDBOX_API_KEY=%q\n' "$sandbox_key"
    printf 'export MAKEZ_CONTROL_KEY=%q\n' "$control_key"
  } > .local/env
fi
source .local/env

if [[ -n "${MAKEZ_CODEX_AGENT:-}" ]]; then
  export MAKEZ_RUNTIME_IMAGE="${MAKEZ_RUNTIME_IMAGE:-makez-runtime-codex:0.1}"
else
  export MAKEZ_RUNTIME_IMAGE="${MAKEZ_RUNTIME_IMAGE:-makez-runtime:0.1}"
fi

docker volume create makez-postgres >/dev/null
docker compose up -d --wait --wait-timeout 90
docker build --target sandbox -t makez-runtime:0.1 .
if [[ "$MAKEZ_RUNTIME_IMAGE" == makez-runtime-codex:0.1 ]]; then
  docker build --target codex -t makez-runtime-codex:0.1 .
fi
docker image inspect "$MAKEZ_RUNTIME_IMAGE" >/dev/null
go build -o .local/makez ./cmd/makez

printf '\nConsole: http://%s\nAPI Key: %s\n' "${MAKEZ_LISTEN:-127.0.0.1:8088}" "$(jq -er 'keys[0]' <<< "$MAKEZ_API_KEYS")"
printf 'Temporal: http://127.0.0.1:18233\nCtrl+C stops makez. Infrastructure stays running; use make infra-down to stop it.\n\n'
exec .local/makez all
