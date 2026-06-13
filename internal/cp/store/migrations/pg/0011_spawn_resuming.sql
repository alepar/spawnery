-- +goose Up
-- Add 'resuming' to the spawns.status CHECK constraint (sp-u53.7.5).
-- Drop the existing check (auto-named by Postgres) and recreate with the new value.
ALTER TABLE spawns DROP CONSTRAINT IF EXISTS spawns_status_check;
ALTER TABLE spawns ADD CONSTRAINT spawns_status_check
  CHECK (status IN ('starting','active','suspending','suspended','resuming','unreachable','error','deleted'));

-- +goose Down
ALTER TABLE spawns DROP CONSTRAINT IF EXISTS spawns_status_check;
ALTER TABLE spawns ADD CONSTRAINT spawns_status_check
  CHECK (status IN ('starting','active','suspending','suspended','unreachable','error','deleted'));
