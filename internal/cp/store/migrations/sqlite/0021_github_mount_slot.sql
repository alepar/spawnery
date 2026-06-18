-- +goose Up
ALTER TABLE app_version_mounts ADD COLUMN github INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE app_version_mounts DROP COLUMN github;
