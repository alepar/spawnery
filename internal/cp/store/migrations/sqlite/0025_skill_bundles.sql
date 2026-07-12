-- +goose NO TRANSACTION

-- +goose Up
-- Skill bundles (sp-mwco.1): a bundle is one ingested repo/ref/subdir; each ingest that changes
-- the member set or a member sha cuts a new version. Members are catalog rows tagged
-- bundle_member=true, keyed into their version by source_subdir (the repo directory is the
-- member's stable identity — catalog_id alone would let two identical skill dirs collapse and
-- would make an upstream rename undetectable).
CREATE TABLE skill_bundle (
  bundle_id     TEXT    NOT NULL PRIMARY KEY,
  creator_id    TEXT    NOT NULL,
  name          TEXT    NOT NULL DEFAULT '',
  source_url    TEXT    NOT NULL DEFAULT '',
  source_ref    TEXT    NOT NULL DEFAULT '',
  source_subdir TEXT    NOT NULL DEFAULT '',
  etag          TEXT    NOT NULL DEFAULT '',
  created_at    INTEGER NOT NULL,
  updated_at    INTEGER NOT NULL
);
-- The re-paste idempotency key. source_ref/source_subdir MUST be NOT NULL DEFAULT '' (not NULL):
-- NULLs are distinct in a unique index and never `=` in a predicate, so a NULL-bearing key would
-- silently never match and re-pasting the same URL would mint a new bundle every time.
CREATE UNIQUE INDEX idx_skill_bundle_key ON skill_bundle(creator_id, source_url, source_ref, source_subdir);
CREATE INDEX idx_skill_bundle_creator ON skill_bundle(creator_id);

CREATE TABLE skill_bundle_version (
  version_id    TEXT    NOT NULL PRIMARY KEY,
  bundle_id     TEXT    NOT NULL REFERENCES skill_bundle(bundle_id) ON DELETE CASCADE,
  seq           INTEGER NOT NULL,
  source_commit TEXT    NOT NULL DEFAULT '',
  created_at    INTEGER NOT NULL
);
CREATE UNIQUE INDEX idx_skill_bundle_version_seq ON skill_bundle_version(bundle_id, seq);

-- No FK on catalog_id (see skill_bundles.go): the delete/reference check (sp-mwco.3.3) needs a
-- counts-only FailedPrecondition from an explicit reference scan, not a driver FK error.
CREATE TABLE skill_bundle_member (
  version_id    TEXT    NOT NULL REFERENCES skill_bundle_version(version_id) ON DELETE CASCADE,
  catalog_id    TEXT    NOT NULL,
  source_subdir TEXT    NOT NULL,
  position      INTEGER NOT NULL,
  PRIMARY KEY (version_id, source_subdir)
);
CREATE INDEX idx_skill_bundle_member_catalog ON skill_bundle_member(catalog_id);

-- customization_catalog: add the bundle-member kind flag + source_commit, and turn the
-- provenance columns NOT NULL DEFAULT '' (same re-paste-key reasoning as skill_bundle above).
-- sha256/size stay NULLable: inline/curated rows have no sha, and NOT NULL DEFAULT '' there would
-- collapse every inline row of one creator onto the same (creator_id, '') key and blow up
-- idx_customization_catalog_owner_sha. SQLite has no ALTER COLUMN, so this is a table rebuild.
PRAGMA foreign_keys = OFF;

CREATE TABLE customization_catalog_new (
  catalog_id    TEXT    NOT NULL PRIMARY KEY,
  creator_id    TEXT    NOT NULL,
  kind          TEXT    NOT NULL,
  name          TEXT    NOT NULL,
  description   TEXT    NOT NULL,
  content       BLOB,
  listed        INTEGER NOT NULL DEFAULT 1,
  created_at    INTEGER NOT NULL,
  updated_at    INTEGER NOT NULL,
  source_url    TEXT    NOT NULL DEFAULT '',
  source_ref    TEXT    NOT NULL DEFAULT '',
  source_subdir TEXT    NOT NULL DEFAULT '',
  sha256        TEXT,
  size          INTEGER,
  bundle_member INTEGER NOT NULL DEFAULT 0,
  source_commit TEXT    NOT NULL DEFAULT ''
);

INSERT INTO customization_catalog_new
  SELECT catalog_id, creator_id, kind, name, description, content, listed, created_at, updated_at,
         COALESCE(source_url, ''), COALESCE(source_ref, ''), COALESCE(source_subdir, ''),
         sha256, size, 0, ''
  FROM customization_catalog;

DROP TABLE customization_catalog;
ALTER TABLE customization_catalog_new RENAME TO customization_catalog;

CREATE INDEX idx_customization_catalog_creator ON customization_catalog(creator_id);
CREATE UNIQUE INDEX idx_customization_catalog_owner_sha ON customization_catalog(creator_id, sha256);

PRAGMA foreign_keys = ON;

-- +goose Down
PRAGMA foreign_keys = OFF;

DROP TABLE skill_bundle_member;
DROP TABLE skill_bundle_version;
DROP TABLE skill_bundle;

CREATE TABLE customization_catalog_old (
  catalog_id    TEXT    NOT NULL PRIMARY KEY,
  creator_id    TEXT    NOT NULL,
  kind          TEXT    NOT NULL,
  name          TEXT    NOT NULL,
  description   TEXT    NOT NULL,
  content       BLOB,
  listed        INTEGER NOT NULL DEFAULT 1,
  created_at    INTEGER NOT NULL,
  updated_at    INTEGER NOT NULL,
  source_url    TEXT,
  source_ref    TEXT,
  source_subdir TEXT,
  sha256        TEXT,
  size          INTEGER
);

INSERT INTO customization_catalog_old
  SELECT catalog_id, creator_id, kind, name, description, content, listed, created_at, updated_at,
         source_url, source_ref, source_subdir, sha256, size
  FROM customization_catalog;

DROP TABLE customization_catalog;
ALTER TABLE customization_catalog_old RENAME TO customization_catalog;

CREATE INDEX idx_customization_catalog_creator ON customization_catalog(creator_id);
CREATE UNIQUE INDEX idx_customization_catalog_owner_sha ON customization_catalog(creator_id, sha256);

PRAGMA foreign_keys = ON;
