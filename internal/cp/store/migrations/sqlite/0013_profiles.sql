-- +goose Up
CREATE TABLE profiles (
  profile_id TEXT NOT NULL PRIMARY KEY,
  owner_id   TEXT NOT NULL,
  name       TEXT NOT NULL,
  version    INTEGER NOT NULL DEFAULT 1,
  updated_at INTEGER NOT NULL
);

CREATE INDEX idx_profiles_owner ON profiles(owner_id);

CREATE TABLE profile_entries (
  profile_id      TEXT NOT NULL REFERENCES profiles(profile_id),
  entry_id        TEXT NOT NULL,
  kind            TEXT NOT NULL,
  name            TEXT NOT NULL,
  source_kind     TEXT NOT NULL,
  catalog_id      TEXT NOT NULL DEFAULT '',
  custom_inline   BLOB,
  targets         TEXT NOT NULL DEFAULT '[]',
  mcp_secret_refs TEXT NOT NULL DEFAULT '[]',
  PRIMARY KEY (profile_id, entry_id)
);

CREATE TABLE profile_secrets (
  profile_id TEXT NOT NULL REFERENCES profiles(profile_id),
  secret_id  TEXT NOT NULL,
  PRIMARY KEY (profile_id, secret_id)
);

-- +goose Down
DROP TABLE profile_secrets;
DROP TABLE profile_entries;
DROP TABLE profiles;
