-- +goose Up
CREATE TABLE agent_images (
  image      TEXT PRIMARY KEY,
  created_at INTEGER NOT NULL
);
CREATE TABLE agent_image_binaries (
  image       TEXT NOT NULL REFERENCES agent_images(image) ON DELETE CASCADE,
  binary_name TEXT NOT NULL,
  PRIMARY KEY (image, binary_name)
);

-- +goose Down
DROP TABLE agent_image_binaries;
DROP TABLE agent_images;
