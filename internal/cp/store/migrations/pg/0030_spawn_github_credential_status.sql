-- +goose Up
-- The GitHub credential CONDITION (sp-2tx8.9 §4.1): '' | 'ok' | 'stale' | 'relink_required'.
-- Deliberately NOT a lifecycle status — the spawn stays 'active' while its token is stale.
ALTER TABLE spawns ADD COLUMN github_credential_status text NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE spawns DROP COLUMN github_credential_status;
