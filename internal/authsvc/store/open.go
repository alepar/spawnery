package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"

	"github.com/pressly/goose/v3"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/sqlitedialect"
	_ "modernc.org/sqlite"
)

//go:embed migrations/sqlite/*.sql
var migrationsFS embed.FS

func openBun(cfg Config) (*bun.DB, error) {
	switch cfg.Driver {
	case "sqlite":
		sqldb, err := sql.Open("sqlite", cfg.DSN)
		if err != nil {
			return nil, err
		}
		// SQLite is single-writer; cap the pool to 1 connection so concurrent ops are serialized
		// at the driver level rather than fighting the retry mutex (R5 / AM3 ops note).
		sqldb.SetMaxOpenConns(1)
		if err := migrate(sqldb, "sqlite3", "migrations/sqlite"); err != nil {
			sqldb.Close()
			return nil, err
		}
		return bun.NewDB(sqldb, sqlitedialect.New()), nil
	default:
		return nil, fmt.Errorf("authsvc/store: unknown driver %q", cfg.Driver)
	}
}

func migrate(sqldb *sql.DB, dialect, dir string) error {
	// NOTE: goose's SetBaseFS/SetDialect/SetLogger mutate package-level globals and are NOT
	// goroutine-safe. Open must not be called concurrently (it's a single startup call today).
	// If concurrent Opens ever become needed, switch to the instance-based goose.NewProvider API.
	goose.SetBaseFS(migrationsFS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect(dialect); err != nil {
		return err
	}
	return goose.Up(sqldb, dir)
}

// Open returns a Store backed by the configured driver, with migrations applied.
func Open(ctx context.Context, cfg Config) (Store, error) {
	db, err := openBun(cfg)
	if err != nil {
		return nil, err
	}
	return &bunStore{db: db, closer: db}, nil
}
