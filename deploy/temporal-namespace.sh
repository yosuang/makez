#!/bin/sh
set -eu

namespace=${TEMPORAL_NAMESPACE:-default}
# The server's port healthcheck precedes this job. Wait for cluster readiness
# and retry registration without masking a final failure.
attempt=0
while [ "$attempt" -lt 30 ]; do
  if temporal operator cluster health --command-timeout 5s; then
    if temporal operator namespace describe --namespace "$namespace" --command-timeout 5s >/dev/null 2>&1; then
      exit 0
    fi
    if temporal operator namespace create --namespace "$namespace" --retention 72h --command-timeout 5s; then
      # Verify the namespace is visible before host workers or the UI start.
      temporal operator namespace describe --namespace "$namespace" --command-timeout 5s >/dev/null && exit 0
    fi
  fi
  attempt=$((attempt + 1))
  sleep 2
done
echo "Temporal namespace initialization did not become ready" >&2
exit 1
