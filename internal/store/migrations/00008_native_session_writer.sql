-- +goose Up

-- One writer at a time for a logical native session. The lease is a timestamp
-- on the row, not a session advisory lock: an advisory lock ends when the
-- activity returns its connection to the pool. writer_fence advances when a
-- different run takes an expired lease. It is not an owner binding and not a
-- retention TTL.
ALTER TABLE makez_native_sessions
  ADD COLUMN writer_run_id text,
  ADD COLUMN writer_fence bigint NOT NULL DEFAULT 0,
  ADD COLUMN writer_until timestamptz,
  ADD CONSTRAINT makez_native_sessions_writer_fence_nonnegative CHECK (writer_fence >= 0),
  ADD CONSTRAINT makez_native_sessions_writer_pair CHECK (
    (writer_run_id IS NULL AND writer_until IS NULL)
    OR (writer_run_id IS NOT NULL AND writer_until IS NOT NULL AND length(writer_run_id) > 0)
  );

-- One row per prompt. A settled observation for a different prompt updates
-- only its own row, so it cannot make a sending or accepted prompt sendable.
CREATE TABLE makez_native_commands (
  session_id text NOT NULL REFERENCES makez_native_sessions (id),
  prompt_sha256 text NOT NULL CHECK (prompt_sha256 ~ '^[0-9a-f]{64}$'),
  workspace_id text NOT NULL,
  run_id text NOT NULL,
  generation bigint NOT NULL CHECK (generation BETWEEN 1 AND 3),
  disposition text NOT NULL CHECK (disposition IN ('sending', 'accepted', 'settled', 'rejected', 'reconciled')),
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (session_id, prompt_sha256),
  FOREIGN KEY (workspace_id, run_id) REFERENCES makez_runs (workspace_id, id)
);
