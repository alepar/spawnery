-- +goose Up
ALTER TABLE customization_catalog
  ADD COLUMN source_url    text,
  ADD COLUMN source_ref    text,
  ADD COLUMN source_subdir text,
  ADD COLUMN sha256        text,
  ADD COLUMN size          bigint;
CREATE UNIQUE INDEX idx_customization_catalog_owner_sha
  ON customization_catalog(creator_id, sha256);

-- +goose Down
DROP INDEX idx_customization_catalog_owner_sha;
ALTER TABLE customization_catalog
  DROP COLUMN source_url, DROP COLUMN source_ref, DROP COLUMN source_subdir,
  DROP COLUMN sha256, DROP COLUMN size;
