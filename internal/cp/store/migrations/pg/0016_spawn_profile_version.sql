-- +goose Up
-- Snapshot the profile's CAS version applied at create time (sp-nrzf.3.8 §9).
ALTER TABLE spawns ADD COLUMN profile_version bigint NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE spawns DROP COLUMN profile_version;
