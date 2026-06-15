-- +goose Up
ALTER TABLE spawns ADD COLUMN fork_capture_deadline bigint;
ALTER TABLE spawns DROP CONSTRAINT IF EXISTS spawns_status_check;
ALTER TABLE spawns ADD CONSTRAINT spawns_status_check
  CHECK (status IN ('starting','active','suspending','suspended','resuming','forking','unreachable','error','deleted'));

-- +goose Down
ALTER TABLE spawns DROP CONSTRAINT IF EXISTS spawns_status_check;
ALTER TABLE spawns ADD CONSTRAINT spawns_status_check
  CHECK (status IN ('starting','active','suspending','suspended','resuming','unreachable','error','deleted'));
ALTER TABLE spawns DROP COLUMN fork_capture_deadline;
