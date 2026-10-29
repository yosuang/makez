-- +goose Up

ALTER TABLE makez_runs ADD COLUMN started_at timestamptz;
-- Historical outbox creation times are unknown; age starts at this migration.
ALTER TABLE makez_outbox ADD COLUMN created_at timestamptz NOT NULL DEFAULT now();
CREATE INDEX makez_tasks_recovery_cursor ON makez_tasks(created_at,id);
CREATE INDEX makez_outbox_pending_created ON makez_outbox(created_at) WHERE delivered_at IS NULL;
CREATE INDEX makez_runs_cleanup_needed ON makez_runs(cleanup) WHERE cleanup IN ('PENDING','BLOCKED');
