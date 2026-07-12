-- +goose Up
-- Global AS-published deny-list of node ids that sealing clients must refuse.
CREATE TABLE node_revocations (
  node_id    TEXT PRIMARY KEY,
  reason     TEXT NOT NULL DEFAULT '',
  revoked_at INTEGER NOT NULL
);
CREATE INDEX idx_node_revocations_revoked_at ON node_revocations(revoked_at, node_id);
