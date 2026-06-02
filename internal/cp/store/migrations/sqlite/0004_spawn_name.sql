-- +goose Up
ALTER TABLE spawns ADD COLUMN name TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE spawns DROP COLUMN name;
