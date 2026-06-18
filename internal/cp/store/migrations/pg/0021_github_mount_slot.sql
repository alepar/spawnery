-- +goose Up
ALTER TABLE app_version_mounts ADD COLUMN github BOOLEAN NOT NULL DEFAULT FALSE;

-- +goose Down
ALTER TABLE app_version_mounts DROP COLUMN github;
