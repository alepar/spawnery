-- +goose Up
CREATE TABLE secrets (
  account_id       TEXT    NOT NULL REFERENCES owners(id) ON DELETE CASCADE,
  secret_id        TEXT    NOT NULL,
  type             TEXT    NOT NULL CHECK (type IN ('github-token', 'inference-key', 'generic-kv')),
  name             TEXT    NOT NULL,
  provider         TEXT    NOT NULL DEFAULT '',
  target_container INTEGER NOT NULL CHECK (target_container IN (1, 2)),
  env_var_name     TEXT    NOT NULL DEFAULT '',
  dest_path        TEXT    NOT NULL DEFAULT '',
  version          INTEGER NOT NULL CHECK (version >= 1),
  deviceset_epoch  INTEGER NOT NULL DEFAULT 0,
  envelope         BLOB    NOT NULL,
  created_at       INTEGER NOT NULL,
  updated_at       INTEGER NOT NULL,
  PRIMARY KEY (account_id, secret_id),
  CHECK (env_var_name <> '' OR dest_path <> '')
);

CREATE INDEX secrets_account_updated_idx ON secrets(account_id, updated_at DESC);
CREATE INDEX secrets_account_epoch_idx ON secrets(account_id, deviceset_epoch, secret_id);

-- +goose Down
DROP TABLE secrets;
