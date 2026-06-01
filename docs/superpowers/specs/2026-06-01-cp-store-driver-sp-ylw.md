# CP Store Driver Selection (sp-ylw) — Design

**Bead:** `sp-ylw` · **Status:** Draft v1 · **Date:** 2026-06-01

## 0. Context
`store.Open` already switches `Driver` (`"sqlite"` modernc / `"postgres"` pgx, both with goose
migration trees). But `cmd/cp/main.go` hardcodes `Driver: "sqlite"`, so prod can't run Postgres.
This slice adds the env knob. Tiny, CP-only.

## 1. Scope
- `cmd/cp` reads **`CP_STORE_DRIVER`** (default `"sqlite"`) and passes it to `store.Config`.
- Keep `CP_STORE_DSN` (default the sqlite file DSN). **Postgres requires an explicit `CP_STORE_DSN`**
  (a pgx DSN); the sqlite-file default won't open under pgx — fail loudly at boot is acceptable, but
  add a friendly guard: if `driver == "postgres"` and `CP_STORE_DSN` is empty/the sqlite default,
  `log.Fatal` with a clear "set CP_STORE_DSN to a postgres DSN" message.
- Extract a pure `storeConfigFromEnv(get func(string) string) (store.Config, error)` so it's
  hermetically unit-testable.
- Update `deployment.md` (drop the "hardcodes sqlite driver" TODO; document `CP_STORE_DRIVER`).

**Out:** connection pooling/tuning · a CI Postgres (pg path stays schema-tested via the `pgtest`
build tag) · migrating existing sqlite data to pg.

## 2. Design
```go
// in cmd/cp/main.go
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
`main` calls it with `os.Getenv`, `log.Fatal` on error, then `store.Open(ctx, cfg)`.

## 3. Testing (hermetic, `cmd/cp/main_test.go`)
- default → `{sqlite, sqliteDefaultDSN}`.
- `CP_STORE_DRIVER=postgres` + a real DSN → `{postgres, that DSN}`.
- `CP_STORE_DRIVER=postgres` + no DSN (or sqlite default) → error.
- `CP_STORE_DSN` override with sqlite driver → passthrough.

## 4. Decision log
| # | Decision | Choice |
|---|---|---|
| D.1 | Env | `CP_STORE_DRIVER` (default `sqlite`); `CP_STORE_DSN` unchanged |
| D.2 | Postgres + no DSN | `log.Fatal` at boot with a clear message (guarded in the pure helper) |
| D.3 | Testability | pure `storeConfigFromEnv(get)` helper, table-tested |
