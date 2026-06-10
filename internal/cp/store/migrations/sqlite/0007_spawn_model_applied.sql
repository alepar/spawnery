-- +goose Up
ALTER TABLE spawns ADD COLUMN model_applied      INTEGER NOT NULL DEFAULT 1;
ALTER TABLE spawns ADD COLUMN model_apply_detail TEXT    NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE spawns DROP COLUMN model_apply_detail;
ALTER TABLE spawns DROP COLUMN model_applied;
