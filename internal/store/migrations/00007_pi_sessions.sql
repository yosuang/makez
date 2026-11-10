-- +goose Up

-- One run may launch up to three Pi processes. Offsets restart with each
-- process, so the event identity includes the attempt generation. Generation 1
-- is the default for rows written before this migration.
ALTER TABLE makez_events ADD COLUMN attempt_generation bigint NOT NULL DEFAULT 1;

-- +goose StatementBegin
DO $$
DECLARE
  constraint_name text;
BEGIN
  SELECT conname INTO constraint_name
  FROM pg_constraint
  WHERE conrelid = 'makez_events'::regclass
    AND contype = 'u'
    AND pg_get_constraintdef(oid) LIKE '%source_offset%'
    AND pg_get_constraintdef(oid) NOT LIKE '%attempt_generation%';
  IF constraint_name IS NOT NULL THEN
    EXECUTE format('ALTER TABLE makez_events DROP CONSTRAINT %I', constraint_name);
  END IF;
END $$;
-- +goose StatementEnd

ALTER TABLE makez_events ADD CONSTRAINT makez_events_attempt_offset_key UNIQUE (run_id, attempt_generation, source_offset, item_index);
ALTER TABLE makez_events ADD CONSTRAINT makez_events_attempt_generation_positive CHECK (attempt_generation >= 1);

-- Attempt 1 keeps the historical step name "agent". Later processes use agent:2 and agent:3.
-- +goose StatementBegin
DO $$
DECLARE
  constraint_name text;
BEGIN
  SELECT conname INTO constraint_name
  FROM pg_constraint
  WHERE conrelid = 'makez_operations'::regclass
    AND contype = 'c'
    AND pg_get_constraintdef(oid) LIKE '%sandbox%';
  IF constraint_name IS NOT NULL THEN
    EXECUTE format('ALTER TABLE makez_operations DROP CONSTRAINT %I', constraint_name);
  END IF;
END $$;
-- +goose StatementEnd

ALTER TABLE makez_operations ADD CONSTRAINT makez_operations_step_check CHECK (
  step = 'sandbox' OR step = 'agent' OR step ~ '^agent:[1-9][0-9]{0,2}$'
);

-- A logical native session survives runs. It is not a Philo conversation, turn,
-- message sequence, owner binding, or retention record.
CREATE TABLE makez_native_sessions (
  id text PRIMARY KEY,
  workspace_id text NOT NULL REFERENCES makez_workspaces(id),
  scope text NOT NULL CHECK (scope IN ('personal', 'worker')),
  bot_id text,
  conversation_id text,
  delegation_run_id text,
  harness_id text NOT NULL,
  harness_pin text NOT NULL,
  last_disposition text NOT NULL DEFAULT 'none' CHECK (last_disposition IN ('none', 'sending', 'accepted', 'settled', 'rejected', 'reconciled')),
  last_prompt_sha256 text NOT NULL DEFAULT '',
  checkpoint bytea,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  CHECK (octet_length(COALESCE(checkpoint, ''::bytea)) <= 1048576),
  CHECK (harness_id ~ '^[a-z][a-z0-9._-]{0,63}$'),
  CHECK (harness_pin ~ '^[0-9a-f]{40}$'),
  CHECK (last_prompt_sha256 = '' OR last_prompt_sha256 ~ '^[0-9a-f]{64}$'),
  CHECK (
    (scope = 'personal' AND bot_id IS NOT NULL AND conversation_id IS NOT NULL AND delegation_run_id IS NULL
      AND length(bot_id) BETWEEN 1 AND 128 AND length(conversation_id) BETWEEN 1 AND 128)
    OR
    (scope = 'worker' AND delegation_run_id IS NOT NULL AND bot_id IS NULL AND conversation_id IS NULL
      AND length(delegation_run_id) BETWEEN 1 AND 128)
  )
);

CREATE UNIQUE INDEX makez_native_sessions_personal_key
  ON makez_native_sessions (workspace_id, bot_id, conversation_id, harness_id, harness_pin)
  WHERE scope = 'personal';

CREATE UNIQUE INDEX makez_native_sessions_worker_key
  ON makez_native_sessions (workspace_id, delegation_run_id, harness_id, harness_pin)
  WHERE scope = 'worker';

CREATE TABLE makez_attempts (
  workspace_id text NOT NULL,
  run_id text NOT NULL,
  session_id text NOT NULL REFERENCES makez_native_sessions(id),
  generation bigint NOT NULL CHECK (generation BETWEEN 1 AND 3),
  step text NOT NULL,
  state text NOT NULL CHECK (state IN ('open', 'lost', 'closed')),
  disposition text NOT NULL DEFAULT 'none' CHECK (disposition IN ('none', 'sending', 'accepted', 'settled', 'rejected', 'reconciled')),
  prompt_sha256 text NOT NULL CHECK (prompt_sha256 ~ '^[0-9a-f]{64}$'),
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (run_id, generation),
  FOREIGN KEY (workspace_id, run_id) REFERENCES makez_runs (workspace_id, id),
  CHECK (step = 'agent' OR step ~ '^agent:[1-9][0-9]{0,2}$')
);
