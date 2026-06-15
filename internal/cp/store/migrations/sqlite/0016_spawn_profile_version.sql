-- +goose Up
-- Snapshot the profile's CAS version applied at create time (sp-nrzf.3.8 §9).
ALTER TABLE spawns ADD COLUMN profile_version INTEGER NOT NULL DEFAULT 0;

-- +goose Down
-- SQLite cannot DROP COLUMN portably; leave the column in place on rollback.
SELECT 1;
