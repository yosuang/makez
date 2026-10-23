#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

for dependency in go docker jq openssl pnpm; do
  command -v "$dependency" >/dev/null || {
    echo "Missing dependency: $dependency" >&2
    exit 1
  }
done
pnpm --dir apps/web install --frozen-lockfile
docker compose version >/dev/null
compose=(docker compose -f deploy/docker-compose.infra.yaml)

umask 077
mkdir -p .local
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
# This is a shell export file, not a Compose env_file.
# shellcheck disable=SC1091
source .local/env

# Enable development features unless the shell or .local/env explicitly sets a mode.
export MAKEZ_ENV="${MAKEZ_ENV-development}"

if [[ -n "${MAKEZ_CODEX_AGENT:-}" ]]; then
  export MAKEZ_RUNTIME_IMAGE="${MAKEZ_RUNTIME_IMAGE:-makez-runtime-codex:0.1}"
else
  export MAKEZ_RUNTIME_IMAGE="${MAKEZ_RUNTIME_IMAGE:-makez-runtime:0.1}"
fi

# Compose creates its development volume; existing external/SQLite data is untouched.
"${compose[@]}" up -d --wait --wait-timeout 180
docker build --target sandbox -t makez-runtime:0.1 .
if [[ "$MAKEZ_RUNTIME_IMAGE" == makez-runtime-codex:0.1 ]]; then
  docker build --target codex -t makez-runtime-codex:0.1 .
fi
docker image inspect "$MAKEZ_RUNTIME_IMAGE" >/dev/null
go run ./build bin/makez

printf '\nGo API: http://%s\nAPI Key: %s\n' "${MAKEZ_LISTEN:-127.0.0.1:8088}" "$(jq -er 'keys[0]' <<< "$MAKEZ_API_KEYS")"
printf 'Web: http://127.0.0.1:3000\n'
printf 'Temporal: http://127.0.0.1:18233\nCtrl+C stops makez and Web. Infrastructure stays running; use docker compose -f deploy/docker-compose.infra.yaml down to stop it (source .local/env first).\n\n'

# Separate process groups let cleanup also stop pnpm's child processes.
set -m
pids=()
cleanup() {
  trap '' INT TERM
  for pid in "${pids[@]}"; do
    kill -TERM -- "-$pid" 2>/dev/null || true
  done
  wait || true
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
bin/makez all </dev/null &
pids+=("$!")
pnpm --dir apps/web dev </dev/null &
pids+=("$!")

# macOS ships Bash 3.2, which has no wait -n.
while true; do
  for pid in "${pids[@]}"; do
    if ! kill -0 "$pid" 2>/dev/null; then
      wait "$pid"
      exit 0
    fi
  done
  sleep 0.5
done
