-- +goose Up
ALTER TABLE customization_catalog ADD COLUMN source_url    TEXT;
ALTER TABLE customization_catalog ADD COLUMN source_ref    TEXT;
ALTER TABLE customization_catalog ADD COLUMN source_subdir TEXT;
ALTER TABLE customization_catalog ADD COLUMN sha256        TEXT;
ALTER TABLE customization_catalog ADD COLUMN size          INTEGER;
CREATE UNIQUE INDEX idx_customization_catalog_owner_sha
  ON customization_catalog(creator_id, sha256);

-- +goose Down
DROP INDEX idx_customization_catalog_owner_sha;
ALTER TABLE customization_catalog DROP COLUMN size;
ALTER TABLE customization_catalog DROP COLUMN sha256;
ALTER TABLE customization_catalog DROP COLUMN source_subdir;
ALTER TABLE customization_catalog DROP COLUMN source_ref;
ALTER TABLE customization_catalog DROP COLUMN source_url;
