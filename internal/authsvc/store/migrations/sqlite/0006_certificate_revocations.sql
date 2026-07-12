-- +goose Up
-- Bind the legacy node-id deny-list to issued certificate identities and persist the latest
-- self-hosted CRL. Empty defaults preserve pre-existing rows as compatibility-only entries.
ALTER TABLE node_revocations ADD COLUMN issuer_serial TEXT NOT NULL DEFAULT '';
ALTER TABLE node_revocations ADD COLUMN leaf_serial TEXT NOT NULL DEFAULT '';
CREATE INDEX idx_node_revocations_issuer ON node_revocations(issuer_serial, leaf_serial);
CREATE UNIQUE INDEX idx_node_revocations_certificate
  ON node_revocations(issuer_serial, leaf_serial)
  WHERE issuer_serial <> '' AND leaf_serial <> '';

CREATE TABLE node_revocation_crls (
  issuer_serial TEXT PRIMARY KEY,
  number        TEXT NOT NULL,
  pem           BLOB NOT NULL
);
