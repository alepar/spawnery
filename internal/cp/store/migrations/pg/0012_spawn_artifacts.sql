-- +goose Up
CREATE TABLE spawn_artifacts (
  spawn_id         text NOT NULL REFERENCES spawns(id),
  artifact_id      text NOT NULL,
  inline           bytea,
  content_type     integer NOT NULL DEFAULT 0,
  target_container integer NOT NULL DEFAULT 0,
  dest_path        text NOT NULL DEFAULT '',
  mode             bigint NOT NULL DEFAULT 0,
  sensitive        boolean NOT NULL DEFAULT false,
  env_var_name     text NOT NULL DEFAULT '',
  PRIMARY KEY (spawn_id, artifact_id)
);

-- +goose Down
DROP TABLE spawn_artifacts;
