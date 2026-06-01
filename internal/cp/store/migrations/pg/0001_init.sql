-- +goose Up
CREATE TABLE owners ( id text PRIMARY KEY, email text, created_at bigint NOT NULL );
CREATE TABLE apps   ( id text PRIMARY KEY, display_name text, created_at bigint NOT NULL );

CREATE TABLE app_versions (
  app_id     text NOT NULL REFERENCES apps(id),
  version    text NOT NULL,
  ref        text NOT NULL,
  reviewed   boolean NOT NULL DEFAULT false,
  created_at bigint NOT NULL,
  PRIMARY KEY (app_id, version)
);
CREATE INDEX idx_app_versions_reviewed ON app_versions(app_id, reviewed, created_at DESC);

CREATE TABLE app_version_mounts (
  app_id   text NOT NULL,
  version  text NOT NULL,
  name     text NOT NULL,
  required boolean NOT NULL DEFAULT true,
  PRIMARY KEY (app_id, version, name),
  FOREIGN KEY (app_id, version) REFERENCES app_versions(app_id, version)
);

CREATE TABLE spawns (
  id           text PRIMARY KEY,
  owner_id     text NOT NULL REFERENCES owners(id),
  app_id       text NOT NULL REFERENCES apps(id),
  app_version  text NOT NULL,
  app_ref      text NOT NULL,
  pinned       boolean NOT NULL DEFAULT false,
  model        text NOT NULL,
  status       text NOT NULL CHECK (status IN ('starting','active','suspending','suspended','unreachable','error','deleted')),
  recovered    boolean NOT NULL DEFAULT false,
  created_at   bigint NOT NULL,
  last_used_at bigint NOT NULL,
  suspended_at bigint,
  deleted_at   bigint
);
CREATE INDEX idx_spawns_owner  ON spawns(owner_id, last_used_at DESC);
CREATE INDEX idx_spawns_status ON spawns(status);

CREATE TABLE spawn_containers (
  spawn_id   text   NOT NULL REFERENCES spawns(id),
  generation bigint NOT NULL,
  node_id    text   NOT NULL,
  phase      text   NOT NULL CHECK (phase IN ('starting','active','suspending','stopped','lost')),
  started_at bigint NOT NULL,
  ended_at   bigint,
  PRIMARY KEY (spawn_id, generation)
);
CREATE UNIQUE INDEX uniq_live_container ON spawn_containers(spawn_id) WHERE ended_at IS NULL;
CREATE INDEX idx_live_by_node ON spawn_containers(node_id) WHERE ended_at IS NULL;

CREATE TABLE spawn_mounts (
  spawn_id       text NOT NULL REFERENCES spawns(id),
  name           text NOT NULL,
  backend_uri    text NOT NULL,
  persist_marker text,
  PRIMARY KEY (spawn_id, name)
);

-- +goose Down
DROP TABLE spawn_mounts;
DROP TABLE spawn_containers;
DROP TABLE spawns;
DROP TABLE app_version_mounts;
DROP TABLE app_versions;
DROP TABLE apps;
DROP TABLE owners;
