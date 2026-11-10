-- name: InsertPersonalSession :one
INSERT INTO makez_native_sessions (
  id, workspace_id, scope, bot_id, conversation_id, harness_id, harness_pin
) VALUES ($1, $2, 'personal', $3, $4, $5, $6)
ON CONFLICT (workspace_id, bot_id, conversation_id, harness_id, harness_pin) WHERE scope = 'personal'
DO UPDATE SET updated_at = now()
RETURNING id, last_disposition, last_prompt_sha256, COALESCE(octet_length(checkpoint), 0)::bigint AS checkpoint_bytes;

-- name: InsertWorkerSession :one
INSERT INTO makez_native_sessions (
  id, workspace_id, scope, delegation_run_id, harness_id, harness_pin
) VALUES ($1, $2, 'worker', $3, $4, $5)
ON CONFLICT (workspace_id, delegation_run_id, harness_id, harness_pin) WHERE scope = 'worker'
DO UPDATE SET updated_at = now()
RETURNING id, last_disposition, last_prompt_sha256, COALESCE(octet_length(checkpoint), 0)::bigint AS checkpoint_bytes;

-- name: LockNativeSession :one
SELECT id, last_disposition, last_prompt_sha256, COALESCE(octet_length(checkpoint), 0)::bigint AS checkpoint_bytes
FROM makez_native_sessions
WHERE id = $1 AND workspace_id = $2
FOR UPDATE;

-- name: NativeCheckpoint :one
SELECT checkpoint
FROM makez_native_sessions
WHERE id = $1 AND workspace_id = $2 AND checkpoint IS NOT NULL AND octet_length(checkpoint) > 0;

-- name: SaveNativeCheckpoint :execrows
UPDATE makez_native_sessions
SET checkpoint = sqlc.arg(checkpoint)::bytea, updated_at = now()
WHERE id = sqlc.arg(id) AND workspace_id = sqlc.arg(workspace_id)
  AND octet_length(sqlc.arg(checkpoint)::bytea) BETWEEN 1 AND 1048576;

-- name: SaveNativeDisposition :execrows
UPDATE makez_native_sessions
SET last_disposition = $3, last_prompt_sha256 = $4, updated_at = now()
WHERE id = $1 AND workspace_id = $2;

-- name: LockLatestAttempt :one
SELECT session_id, generation, step, state, disposition, prompt_sha256
FROM makez_attempts
WHERE workspace_id = $1 AND run_id = $2
ORDER BY generation DESC
LIMIT 1
FOR UPDATE;

-- name: InsertAttempt :execrows
INSERT INTO makez_attempts (
  workspace_id, run_id, session_id, generation, step, state, prompt_sha256
) VALUES ($1, $2, $3, $4, $5, 'open', $6);

-- name: SaveAttemptOutcome :execrows
UPDATE makez_attempts
SET disposition = sqlc.arg(disposition),
    state = CASE WHEN sqlc.arg(state)::text = '' THEN state ELSE sqlc.arg(state) END,
    updated_at = now()
WHERE workspace_id = sqlc.arg(workspace_id) AND run_id = sqlc.arg(run_id) AND generation = sqlc.arg(generation);

-- name: SaveAttemptState :execrows
UPDATE makez_attempts
SET state = CASE WHEN sqlc.arg(state)::text = '' THEN state ELSE sqlc.arg(state) END,
    updated_at = now()
WHERE workspace_id = sqlc.arg(workspace_id) AND run_id = sqlc.arg(run_id) AND generation = sqlc.arg(generation);

-- name: LockSessionOwner :one
SELECT writer_run_id, writer_fence, (writer_until IS NOT NULL AND writer_until > now()) AS writer_current
FROM makez_native_sessions
WHERE id = $1 AND workspace_id = $2
FOR UPDATE;

-- name: ClaimSessionWriter :exec
UPDATE makez_native_sessions
SET writer_run_id = sqlc.arg(writer_run_id),
    writer_fence = CASE
      WHEN writer_run_id IS DISTINCT FROM sqlc.arg(writer_run_id) THEN writer_fence + 1
      ELSE writer_fence
    END,
    writer_until = now() + (sqlc.arg(lease_seconds)::int * interval '1 second'),
    updated_at = now()
WHERE id = sqlc.arg(id) AND workspace_id = sqlc.arg(workspace_id);

-- name: ReleaseSessionWriter :exec
UPDATE makez_native_sessions
SET writer_run_id = NULL, writer_until = NULL, updated_at = now()
WHERE id = $1 AND workspace_id = $2 AND writer_run_id = $3;

-- name: GetNativeCommand :one
SELECT c.disposition, c.run_id, c.generation, c.started
FROM makez_native_commands c
JOIN makez_native_sessions s ON s.id = c.session_id
WHERE c.session_id = $1 AND s.workspace_id = $2 AND c.prompt_sha256 = $3;

-- name: HasUnknownNativeCommand :one
SELECT EXISTS (
  SELECT 1
  FROM makez_native_commands c
  JOIN makez_native_sessions s ON s.id = c.session_id
  WHERE c.session_id = $1
    AND s.workspace_id = $2
    AND c.prompt_sha256 <> $3
    AND (c.disposition = 'accepted' OR (c.disposition = 'sending' AND c.started))
) AS unknown;

-- name: UpsertNativeCommand :exec
INSERT INTO makez_native_commands (
  session_id, prompt_sha256, disposition, workspace_id, run_id, generation, started
) VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (session_id, prompt_sha256) DO UPDATE
SET disposition = EXCLUDED.disposition,
    workspace_id = EXCLUDED.workspace_id,
    run_id = EXCLUDED.run_id,
    generation = EXCLUDED.generation,
    -- A repeated sending write by the same run must not clear a start that already happened.
    started = CASE
      WHEN makez_native_commands.started AND makez_native_commands.run_id = EXCLUDED.run_id THEN true
      ELSE EXCLUDED.started
    END,
    updated_at = now();

-- name: MarkNativeCommandStarted :execrows
UPDATE makez_native_commands
SET started = true, updated_at = now()
WHERE session_id = $1 AND prompt_sha256 = $2 AND workspace_id = $3
  AND run_id = $4 AND generation = $5 AND disposition = 'sending';
