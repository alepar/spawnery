-- +goose Up
-- The GitHub credential CONDITION (sp-2tx8.9 §4.1): '' | 'ok' | 'stale' | 'relink_required'.
-- Deliberately NOT a lifecycle status — the spawn stays 'active' while its token is stale; it is
-- still healthy for everything that is not git. '' = never reported (no GitHub mount).
ALTER TABLE spawns ADD COLUMN github_credential_status TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE spawns DROP COLUMN github_credential_status;
