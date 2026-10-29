-- +goose Up
ALTER TABLE makez_runs ADD COLUMN IF NOT EXISTS agent_plan jsonb;
