-- +goose Up
ALTER TABLE github_links ADD COLUMN pending_refresh_token TEXT;
ALTER TABLE github_links ADD COLUMN pending_refresh_expires_at_unix INTEGER;
ALTER TABLE github_links ADD COLUMN pending_access_token TEXT;
ALTER TABLE github_links ADD COLUMN pending_access_expires_at_unix INTEGER;
ALTER TABLE github_links ADD COLUMN pending_token_type TEXT;
ALTER TABLE github_links ADD COLUMN pending_version INTEGER;
ALTER TABLE github_links ADD COLUMN relink_required INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE github_links DROP COLUMN pending_refresh_token;
ALTER TABLE github_links DROP COLUMN pending_refresh_expires_at_unix;
ALTER TABLE github_links DROP COLUMN pending_access_token;
ALTER TABLE github_links DROP COLUMN pending_access_expires_at_unix;
ALTER TABLE github_links DROP COLUMN pending_token_type;
ALTER TABLE github_links DROP COLUMN pending_version;
ALTER TABLE github_links DROP COLUMN relink_required;
