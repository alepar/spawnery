-- +goose Up
-- Rebuild the legacy node-id-keyed deny-list so multiple rotated leaves for one node can remain
-- revoked. Empty certificate identities preserve legacy rows as compatibility-only entries.
CREATE TABLE node_revocations_v2 (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  node_id       TEXT NOT NULL,
  issuer_serial TEXT NOT NULL DEFAULT '',
  leaf_serial   TEXT NOT NULL DEFAULT '',
  reason        TEXT NOT NULL DEFAULT '',
  revoked_at    INTEGER NOT NULL
);
INSERT INTO node_revocations_v2 (node_id, reason, revoked_at)
  SELECT node_id, reason, revoked_at FROM node_revocations;
DROP TABLE node_revocations;
ALTER TABLE node_revocations_v2 RENAME TO node_revocations;
CREATE INDEX idx_node_revocations_revoked_at ON node_revocations(revoked_at, node_id);
CREATE INDEX idx_node_revocations_issuer ON node_revocations(issuer_serial, leaf_serial);
CREATE UNIQUE INDEX idx_node_revocations_certificate
  ON node_revocations(issuer_serial, leaf_serial)
  WHERE issuer_serial <> '' AND leaf_serial <> '';

CREATE TABLE node_revocation_crls (
  issuer_serial TEXT PRIMARY KEY,
  number        TEXT NOT NULL,
  pem           BLOB NOT NULL
);
