-- +goose Up
-- Skill bundles (sp-mwco.1): a bundle is one ingested repo/ref/subdir; each ingest that changes
-- the member set or a member sha cuts a new version. Members are catalog rows tagged
-- bundle_member=true, keyed into their version by source_subdir (the repo directory is the
-- member's stable identity — catalog_id alone would let two identical skill dirs collapse and
-- would make an upstream rename undetectable).
CREATE TABLE skill_bundle (
  bundle_id     text   NOT NULL PRIMARY KEY,
  creator_id    text   NOT NULL,
  name          text   NOT NULL DEFAULT '',
  source_url    text   NOT NULL DEFAULT '',
  source_ref    text   NOT NULL DEFAULT '',
  source_subdir text   NOT NULL DEFAULT '',
  etag          text   NOT NULL DEFAULT '',
  created_at    bigint NOT NULL,
  updated_at    bigint NOT NULL
);
-- The re-paste idempotency key. source_ref/source_subdir MUST be NOT NULL DEFAULT '' (not NULL):
-- NULLs are distinct in a unique index and never `=` in a predicate, so a NULL-bearing key would
-- silently never match and re-pasting the same URL would mint a new bundle every time.
CREATE UNIQUE INDEX idx_skill_bundle_key ON skill_bundle(creator_id, source_url, source_ref, source_subdir);
CREATE INDEX idx_skill_bundle_creator ON skill_bundle(creator_id);

CREATE TABLE skill_bundle_version (
  version_id    text   NOT NULL PRIMARY KEY,
  bundle_id     text   NOT NULL REFERENCES skill_bundle(bundle_id) ON DELETE CASCADE,
  seq           bigint NOT NULL,
  source_commit text   NOT NULL DEFAULT '',
  created_at    bigint NOT NULL
);
CREATE UNIQUE INDEX idx_skill_bundle_version_seq ON skill_bundle_version(bundle_id, seq);

-- No FK on catalog_id (see skill_bundles.go): the delete/reference check (sp-mwco.3.3) needs a
-- counts-only FailedPrecondition from an explicit reference scan, not a driver FK error.
CREATE TABLE skill_bundle_member (
  version_id    text    NOT NULL REFERENCES skill_bundle_version(version_id) ON DELETE CASCADE,
  catalog_id    text    NOT NULL,
  source_subdir text    NOT NULL,
  position      integer NOT NULL,
  PRIMARY KEY (version_id, source_subdir)
);
CREATE INDEX idx_skill_bundle_member_catalog ON skill_bundle_member(catalog_id);

-- customization_catalog: add the bundle-member kind flag + source_commit, and turn the
-- provenance columns NOT NULL DEFAULT '' (same re-paste-key reasoning as skill_bundle above).
-- sha256/size stay NULLable: inline/curated rows have no sha, and NOT NULL DEFAULT '' there would
-- collapse every inline row of one creator onto the same (creator_id, '') key and blow up
-- idx_customization_catalog_owner_sha.
UPDATE customization_catalog SET
  source_url    = COALESCE(source_url, ''),
  source_ref    = COALESCE(source_ref, ''),
  source_subdir = COALESCE(source_subdir, '');

ALTER TABLE customization_catalog
  ALTER COLUMN source_url    SET DEFAULT '',
  ALTER COLUMN source_url    SET NOT NULL,
  ALTER COLUMN source_ref    SET DEFAULT '',
  ALTER COLUMN source_ref    SET NOT NULL,
  ALTER COLUMN source_subdir SET DEFAULT '',
  ALTER COLUMN source_subdir SET NOT NULL,
  ADD COLUMN bundle_member boolean NOT NULL DEFAULT false,
  ADD COLUMN source_commit text NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE customization_catalog
  ALTER COLUMN source_url    DROP NOT NULL,
  ALTER COLUMN source_url    DROP DEFAULT,
  ALTER COLUMN source_ref    DROP NOT NULL,
  ALTER COLUMN source_ref    DROP DEFAULT,
  ALTER COLUMN source_subdir DROP NOT NULL,
  ALTER COLUMN source_subdir DROP DEFAULT,
  DROP COLUMN bundle_member,
  DROP COLUMN source_commit;

DROP TABLE skill_bundle_member;
DROP TABLE skill_bundle_version;
DROP TABLE skill_bundle;
