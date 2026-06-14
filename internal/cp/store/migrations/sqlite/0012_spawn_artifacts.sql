-- +goose Up
CREATE TABLE spawn_artifacts (
  spawn_id         TEXT NOT NULL REFERENCES spawns(id),
  artifact_id      TEXT NOT NULL,
  inline           BLOB,
  content_type     INTEGER NOT NULL DEFAULT 0,
  target_container INTEGER NOT NULL DEFAULT 0,
  dest_path        TEXT NOT NULL DEFAULT '',
  mode             INTEGER NOT NULL DEFAULT 0,
  sensitive        INTEGER NOT NULL DEFAULT 0,
  env_var_name     TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (spawn_id, artifact_id)
);

-- +goose Down
DROP TABLE spawn_artifacts;
