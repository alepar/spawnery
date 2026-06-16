-- +goose Up
ALTER TABLE spawn_mounts ADD COLUMN credential_secret_id text NOT NULL DEFAULT '';
ALTER TABLE spawn_mounts ADD COLUMN create_if_missing boolean NOT NULL DEFAULT false;
ALTER TABLE spawn_mounts ADD COLUMN repository_id text NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE spawn_mounts DROP COLUMN repository_id;
ALTER TABLE spawn_mounts DROP COLUMN create_if_missing;
ALTER TABLE spawn_mounts DROP COLUMN credential_secret_id;
