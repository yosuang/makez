# Sandbox supervisor and execution contract

`cmd/sandboxd` runs the Go supervisor in `internal/sandbox/supervisor`. The Codex executable lives separately in `cmd/codex-runner` and `internal/agent/codexrunner`; neither the supervisor nor its protocol depends on Codex or OpenSandbox. Both binaries are built by the root Dockerfile.

Build from the repository root:

```sh
make image
make image-codex
MAKEZ_SANDBOX_INTEGRATION=1 go test ./internal/sandbox -run TestOpenSandboxIntegration -v
```

The Go process tests require an isolated Linux container because stopping the Agent intentionally terminates all processes owned by uid 1000:

```sh
mkdir -p .local
CGO_ENABLED=0 GOOS=linux go test -c -o .local/supervisor.test ./internal/sandbox/supervisor
docker run --rm --entrypoint /tests/supervisor.test \
  -e MAKEZ_SUPERVISOR_TEST=1 \
  -v "$PWD/.local/supervisor.test:/tests/supervisor.test:ro" \
  makez-runtime:0.1 -test.v
```

Drain unfinished tasks before switching from the old Python image to these binaries, then rebuild both images and restart API/Worker. Existing frozen plans may still name the old executable. No in-place upgrade of running sandboxes is attempted.

The integration test uses the local OpenSandbox service at `127.0.0.1:18080` and removes its sandbox. Set `OPEN_SANDBOX_URL` and `OPEN_SANDBOX_API_KEY` to override that local endpoint. The Go provider uses the native lifecycle API and its server proxy to reach the runtime's port 8099. OpenSandbox injects its own bootstrap/execd components; no E2B gateway is required.

The root supervisor launches argv directly as UID/GID 1000, with a clean environment and Linux `no_new_privs`. Prompts are files. Only the supervisor can write receipts and output. A per-sandbox token authenticates every control request. A launch key has one durable intent; ambiguous receipts are never launched again. Closing a key prevents delayed launch requests and stops the sandbox's exclusive Agent UID, including children that create new sessions. The absolute execution deadline also closes that key. The provider sets a finite, fixed sandbox TTL; this profile does not renew leases.

File operations reject traversal and symlink components with directory-relative `O_NOFOLLOW` opens. Input files are written atomically before Agent execution. Output is replayable until the sandbox expires; the worker must archive artifacts before deletion. Limits are 16 MiB per file and 64 MiB of raw JSON/base64 output spool; exceeding the spool returns `OUTPUT_GAP`.

The Worker resolves versioned, workspace-scoped SecretRefs. The provider accepts an exact mapping of references and ephemeral resolved values, checks for environment collisions, and transmits values only in the explicit launch payload. Ordinary JSON serialization, request hashes and runtime receipts exclude those values. Remote secret transport requires HTTPS; loopback HTTP remains available for local development. The runtime does not know which Agent or secret manager is used.

Injected values remain in supervisor memory for exact-literal inspection. For JSON secret values, the runtime also extracts nonempty string values under generic sensitive fields such as `access_token`, `refresh_token`, `id_token`, `api_key` and `password`, without recording ordinary metadata such as authentication mode. JSON depth, node and candidate limits fail closed. stdout and stderr are redacted **before** durable spooling, including values split across reads; only a possible secret prefix is buffered. A duplicate launch with the same references attaches to the original process and its original filtering context. Artifact reads containing an injected literal are rejected with `PERMISSION_DENIED`; the file is not silently modified. If the supervisor restarts and loses its in-memory values, artifact reads fail closed.

This first runtime is intended for local infrastructure testing. It contains the Go supervisor and Python as an Agent tool; custom CLI Agent executables must be installed in a derived image. Network allowlists, filesystem disk quotas, pause/resume and lease renewal are rejected as unsupported. OpenSandbox's create endpoint is not idempotent: the caller must persist its create intent, send once, and reconcile by ownership metadata after an uncertain response. The provider's in-process guard alone cannot survive a Worker restart.

Protocol references: [OpenSandbox lifecycle API](https://github.com/opensandbox-group/OpenSandbox/blob/main/specs/sandbox-lifecycle.yml), [architecture](https://github.com/opensandbox-group/OpenSandbox/blob/main/docs/architecture/index.md). Verified with OpenSandbox server 1.1.1.

Literal redaction does not detect encoded, transformed or fragmented credentials across different streams or artifacts, or new credentials generated/refreshed inside the Agent after launch. An Agent legitimately receives its credentials and can read its own environment; Agent-specific subprocess policies belong to its adapter. This protection does not replace trusting the configured Agent and controlling its network access.
