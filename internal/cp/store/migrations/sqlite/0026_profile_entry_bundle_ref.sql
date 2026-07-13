-- +goose Up
-- bundle_ref profile entries (sp-mwco.1.5): a bundle_ref entry pins a skill_bundle_version and
-- expands to N per-member artifacts at assembly time (see profiles_assembly.go). catalog_id stays
-- empty ('' — already NOT NULL DEFAULT '', see types.go) for these entries; bundle_id/version_id
-- carry the pin instead.
ALTER TABLE profile_entries ADD COLUMN bundle_id  TEXT NOT NULL DEFAULT '';
ALTER TABLE profile_entries ADD COLUMN version_id TEXT NOT NULL DEFAULT '';
CREATE INDEX idx_profile_entries_version ON profile_entries(version_id);
CREATE INDEX idx_profile_entries_bundle  ON profile_entries(bundle_id);

-- +goose Down
DROP INDEX idx_profile_entries_bundle;
DROP INDEX idx_profile_entries_version;
ALTER TABLE profile_entries DROP COLUMN version_id;
ALTER TABLE profile_entries DROP COLUMN bundle_id;
