-- +goose Up
ALTER TABLE spawns ADD COLUMN status_seq    bigint NOT NULL DEFAULT 0;
ALTER TABLE spawns ADD COLUMN claim_holder  text;
ALTER TABLE spawns ADD COLUMN claim_lease_id text;
ALTER TABLE spawns ADD COLUMN claim_deadline bigint;

-- +goose Down
ALTER TABLE spawns DROP COLUMN claim_deadline;
ALTER TABLE spawns DROP COLUMN claim_lease_id;
ALTER TABLE spawns DROP COLUMN claim_holder;
ALTER TABLE spawns DROP COLUMN status_seq;
