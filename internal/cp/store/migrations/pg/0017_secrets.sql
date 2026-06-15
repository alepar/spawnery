-- +goose Up
CREATE TABLE secrets (
  account_id       text   NOT NULL REFERENCES owners(id) ON DELETE CASCADE,
  secret_id        text   NOT NULL,
  type             text   NOT NULL CHECK (type IN ('github-token', 'inference-key', 'generic-kv')),
  name             text   NOT NULL,
  provider         text   NOT NULL DEFAULT '',
  target_container bigint NOT NULL CHECK (target_container IN (1, 2)),
  env_var_name     text   NOT NULL DEFAULT '',
  dest_path        text   NOT NULL DEFAULT '',
  version          bigint NOT NULL CHECK (version >= 1),
  deviceset_epoch  bigint NOT NULL DEFAULT 0,
  envelope         bytea  NOT NULL,
  created_at       bigint NOT NULL,
  updated_at       bigint NOT NULL,
  PRIMARY KEY (account_id, secret_id),
  CHECK (env_var_name <> '' OR dest_path <> '')
);

CREATE INDEX secrets_account_updated_idx ON secrets(account_id, updated_at DESC);
CREATE INDEX secrets_account_epoch_idx ON secrets(account_id, deviceset_epoch, secret_id);

-- +goose Down
DROP TABLE secrets;
