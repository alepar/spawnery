package store

import (
	"context"
	"database/sql"
	"testing"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/sqlitedialect"
	_ "modernc.org/sqlite"
)

// Proves the chosen stack wires up: modernc driver "sqlite" + Bun sqlitedialect on :memory:.
func TestBunModerncSmoke(t *testing.T) {
	sqldb, err := sql.Open("sqlite", "file:smoke?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer sqldb.Close()
	db := bun.NewDB(sqldb, sqlitedialect.New())

	var one int
	if err := db.NewSelect().ColumnExpr("1").Scan(context.Background(), &one); err != nil {
		t.Fatal(err)
	}
	if one != 1 {
		t.Fatalf("got %d", one)
	}
}
