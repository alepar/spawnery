-- +goose Up
-- Add the internal 'forking' source-capture status and its durable capture deadline.
-- SQLite cannot alter CHECK constraints in place, so recreate the table.
PRAGMA foreign_keys = OFF;

CREATE TABLE spawns_new (
  id                    TEXT PRIMARY KEY,
  owner_id              TEXT NOT NULL REFERENCES owners(id),
  app_id                TEXT NOT NULL REFERENCES apps(id),
  app_version           TEXT NOT NULL,
  app_ref               TEXT NOT NULL,
  pinned                INTEGER NOT NULL DEFAULT 0,
  model                 TEXT NOT NULL,
  status                TEXT NOT NULL CHECK (status IN ('starting','active','suspending','suspended','resuming','forking','unreachable','error','deleted')),
  recovered             INTEGER NOT NULL DEFAULT 0,
  created_at            INTEGER NOT NULL,
  last_used_at          INTEGER NOT NULL,
  suspended_at          INTEGER,
  deleted_at            INTEGER,
  name                  TEXT NOT NULL DEFAULT '',
  image                 TEXT NOT NULL DEFAULT '',
  runnable_id           TEXT NOT NULL DEFAULT '',
  mode                  TEXT NOT NULL DEFAULT '',
  model_applied         INTEGER NOT NULL DEFAULT 1,
  model_apply_detail    TEXT NOT NULL DEFAULT '',
  base_image_digest     TEXT NOT NULL DEFAULT '',
  status_seq            INTEGER NOT NULL DEFAULT 0,
  claim_holder          TEXT,
  claim_lease_id        TEXT,
  claim_deadline        INTEGER,
  fork_capture_deadline INTEGER
);

INSERT INTO spawns_new
  SELECT id, owner_id, app_id, app_version, app_ref, pinned, model, status, recovered,
         created_at, last_used_at, suspended_at, deleted_at, name, image, runnable_id, mode,
         model_applied, model_apply_detail, base_image_digest, status_seq,
         claim_holder, claim_lease_id, claim_deadline, NULL
  FROM spawns;

DROP TABLE spawns;
ALTER TABLE spawns_new RENAME TO spawns;

CREATE INDEX idx_spawns_owner  ON spawns(owner_id, last_used_at DESC);
CREATE INDEX idx_spawns_status ON spawns(status);

PRAGMA foreign_keys = ON;

-- +goose Down
PRAGMA foreign_keys = OFF;

CREATE TABLE spawns_old (
  id                 TEXT PRIMARY KEY,
  owner_id           TEXT NOT NULL REFERENCES owners(id),
  app_id             TEXT NOT NULL REFERENCES apps(id),
  app_version        TEXT NOT NULL,
  app_ref            TEXT NOT NULL,
  pinned             INTEGER NOT NULL DEFAULT 0,
  model              TEXT NOT NULL,
  status             TEXT NOT NULL CHECK (status IN ('starting','active','suspending','suspended','resuming','unreachable','error','deleted')),
  recovered          INTEGER NOT NULL DEFAULT 0,
  created_at         INTEGER NOT NULL,
  last_used_at       INTEGER NOT NULL,
  suspended_at       INTEGER,
  deleted_at         INTEGER,
  name               TEXT NOT NULL DEFAULT '',
  image              TEXT NOT NULL DEFAULT '',
  runnable_id        TEXT NOT NULL DEFAULT '',
  mode               TEXT NOT NULL DEFAULT '',
  model_applied      INTEGER NOT NULL DEFAULT 1,
  model_apply_detail TEXT NOT NULL DEFAULT '',
  base_image_digest  TEXT NOT NULL DEFAULT '',
  status_seq         INTEGER NOT NULL DEFAULT 0,
  claim_holder       TEXT,
  claim_lease_id     TEXT,
  claim_deadline     INTEGER
);

INSERT INTO spawns_old
  SELECT id, owner_id, app_id, app_version, app_ref, pinned, model, status, recovered,
         created_at, last_used_at, suspended_at, deleted_at, name, image, runnable_id, mode,
         model_applied, model_apply_detail, base_image_digest, status_seq,
         claim_holder, claim_lease_id, claim_deadline
  FROM spawns
  WHERE status != 'forking';

DROP TABLE spawns;
ALTER TABLE spawns_old RENAME TO spawns;

CREATE INDEX idx_spawns_owner  ON spawns(owner_id, last_used_at DESC);
CREATE INDEX idx_spawns_status ON spawns(status);

PRAGMA foreign_keys = ON;
