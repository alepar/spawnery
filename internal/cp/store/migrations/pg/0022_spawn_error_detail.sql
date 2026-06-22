-- +goose Up
ALTER TABLE spawns ADD COLUMN error_step   text NOT NULL DEFAULT '';
ALTER TABLE spawns ADD COLUMN error_detail text NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE spawns DROP COLUMN error_detail;
ALTER TABLE spawns DROP COLUMN error_step;
