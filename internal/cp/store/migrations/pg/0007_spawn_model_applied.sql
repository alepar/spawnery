-- +goose Up
ALTER TABLE spawns ADD COLUMN model_applied      boolean NOT NULL DEFAULT true;
ALTER TABLE spawns ADD COLUMN model_apply_detail text    NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE spawns DROP COLUMN model_apply_detail;
ALTER TABLE spawns DROP COLUMN model_applied;
