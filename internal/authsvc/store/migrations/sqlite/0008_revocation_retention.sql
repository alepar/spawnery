-- +goose Up
CREATE TABLE refresh_sessions_v8 (
  token_hash TEXT PRIMARY KEY,
  account_id TEXT NOT NULL REFERENCES users(account_id),
  family_id TEXT NOT NULL,
  client_kind TEXT NOT NULL CHECK (client_kind IN ('web','cli')),
  session_pubkey_spki BLOB NOT NULL,
  cp_access_token_id TEXT NOT NULL,
  node_access_token_id TEXT NOT NULL,
  access_expires_at INTEGER NOT NULL CHECK (access_expires_at > 0),
  created_at INTEGER NOT NULL,
  last_used_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  family_created_at INTEGER NOT NULL,
  superseded_by TEXT,
  superseded_at INTEGER,
  successor_cache TEXT,
  revoked INTEGER NOT NULL DEFAULT 0
);
INSERT INTO refresh_sessions_v8 (
  token_hash, account_id, family_id, client_kind, session_pubkey_spki,
  cp_access_token_id, node_access_token_id, access_expires_at, created_at,
  last_used_at, expires_at, family_created_at, superseded_by, superseded_at,
  successor_cache, revoked
)
SELECT token_hash, account_id, family_id, client_kind, session_pubkey_spki,
       cp_access_token_id, node_access_token_id, created_at + 900, created_at,
       last_used_at, expires_at, family_created_at, superseded_by, superseded_at,
       successor_cache, revoked
FROM refresh_sessions;
DROP TABLE refresh_sessions;
ALTER TABLE refresh_sessions_v8 RENAME TO refresh_sessions;
CREATE INDEX idx_refresh_family ON refresh_sessions(family_id);
CREATE INDEX idx_refresh_account ON refresh_sessions(account_id, family_created_at);
CREATE INDEX idx_refresh_access_expiry ON refresh_sessions(access_expires_at);

CREATE TEMP TABLE revocation_migration_validation (
  valid INTEGER NOT NULL CHECK (valid = 1)
);
INSERT INTO revocation_migration_validation(valid)
SELECT json_valid(token_ids) AND json_type(token_ids) = 'array'
FROM revocation_events;
INSERT INTO revocation_migration_validation(valid)
SELECT CASE WHEN type = 'text' AND value <> '' THEN 1 ELSE 0 END
FROM revocation_events, json_each(revocation_events.token_ids);

ALTER TABLE revocation_events RENAME TO revocation_events_v7;
CREATE TABLE revocation_events (
  seq INTEGER PRIMARY KEY AUTOINCREMENT,
  account_id TEXT NOT NULL,
  family_id TEXT NOT NULL,
  revoked_at INTEGER NOT NULL,
  revoke_tokens_issued_before INTEGER NOT NULL DEFAULT 0
);
INSERT INTO revocation_events(seq, account_id, family_id, revoked_at, revoke_tokens_issued_before)
SELECT seq, account_id, family_id, revoked_at, 0
FROM revocation_events_v7;

CREATE TABLE revocation_event_tokens (
  event_seq INTEGER NOT NULL REFERENCES revocation_events(seq) ON DELETE CASCADE,
  token_id TEXT NOT NULL,
  retain_until INTEGER NOT NULL CHECK (retain_until > 0),
  PRIMARY KEY(event_seq, token_id)
);
INSERT INTO revocation_event_tokens(event_seq, token_id, retain_until)
SELECT re.seq,
       CAST(j.value AS TEXT),
       COALESCE((
         SELECT MAX(rs.access_expires_at)
         FROM refresh_sessions rs
         WHERE rs.cp_access_token_id = CAST(j.value AS TEXT)
            OR rs.node_access_token_id = CAST(j.value AS TEXT)
       ), re.revoked_at + 900)
FROM revocation_events_v7 re, json_each(re.token_ids) j;
CREATE INDEX idx_revocation_token_expiry ON revocation_event_tokens(retain_until);
CREATE INDEX idx_revocation_token_id ON revocation_event_tokens(token_id);

DROP TABLE revocation_events_v7;
DROP TABLE revocation_migration_validation;

-- +goose Down
DROP TABLE revocation_event_tokens;
ALTER TABLE revocation_events RENAME TO revocation_events_v8;
CREATE TABLE revocation_events (
  seq INTEGER PRIMARY KEY AUTOINCREMENT,
  account_id TEXT NOT NULL,
  family_id TEXT NOT NULL,
  token_ids TEXT NOT NULL,
  revoked_at INTEGER NOT NULL
);
INSERT INTO revocation_events(seq, account_id, family_id, token_ids, revoked_at)
SELECT re.seq, re.account_id, re.family_id,
       COALESCE((SELECT json_group_array(ret.token_id) FROM revocation_event_tokens ret WHERE ret.event_seq = re.seq), '[]'),
       re.revoked_at
FROM revocation_events_v8 re;
DROP TABLE revocation_events_v8;

CREATE TABLE refresh_sessions_v7 (
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
INSERT INTO refresh_sessions_v7
SELECT token_hash, account_id, family_id, client_kind, session_pubkey_spki,
       cp_access_token_id, node_access_token_id, created_at, last_used_at,
       expires_at, family_created_at, superseded_by, superseded_at,
       successor_cache, revoked
FROM refresh_sessions;
DROP TABLE refresh_sessions;
ALTER TABLE refresh_sessions_v7 RENAME TO refresh_sessions;
CREATE INDEX idx_refresh_family ON refresh_sessions(family_id);
CREATE INDEX idx_refresh_account ON refresh_sessions(account_id, family_created_at);
