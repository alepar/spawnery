# CP Store Driver Selection (sp-ylw) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:subagent-driven-development. Checkbox steps.

**Goal:** Let `cmd/cp` select sqlite vs postgres via `CP_STORE_DRIVER` (store already supports both).

**Source spec:** `docs/superpowers/specs/2026-06-01-cp-store-driver-sp-ylw.md`. CP-only, no codegen, no schema change. Branch `sp-ylw-store-driver` off master. Commit `--no-verify`.

## Task 1: `storeConfigFromEnv` helper + wiring + docs

**Files:** Modify `cmd/cp/main.go`; create `cmd/cp/main_test.go`; modify `deployment.md`.

- [ ] **Step 1: Failing test** — create `cmd/cp/main_test.go`:
```go
package main

import "testing"

func TestStoreConfigFromEnv(t *testing.T) {
	// default -> sqlite + sqlite DSN
	cfg, err := storeConfigFromEnv(func(string) string { return "" })
	if err != nil || cfg.Driver != "sqlite" || cfg.DSN != sqliteDefaultDSN {
		t.Fatalf("default = %+v err=%v", cfg, err)
	}
	// postgres + real DSN -> passthrough
	pg := map[string]string{"CP_STORE_DRIVER": "postgres", "CP_STORE_DSN": "postgres://u:p@h/db"}
	cfg, err = storeConfigFromEnv(func(k string) string { return pg[k] })
	if err != nil || cfg.Driver != "postgres" || cfg.DSN != "postgres://u:p@h/db" {
		t.Fatalf("pg = %+v err=%v", cfg, err)
	}
	// postgres + no DSN -> error
	if _, err := storeConfigFromEnv(func(k string) string { return map[string]string{"CP_STORE_DRIVER": "postgres"}[k] }); err == nil {
		t.Fatal("postgres without DSN must error")
	}
	// sqlite + custom DSN -> passthrough
	cfg, err = storeConfigFromEnv(func(k string) string { return map[string]string{"CP_STORE_DSN": "file:other.db"}[k] })
	if err != nil || cfg.Driver != "sqlite" || cfg.DSN != "file:other.db" {
		t.Fatalf("custom sqlite = %+v err=%v", cfg, err)
	}
}
```

- [ ] **Step 2: Confirm failure:** `go test ./cmd/cp/ 2>&1 | head` (no `storeConfigFromEnv`/`sqliteDefaultDSN`).

- [ ] **Step 3: Add the helper to `cmd/cp/main.go`** (add `"fmt"` to imports — currently not imported):
```go
const sqliteDefaultDSN = "file:cp.db?_pragma=busy_timeout(5000)"

func storeConfigFromEnv(get func(string) string) (store.Config, error) {
	driver := get("CP_STORE_DRIVER")
	if driver == "" {
		driver = "sqlite"
	}
	dsn := get("CP_STORE_DSN")
	if dsn == "" {
		dsn = sqliteDefaultDSN
	}
	if driver == "postgres" && (dsn == "" || dsn == sqliteDefaultDSN) {
		return store.Config{}, fmt.Errorf("CP_STORE_DRIVER=postgres requires CP_STORE_DSN (a postgres DSN)")
	}
	return store.Config{Driver: driver, DSN: dsn}, nil
}
```

- [ ] **Step 4: Wire `main`** — replace:
```go
	st, err := store.Open(ctx, store.Config{Driver: "sqlite", DSN: env("CP_STORE_DSN", "file:cp.db?_pragma=busy_timeout(5000)")})
	if err != nil {
		log.Fatalf("store open: %v", err)
	}
```
with:
```go
	storeCfg, err := storeConfigFromEnv(os.Getenv)
	if err != nil {
		log.Fatalf("store config: %v", err)
	}
	st, err := store.Open(ctx, storeCfg)
	if err != nil {
		log.Fatalf("store open: %v", err)
	}
```
(If the `env("CP_STORE_DSN", ...)` default string literal now lives only in `sqliteDefaultDSN`, fine. Leave the `env` helper for the other CP envs.)

- [ ] **Step 5: Run:** `go test ./cmd/cp/` — PASS. `go build ./...` — clean.

- [ ] **Step 6: Update `deployment.md`** — in the CP config table, change the `CP_STORE_DSN` row note and add a `CP_STORE_DRIVER` row:
  - Add row: `| `CP_STORE_DRIVER` | `sqlite` | `sqlite` (modernc, file/`:memory:`) or `postgres` (pgx). Postgres requires an explicit `CP_STORE_DSN`. |`
  - In the "Not-yet-prod" section, **remove** the bullet "Postgres driver wiring: the CP entrypoint hardcodes the sqlite driver (the store supports pg)." (it's now done).

- [ ] **Step 7: Commit:**
```bash
git add cmd/cp/main.go cmd/cp/main_test.go deployment.md
git commit --no-verify -m "feat(cp): CP_STORE_DRIVER env to select sqlite/postgres (sp-ylw)"
```

## Final Verification
- [ ] `go build ./... && go vet ./...` clean; `go test ./cmd/cp/ ./internal/cp/...` pass.

Then **superpowers:finishing-a-development-branch** (Option 1: merge locally).

## Self-Review
- Spec coverage: §1 env + guard → S3/S4; §3 tests → S1; docs → S6. ✓
- Types: `storeConfigFromEnv(func(string)string)(store.Config,error)`, `sqliteDefaultDSN`. ✓
- `fmt` import added (was absent). ✓
