-- +goose Up
-- Per-member exclude/rename overrides on a bundle_ref profile entry (sp-mwco.1.8 §4.4): one JSON
-- column holding {"exclude":["skills/foo"],"rename":{"skills/bar":"bar-2"}}, keyed by the member's
-- source_subdir — the stable member identity across versions (§4.3; catalog_id is not stable
-- under content dedup, and Name is upstream-controlled). '' for non-bundle entries and for a
-- bundle_ref entry with no overrides. Decoded into ProfileEntry.ExcludeSubdirs/RenameSubdirs
-- (bun:"-") by decodeProfileEntry; see store.EncodeBundleOverrides for the encode side.
ALTER TABLE profile_entries ADD COLUMN bundle_overrides TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE profile_entries DROP COLUMN bundle_overrides;
