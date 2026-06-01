-- +goose Up
CREATE TABLE owners ( id TEXT PRIMARY KEY, email TEXT, created_at INTEGER NOT NULL );
CREATE TABLE apps   ( id TEXT PRIMARY KEY, display_name TEXT, created_at INTEGER NOT NULL );

CREATE TABLE app_versions (
  app_id     TEXT NOT NULL REFERENCES apps(id),
  version    TEXT NOT NULL,
  ref        TEXT NOT NULL,
  reviewed   INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  PRIMARY KEY (app_id, version)
);
CREATE INDEX idx_app_versions_reviewed ON app_versions(app_id, reviewed, created_at DESC);

CREATE TABLE app_version_mounts (
  app_id   TEXT NOT NULL,
  version  TEXT NOT NULL,
  name     TEXT NOT NULL,
  required INTEGER NOT NULL DEFAULT 1,
  PRIMARY KEY (app_id, version, name),
  FOREIGN KEY (app_id, version) REFERENCES app_versions(app_id, version)
);

CREATE TABLE spawns (
  id           TEXT PRIMARY KEY,
  owner_id     TEXT NOT NULL REFERENCES owners(id),
  app_id       TEXT NOT NULL REFERENCES apps(id),
  app_version  TEXT NOT NULL,
  app_ref      TEXT NOT NULL,
  pinned       INTEGER NOT NULL DEFAULT 0,
  model        TEXT NOT NULL,
  status       TEXT NOT NULL CHECK (status IN ('starting','active','suspending','suspended','unreachable','error','deleted')),
  recovered    INTEGER NOT NULL DEFAULT 0,
  created_at   INTEGER NOT NULL,
  last_used_at INTEGER NOT NULL,
  suspended_at INTEGER,
  deleted_at   INTEGER
);
CREATE INDEX idx_spawns_owner  ON spawns(owner_id, last_used_at DESC);
CREATE INDEX idx_spawns_status ON spawns(status);

CREATE TABLE spawn_containers (
  spawn_id   TEXT    NOT NULL REFERENCES spawns(id),
  generation INTEGER NOT NULL,
  node_id    TEXT    NOT NULL,
  phase      TEXT    NOT NULL CHECK (phase IN ('starting','active','suspending','stopped','lost')),
  started_at INTEGER NOT NULL,
  ended_at   INTEGER,
  PRIMARY KEY (spawn_id, generation)
);
CREATE UNIQUE INDEX uniq_live_container ON spawn_containers(spawn_id) WHERE ended_at IS NULL;
CREATE INDEX idx_live_by_node ON spawn_containers(node_id) WHERE ended_at IS NULL;

CREATE TABLE spawn_mounts (
  spawn_id       TEXT NOT NULL REFERENCES spawns(id),
  name           TEXT NOT NULL,
  backend_uri    TEXT NOT NULL,
  persist_marker TEXT,
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
