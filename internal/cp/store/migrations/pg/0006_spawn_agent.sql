-- +goose Up
ALTER TABLE spawns ADD COLUMN image       text NOT NULL DEFAULT '';
ALTER TABLE spawns ADD COLUMN runnable_id text NOT NULL DEFAULT '';
ALTER TABLE spawns ADD COLUMN mode        text NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE spawns DROP COLUMN mode;
ALTER TABLE spawns DROP COLUMN runnable_id;
ALTER TABLE spawns DROP COLUMN image;
