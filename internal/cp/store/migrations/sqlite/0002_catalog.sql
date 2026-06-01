-- +goose Up
ALTER TABLE apps ADD COLUMN summary    TEXT    NOT NULL DEFAULT '';
ALTER TABLE apps ADD COLUMN tags       TEXT    NOT NULL DEFAULT '';
ALTER TABLE apps ADD COLUMN visibility TEXT    NOT NULL DEFAULT 'public' CHECK (visibility IN ('public','private'));
ALTER TABLE apps ADD COLUMN listed     INTEGER NOT NULL DEFAULT 1;

DROP INDEX idx_app_versions_reviewed;
ALTER TABLE app_versions ADD COLUMN tier TEXT NOT NULL DEFAULT 'unverified' CHECK (tier IN ('unverified','scanned','reviewed'));
UPDATE app_versions SET tier = 'reviewed' WHERE reviewed = 1;
ALTER TABLE app_versions DROP COLUMN reviewed;
CREATE INDEX idx_app_versions_tier ON app_versions(app_id, tier, created_at DESC);

-- +goose Down
DROP INDEX idx_app_versions_tier;
ALTER TABLE app_versions ADD COLUMN reviewed INTEGER NOT NULL DEFAULT 0;
UPDATE app_versions SET reviewed = 1 WHERE tier = 'reviewed';
ALTER TABLE app_versions DROP COLUMN tier;
CREATE INDEX idx_app_versions_reviewed ON app_versions(app_id, reviewed, created_at DESC);
ALTER TABLE apps DROP COLUMN listed;
ALTER TABLE apps DROP COLUMN visibility;
ALTER TABLE apps DROP COLUMN tags;
ALTER TABLE apps DROP COLUMN summary;
