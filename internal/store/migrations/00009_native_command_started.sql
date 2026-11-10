-- +goose Up

-- started is false while the command row exists and the process has not started.
-- The prompt has not reached Pi, so the lease holder or the next writer sends it once.
-- It becomes true only after that process exists, and before the runner may write the prompt.
-- It is not a public task field.
ALTER TABLE makez_native_commands
  ADD COLUMN started boolean NOT NULL DEFAULT false;
