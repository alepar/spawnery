-- +goose Up
CREATE TABLE github_links (
  secret_id               TEXT PRIMARY KEY,
  account_id              TEXT NOT NULL,
  host                    TEXT NOT NULL,
  login                   TEXT NOT NULL,
  github_user_id          TEXT NOT NULL,
  app_client_id           TEXT NOT NULL,
  refresh_token           TEXT NOT NULL,
  refresh_expires_at_unix INTEGER NOT NULL,
  access_token            TEXT,
  access_expires_at_unix  INTEGER,
  token_type              TEXT NOT NULL,
  version                 INTEGER NOT NULL,
  delivery_id             TEXT NOT NULL,
  updated_at              INTEGER NOT NULL,
  revoked                 INTEGER NOT NULL DEFAULT 0,
  revoked_at              INTEGER
);
CREATE INDEX idx_github_links_account ON github_links(account_id);
CREATE INDEX idx_github_links_revoked ON github_links(revoked);

-- +goose Down
DROP TABLE github_links;
