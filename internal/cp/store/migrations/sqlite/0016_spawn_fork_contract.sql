-- +goose Up
ALTER TABLE spawns ADD COLUMN parent_spawn_id TEXT REFERENCES spawns(id);
ALTER TABLE spawns ADD COLUMN forked_at INTEGER;
CREATE INDEX idx_spawns_parent_spawn_id ON spawns(parent_spawn_id);

ALTER TABLE migration_transfer_sets ADD COLUMN kind TEXT NOT NULL DEFAULT 'migration';
ALTER TABLE migration_transfer_sets ADD COLUMN source_spawn_id TEXT NOT NULL DEFAULT '';
ALTER TABLE migration_transfer_sets ADD COLUMN fork_spawn_id TEXT NOT NULL DEFAULT '';
UPDATE migration_transfer_sets
  SET source_spawn_id = spawn_id
  WHERE source_spawn_id = '';
CREATE INDEX idx_migration_transfer_sets_kind_status_fork
  ON migration_transfer_sets(kind, status, fork_spawn_id);
CREATE INDEX idx_migration_transfer_sets_source
  ON migration_transfer_sets(source_spawn_id);

-- +goose Down
PRAGMA foreign_keys = OFF;

CREATE TABLE spawns_old (
  id                    TEXT PRIMARY KEY,
  owner_id              TEXT NOT NULL REFERENCES owners(id),
  app_id                TEXT NOT NULL REFERENCES apps(id),
  app_version           TEXT NOT NULL,
  app_ref               TEXT NOT NULL,
  pinned                INTEGER NOT NULL DEFAULT 0,
  model                 TEXT NOT NULL,
  status                TEXT NOT NULL CHECK (status IN ('starting','active','suspending','suspended','resuming','forking','unreachable','error','deleted')),
  recovered             INTEGER NOT NULL DEFAULT 0,
  created_at            INTEGER NOT NULL,
  last_used_at          INTEGER NOT NULL,
  suspended_at          INTEGER,
  deleted_at            INTEGER,
  name                  TEXT NOT NULL DEFAULT '',
  image                 TEXT NOT NULL DEFAULT '',
  runnable_id           TEXT NOT NULL DEFAULT '',
  mode                  TEXT NOT NULL DEFAULT '',
  model_applied         INTEGER NOT NULL DEFAULT 1,
  model_apply_detail    TEXT NOT NULL DEFAULT '',
  base_image_digest     TEXT NOT NULL DEFAULT '',
  status_seq            INTEGER NOT NULL DEFAULT 0,
  claim_holder          TEXT,
  claim_lease_id        TEXT,
  claim_deadline        INTEGER,
  fork_capture_deadline INTEGER
);

INSERT INTO spawns_old
  SELECT id, owner_id, app_id, app_version, app_ref, pinned, model, status, recovered,
         created_at, last_used_at, suspended_at, deleted_at, name, image, runnable_id, mode,
         model_applied, model_apply_detail, base_image_digest, status_seq,
         claim_holder, claim_lease_id, claim_deadline, fork_capture_deadline
  FROM spawns;

DROP TABLE spawns;
ALTER TABLE spawns_old RENAME TO spawns;
CREATE INDEX idx_spawns_owner  ON spawns(owner_id, last_used_at DESC);
CREATE INDEX idx_spawns_status ON spawns(status);

CREATE TABLE migration_transfer_sets_old (
  id TEXT PRIMARY KEY,
  spawn_id TEXT NOT NULL REFERENCES spawns(id) ON DELETE CASCADE,
  source_generation INTEGER NOT NULL,
  target_generation INTEGER NOT NULL,
  source_node_id TEXT NOT NULL,
  target_node_id TEXT NOT NULL,
  base_image_digest TEXT NOT NULL DEFAULT '',
  mount_manifest_pins TEXT NOT NULL DEFAULT '{}',
  rootfs_artifact_pins TEXT NOT NULL DEFAULT '[]',
  transfer_key_ciphertext_metadata TEXT NOT NULL DEFAULT '{}',
  transfer_key_status TEXT NOT NULL,
  status TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);

INSERT INTO migration_transfer_sets_old
  SELECT id, spawn_id, source_generation, target_generation, source_node_id, target_node_id,
         base_image_digest, mount_manifest_pins, rootfs_artifact_pins,
         transfer_key_ciphertext_metadata, transfer_key_status, status, created_at, updated_at
  FROM migration_transfer_sets;

DROP TABLE migration_transfer_sets;
ALTER TABLE migration_transfer_sets_old RENAME TO migration_transfer_sets;
CREATE INDEX idx_migration_transfer_sets_spawn
  ON migration_transfer_sets(spawn_id, created_at);

PRAGMA foreign_keys = ON;
