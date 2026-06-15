-- +goose Up
ALTER TABLE spawns
  ADD COLUMN parent_spawn_id text REFERENCES spawns(id),
  ADD COLUMN forked_at bigint;
CREATE INDEX idx_spawns_parent_spawn_id ON spawns(parent_spawn_id);

ALTER TABLE migration_transfer_sets
  ADD COLUMN kind text NOT NULL DEFAULT 'migration',
  ADD COLUMN source_spawn_id text NOT NULL DEFAULT '',
  ADD COLUMN fork_spawn_id text NOT NULL DEFAULT '';
UPDATE migration_transfer_sets
  SET source_spawn_id = spawn_id
  WHERE source_spawn_id = '';
CREATE INDEX idx_migration_transfer_sets_kind_status_fork
  ON migration_transfer_sets(kind, status, fork_spawn_id);
CREATE INDEX idx_migration_transfer_sets_source
  ON migration_transfer_sets(source_spawn_id);

-- +goose Down
DROP INDEX IF EXISTS idx_migration_transfer_sets_source;
DROP INDEX IF EXISTS idx_migration_transfer_sets_kind_status_fork;
ALTER TABLE migration_transfer_sets
  DROP COLUMN IF EXISTS fork_spawn_id,
  DROP COLUMN IF EXISTS source_spawn_id,
  DROP COLUMN IF EXISTS kind;

DROP INDEX IF EXISTS idx_spawns_parent_spawn_id;
ALTER TABLE spawns
  DROP COLUMN IF EXISTS forked_at,
  DROP COLUMN IF EXISTS parent_spawn_id;
