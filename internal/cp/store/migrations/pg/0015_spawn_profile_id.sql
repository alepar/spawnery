-- +goose Up
-- Add profile_id to spawns: records which profile was applied at create time (sp-nrzf.3.8/3.9).
-- Nullable (empty string sentinel) — pre-existing spawns have no profile.
ALTER TABLE spawns ADD COLUMN profile_id text NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE spawns DROP COLUMN profile_id;
