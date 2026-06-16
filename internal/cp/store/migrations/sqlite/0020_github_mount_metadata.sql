-- +goose Up
ALTER TABLE spawn_mounts ADD COLUMN credential_secret_id TEXT NOT NULL DEFAULT '';
ALTER TABLE spawn_mounts ADD COLUMN create_if_missing INTEGER NOT NULL DEFAULT 0;
ALTER TABLE spawn_mounts ADD COLUMN repository_id TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE spawn_mounts DROP COLUMN repository_id;
ALTER TABLE spawn_mounts DROP COLUMN create_if_missing;
ALTER TABLE spawn_mounts DROP COLUMN credential_secret_id;
