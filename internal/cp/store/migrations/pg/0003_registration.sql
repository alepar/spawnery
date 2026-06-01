-- +goose Up
ALTER TABLE apps               ADD COLUMN creator_id TEXT NOT NULL DEFAULT '';
ALTER TABLE app_versions       ADD COLUMN manifest   TEXT NOT NULL DEFAULT '';
ALTER TABLE app_version_mounts ADD COLUMN path       TEXT NOT NULL DEFAULT '';
ALTER TABLE app_version_mounts ADD COLUMN seed       TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE app_version_mounts DROP COLUMN seed;
ALTER TABLE app_version_mounts DROP COLUMN path;
ALTER TABLE app_versions       DROP COLUMN manifest;
ALTER TABLE apps               DROP COLUMN creator_id;
