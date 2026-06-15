-- +goose Up
CREATE TABLE customization_catalog (
  catalog_id  text    NOT NULL PRIMARY KEY,
  creator_id  text    NOT NULL,
  kind        text    NOT NULL,
  name        text    NOT NULL,
  description text    NOT NULL,
  content     bytea,
  listed      boolean NOT NULL DEFAULT true,
  created_at  bigint  NOT NULL,
  updated_at  bigint  NOT NULL
);

CREATE INDEX idx_customization_catalog_creator ON customization_catalog(creator_id);

-- +goose Down
DROP TABLE customization_catalog;
