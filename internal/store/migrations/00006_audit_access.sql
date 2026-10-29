-- +goose Up
-- Existing request hashes are canonical format version 1; do not rewrite them.
ALTER TABLE makez_tasks ADD COLUMN request_hash_version smallint NOT NULL DEFAULT 1 CHECK(request_hash_version=1);
CREATE INDEX makez_artifact_storage_key ON makez_artifacts(storage_key);
