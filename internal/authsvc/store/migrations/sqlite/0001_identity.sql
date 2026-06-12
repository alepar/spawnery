-- +goose Up
CREATE TABLE users (
  account_id TEXT PRIMARY KEY,
  github_sub INTEGER NOT NULL UNIQUE,  -- GitHub immutable numeric id, never login [AM9]
  handle     TEXT NOT NULL,            -- display-only
  status     TEXT NOT NULL CHECK (status IN ('active','disabled')),
  created_at INTEGER NOT NULL
);

CREATE TABLE refresh_sessions (
  token_hash          TEXT PRIMARY KEY,                       -- sha256(token) hex
  account_id          TEXT NOT NULL REFERENCES users(account_id),
  family_id           TEXT NOT NULL,
  client_kind         TEXT NOT NULL CHECK (client_kind IN ('web','cli')),
  session_pubkey_spki BLOB NOT NULL,                          -- [AM5] PoP material, raw DER SPKI
  access_token_id     TEXT NOT NULL,                          -- minted alongside (revocation feed payload)
  created_at          INTEGER NOT NULL,
  last_used_at        INTEGER NOT NULL,
  expires_at          INTEGER NOT NULL,                       -- 30d sliding
  family_created_at   INTEGER NOT NULL,                       -- [AM6] 90d absolute
  superseded_by       TEXT,
  superseded_at       INTEGER,                                -- grace-window anchor [AM3]
  successor_cache     TEXT,                                   -- cached successor pair JSON [AM3]
  revoked             INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_refresh_family  ON refresh_sessions(family_id);
CREATE INDEX idx_refresh_account ON refresh_sessions(account_id, family_created_at);

CREATE TABLE oauth_states (
  state               TEXT PRIMARY KEY,
  flow_cookie_hash    TEXT NOT NULL,  -- [AM8] binds the callback to the initiating browser session
  client_challenge    TEXT NOT NULL,
  client_redirect_uri TEXT NOT NULL,
  client_state        TEXT NOT NULL,
  gh_verifier         TEXT NOT NULL,
  created_at          INTEGER NOT NULL,
  expires_at          INTEGER NOT NULL,
  used                INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE auth_codes (
  code_hash    TEXT PRIMARY KEY,
  account_id   TEXT NOT NULL,
  challenge    TEXT NOT NULL,
  redirect_uri TEXT NOT NULL,
  expires_at   INTEGER NOT NULL,
  used         INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE device_grants (
  device_code_hash    TEXT PRIMARY KEY,
  user_code           TEXT NOT NULL UNIQUE,
  session_pubkey_spki BLOB NOT NULL,  -- [AM7] pubkey posted at device-authorization
  status              TEXT NOT NULL CHECK (status IN ('pending','approved','denied','redeemed','expired')),
  account_id          TEXT,
  attempt_count       INTEGER NOT NULL DEFAULT 0,
  created_at          INTEGER NOT NULL,
  expires_at          INTEGER NOT NULL,
  last_polled_at      INTEGER
);

CREATE TABLE revocation_events (
  seq        INTEGER PRIMARY KEY AUTOINCREMENT,
  account_id TEXT NOT NULL,
  family_id  TEXT NOT NULL,
  token_ids  TEXT NOT NULL,  -- JSON array of access-token token_ids
  revoked_at INTEGER NOT NULL
);
