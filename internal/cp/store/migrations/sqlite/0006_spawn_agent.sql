-- +goose Up
ALTER TABLE spawns ADD COLUMN image       TEXT NOT NULL DEFAULT '';
ALTER TABLE spawns ADD COLUMN runnable_id TEXT NOT NULL DEFAULT '';
ALTER TABLE spawns ADD COLUMN mode        TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE spawns DROP COLUMN mode;
ALTER TABLE spawns DROP COLUMN runnable_id;
ALTER TABLE spawns DROP COLUMN image;
