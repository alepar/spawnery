-- +goose Up
-- Global AS-published deny-list of node ids that sealing clients must refuse.
CREATE TABLE node_revocations (
  node_id       TEXT PRIMARY KEY,
  issuer_serial TEXT NOT NULL,
  leaf_serial   TEXT NOT NULL,
  reason        TEXT NOT NULL DEFAULT '',
  revoked_at    INTEGER NOT NULL,
  UNIQUE (issuer_serial, leaf_serial)
);
CREATE INDEX idx_node_revocations_revoked_at ON node_revocations(revoked_at, node_id);
CREATE INDEX idx_node_revocations_issuer ON node_revocations(issuer_serial, leaf_serial);

CREATE TABLE node_revocation_crls (
  issuer_serial TEXT PRIMARY KEY,
  number        TEXT NOT NULL,
  pem           BLOB NOT NULL
);
