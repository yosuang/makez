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
SET checkpoint = $3, updated_at = now()
WHERE id = $1 AND workspace_id = $2 AND octet_length($3) BETWEEN 1 AND 1048576;

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
