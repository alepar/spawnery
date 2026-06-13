-- +goose Up
ALTER TABLE spawns ADD COLUMN status_seq   INTEGER NOT NULL DEFAULT 0;
ALTER TABLE spawns ADD COLUMN claim_holder  TEXT;
ALTER TABLE spawns ADD COLUMN claim_lease_id TEXT;
ALTER TABLE spawns ADD COLUMN claim_deadline INTEGER;

-- +goose Down
ALTER TABLE spawns DROP COLUMN claim_deadline;
ALTER TABLE spawns DROP COLUMN claim_lease_id;
ALTER TABLE spawns DROP COLUMN claim_holder;
ALTER TABLE spawns DROP COLUMN status_seq;
