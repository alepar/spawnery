-- +goose Up
CREATE TABLE customization_catalog (
  catalog_id  TEXT    NOT NULL PRIMARY KEY,
  creator_id  TEXT    NOT NULL,
  kind        TEXT    NOT NULL,
  name        TEXT    NOT NULL,
  description TEXT    NOT NULL,
  content     BLOB,
  listed      INTEGER NOT NULL DEFAULT 1,
  created_at  INTEGER NOT NULL,
  updated_at  INTEGER NOT NULL
);

CREATE INDEX idx_customization_catalog_creator ON customization_catalog(creator_id);

-- +goose Down
DROP TABLE customization_catalog;
