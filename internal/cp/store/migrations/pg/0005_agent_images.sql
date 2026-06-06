-- +goose Up
CREATE TABLE agent_images (
  image      text   PRIMARY KEY,
  created_at bigint NOT NULL
);
CREATE TABLE agent_image_binaries (
  image       text NOT NULL REFERENCES agent_images(image) ON DELETE CASCADE,
  binary_name text NOT NULL,
  PRIMARY KEY (image, binary_name)
);

-- +goose Down
DROP TABLE agent_image_binaries;
DROP TABLE agent_images;
