#!/bin/sh
set -eu

# An open gRPC port does not yet mean the cluster can serve namespace requests.
attempt=0
until temporal operator cluster health --command-timeout 5s; do
  attempt=$((attempt + 1))
  if [ "$attempt" -ge 30 ]; then
    echo "Temporal did not become ready" >&2
    exit 1
  fi
  sleep 2
done

namespace=${TEMPORAL_NAMESPACE:-default}
if ! temporal operator namespace describe --namespace "$namespace" --command-timeout 5s >/dev/null 2>&1; then
  temporal operator namespace create --namespace "$namespace" --retention 72h --command-timeout 5s
fi
