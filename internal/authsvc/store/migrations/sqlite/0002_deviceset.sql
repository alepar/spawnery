-- +goose Up
-- Device-set registry (sp-2ckv.3, WM1/WM9): append-only hash-chained entries
-- keyed on (account_id, version). The AS stores raw entry bytes and a
-- pre-computed head hash; it performs pure head comparison for CAS and never
-- validates member signatures (WM1: AS stores, never authors).
CREATE TABLE device_set_entries (
  account_id  TEXT    NOT NULL,
  version     INTEGER NOT NULL,          -- monotonic; genesis = 1
  prev_hash   BLOB,                      -- NULL for genesis
  head_hash   BLOB    NOT NULL,          -- encodeFields(Body, sigs...) chain hash
  entry_bytes BLOB    NOT NULL,          -- json.Marshal(StoredEntry) bytes
  created_at  INTEGER NOT NULL,
  PRIMARY KEY (account_id, version)
);
CREATE INDEX idx_dse_account ON device_set_entries(account_id, version DESC);
