-- +goose Up
DROP TABLE refresh_sessions;
CREATE TABLE refresh_sessions (
  token_hash TEXT PRIMARY KEY,
  account_id TEXT NOT NULL REFERENCES users(account_id),
  family_id TEXT NOT NULL,
  client_kind TEXT NOT NULL CHECK (client_kind IN ('web','cli')),
  session_pubkey_spki BLOB NOT NULL,
  cp_access_token_id TEXT NOT NULL,
  node_access_token_id TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  last_used_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  family_created_at INTEGER NOT NULL,
  superseded_by TEXT,
  superseded_at INTEGER,
  successor_cache TEXT,
  revoked INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_refresh_family ON refresh_sessions(family_id);
CREATE INDEX idx_refresh_account ON refresh_sessions(account_id, family_created_at);

-- +goose Down
DROP TABLE refresh_sessions;
CREATE TABLE refresh_sessions (
  token_hash TEXT PRIMARY KEY,
  account_id TEXT NOT NULL REFERENCES users(account_id),
  family_id TEXT NOT NULL,
  client_kind TEXT NOT NULL CHECK (client_kind IN ('web','cli')),
  session_pubkey_spki BLOB NOT NULL,
  access_token_id TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  last_used_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  family_created_at INTEGER NOT NULL,
  superseded_by TEXT,
  superseded_at INTEGER,
  successor_cache TEXT,
  revoked INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_refresh_family ON refresh_sessions(family_id);
CREATE INDEX idx_refresh_account ON refresh_sessions(account_id, family_created_at);
