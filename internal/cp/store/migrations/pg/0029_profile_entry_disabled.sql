-- +goose Up
-- Per-entry off switch (sp-mwco.2.8 §4.6): an explicit boolean, never an overloaded empty
-- targets list. Assembly skips a disabled entry entirely — no manifest artifact, no payload,
-- installed nowhere, for any agent. Unlike the old (removed) UI behaviour of unchecking every
-- agent to send targets=[], disabled is orthogonal to targets: Enable restores the entry with
-- its prior scope intact.
ALTER TABLE profile_entries ADD COLUMN disabled boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE profile_entries DROP COLUMN disabled;
