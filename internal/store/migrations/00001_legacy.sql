-- +goose Up
CREATE TABLE IF NOT EXISTS makez_tasks (
    id text PRIMARY KEY,
    workspace_id text NOT NULL,
    idempotency_key text NOT NULL,
    request_hash text NOT NULL,
    prompt text NOT NULL,
    config jsonb NOT NULL DEFAULT '{}'::jsonb,
    agent_id text NOT NULL,
    agent_version text NOT NULL,
    profile_id text NOT NULL,
    profile_version text NOT NULL,
    state text NOT NULL CHECK (state IN ('QUEUED','RUNNING','CANCELING','SUCCEEDED','FAILED','CANCELED')),
    reason text NOT NULL DEFAULT '',
    deadline timestamptz NOT NULL,
    cancel_requested boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT now(),
    version bigint NOT NULL DEFAULT 1,
    UNIQUE(workspace_id,idempotency_key)
);
CREATE INDEX IF NOT EXISTS makez_tasks_workspace_created ON makez_tasks(workspace_id,created_at DESC);
CREATE TABLE IF NOT EXISTS makez_runs (
    id text PRIMARY KEY,
    task_id text NOT NULL UNIQUE REFERENCES makez_tasks(id),
    state text NOT NULL CHECK (state IN ('PREPARING','STARTING','RUNNING','STOPPING','FINALIZING','SUCCEEDED','FAILED','CANCELED')),
    backend_id text NOT NULL,
    sandbox jsonb,
    process jsonb,
    cursor text NOT NULL DEFAULT '',
    parser_state bytea NOT NULL DEFAULT ''::bytea,
    observer_epoch bigint NOT NULL DEFAULT 0,
    process_exit jsonb,
    prepared_result jsonb,
    cleanup text NOT NULL DEFAULT 'NOT_REQUIRED' CHECK (cleanup IN ('NOT_REQUIRED','PENDING','DONE','BLOCKED')),
    verified_stopped boolean NOT NULL DEFAULT false,
    version bigint NOT NULL DEFAULT 1
);
CREATE TABLE IF NOT EXISTS makez_outbox (
    id text PRIMARY KEY,
    workspace_id text NOT NULL,
    task_id text NOT NULL REFERENCES makez_tasks(id),
    run_id text NOT NULL DEFAULT '',
    command text NOT NULL CHECK (command IN ('START_TASK','CANCEL_TASK','START_CLEANUP')),
    attempts integer NOT NULL DEFAULT 0,
    last_error text NOT NULL DEFAULT '',
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    delivered_at timestamptz,
    UNIQUE(task_id,command)
);
CREATE INDEX IF NOT EXISTS makez_outbox_pending ON makez_outbox(next_attempt_at) WHERE delivered_at IS NULL;
CREATE TABLE IF NOT EXISTS makez_operations (
    run_id text NOT NULL REFERENCES makez_runs(id),
    step text NOT NULL CHECK (step IN ('sandbox','agent')),
    request_hash text NOT NULL,
    phase text NOT NULL CHECK (phase IN ('DISPATCHING','CONFIRMED','UNCERTAIN','REJECTED')),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY(run_id,step)
);
CREATE TABLE IF NOT EXISTS makez_events (
    seq bigserial PRIMARY KEY,
    run_id text NOT NULL REFERENCES makez_runs(id),
    source_offset text NOT NULL,
    item_index bigint NOT NULL,
    type text NOT NULL,
    payload jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE(run_id,source_offset,item_index)
);
CREATE INDEX IF NOT EXISTS makez_events_run_seq ON makez_events(run_id,seq);
CREATE TABLE IF NOT EXISTS makez_artifacts (
    id text PRIMARY KEY,
    run_id text NOT NULL REFERENCES makez_runs(id),
    path text NOT NULL,
    storage_key text NOT NULL,
    sha256 text NOT NULL,
    media_type text NOT NULL,
    size bigint NOT NULL CHECK (size >= 0),
    UNIQUE(run_id,path)
);
ALTER TABLE makez_tasks ADD COLUMN IF NOT EXISTS config jsonb NOT NULL DEFAULT '{}'::jsonb;
ALTER TABLE makez_runs ADD COLUMN IF NOT EXISTS prepared_result jsonb;
