-- +goose Up
-- Real revocation (sp-mwco.3.2 §4.2): delete removes only the customization_catalog row, never
-- the Garage object, and an already-bound spawn replays from its own persisted spawn_artifacts
-- row (object_key + sha) at resume/fork WITHOUT re-consulting the catalog. So a deleted skill
-- stays fetchable by every spawn that ever bound it. This table is the kill switch: keyed by
-- sha256 (content identity — what a spawn replays from), NOT catalog_id, since the same object
-- can be reachable from many catalog rows / bundle members / owners, and it must outlive a
-- deleted catalog row. Consulted by presignNodeArtifacts (internal/cp/artifacts.go) on every
-- start path; no FK to customization_catalog by design.
CREATE TABLE skill_object_denylist (
  sha256     TEXT    NOT NULL PRIMARY KEY,
  reason     TEXT    NOT NULL DEFAULT '',
  denied_by  TEXT    NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL
);

-- +goose Down
DROP TABLE skill_object_denylist;
