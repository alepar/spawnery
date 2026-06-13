-- +goose Up
CREATE TABLE migration_transfer_sets (
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

CREATE INDEX idx_migration_transfer_sets_spawn ON migration_transfer_sets(spawn_id, created_at);

-- +goose Down
DROP TABLE migration_transfer_sets;
