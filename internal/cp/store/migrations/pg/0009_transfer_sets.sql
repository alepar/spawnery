-- +goose Up
CREATE TABLE migration_transfer_sets (
  id text PRIMARY KEY,
  spawn_id text NOT NULL REFERENCES spawns(id) ON DELETE CASCADE,
  source_generation bigint NOT NULL,
  target_generation bigint NOT NULL,
  source_node_id text NOT NULL,
  target_node_id text NOT NULL,
  base_image_digest text NOT NULL DEFAULT '',
  mount_manifest_pins text NOT NULL DEFAULT '{}',
  rootfs_artifact_pins text NOT NULL DEFAULT '[]',
  transfer_key_ciphertext_metadata text NOT NULL DEFAULT '{}',
  transfer_key_status text NOT NULL,
  status text NOT NULL,
  created_at bigint NOT NULL,
  updated_at bigint NOT NULL
);

CREATE INDEX idx_migration_transfer_sets_spawn ON migration_transfer_sets(spawn_id, created_at);

-- +goose Down
DROP TABLE migration_transfer_sets;
