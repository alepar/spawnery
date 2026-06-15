-- +goose Up
-- Add profile_id to spawns: records which profile was applied at create time (sp-nrzf.3.8/3.9).
-- Nullable (empty string sentinel) — pre-existing spawns have no profile.
ALTER TABLE spawns ADD COLUMN profile_id TEXT NOT NULL DEFAULT '';

-- +goose Down
-- SQLite does not support DROP COLUMN in all versions; leave the column in place on rollback.
SELECT 1;
