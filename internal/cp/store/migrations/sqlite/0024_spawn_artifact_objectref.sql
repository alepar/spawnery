-- +goose Up
-- By-ref skill delivery (sp-nrzf.3.14.5): object_key != '' discriminates the by-ref path;
-- object_sha256 is the hex sha256 of the canonical plain tar (integrity identity).
-- Existing rows (inline artifacts) default to '' (empty = inline discriminator).
ALTER TABLE spawn_artifacts ADD COLUMN object_key TEXT NOT NULL DEFAULT '';
ALTER TABLE spawn_artifacts ADD COLUMN object_sha256 TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE spawn_artifacts DROP COLUMN object_sha256;
ALTER TABLE spawn_artifacts DROP COLUMN object_key;
