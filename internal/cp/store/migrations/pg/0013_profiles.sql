-- +goose Up
CREATE TABLE profiles (
  profile_id text NOT NULL PRIMARY KEY,
  owner_id   text NOT NULL,
  name       text NOT NULL,
  version    bigint NOT NULL DEFAULT 1,
  updated_at bigint NOT NULL
);

CREATE INDEX idx_profiles_owner ON profiles(owner_id);

CREATE TABLE profile_entries (
  profile_id      text NOT NULL REFERENCES profiles(profile_id),
  entry_id        text NOT NULL,
  kind            text NOT NULL,
  name            text NOT NULL,
  source_kind     text NOT NULL,
  catalog_id      text NOT NULL DEFAULT '',
  custom_inline   bytea,
  targets         text NOT NULL DEFAULT '[]',
  mcp_secret_refs text NOT NULL DEFAULT '[]',
  PRIMARY KEY (profile_id, entry_id)
);

CREATE TABLE profile_secrets (
  profile_id text NOT NULL REFERENCES profiles(profile_id),
  secret_id  text NOT NULL,
  PRIMARY KEY (profile_id, secret_id)
);

-- +goose Down
DROP TABLE profile_secrets;
DROP TABLE profile_entries;
DROP TABLE profiles;
