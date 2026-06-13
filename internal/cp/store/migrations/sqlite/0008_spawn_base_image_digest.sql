-- +goose Up
ALTER TABLE spawns ADD COLUMN base_image_digest TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE spawns DROP COLUMN base_image_digest;
