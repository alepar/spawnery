-- +goose Up
ALTER TABLE spawns ADD COLUMN name text NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE spawns DROP COLUMN name;
