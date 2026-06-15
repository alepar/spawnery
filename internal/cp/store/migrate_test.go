package store

import (
	"context"
	"database/sql"
	"sort"
	"testing"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"
)

func TestMigrationsCreateAllTables(t *testing.T) {
	st, err := Open(context.Background(), Config{
		Driver: "sqlite",
		DSN:    "file:migtest?mode=memory&cache=shared&_pragma=foreign_keys(1)",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	bs := st.(*bunStore)
	var names []string
	if err := bs.db.NewSelect().
		ColumnExpr("name").
		TableExpr("sqlite_master").
		Where("type = ?", "table").
		Where("name NOT LIKE ?", "sqlite_%").
		Where("name <> ?", "goose_db_version").
		Scan(context.Background(), &names); err != nil {
		t.Fatal(err)
	}
	sort.Strings(names)
	want := []string{"agent_image_binaries", "agent_images", "app_version_mounts", "app_versions", "apps", "customization_catalog", "migration_transfer_sets", "owners", "profile_entries", "profile_secrets", "profiles", "spawn_artifacts", "spawn_containers", "spawn_mounts", "spawns"}
	if len(names) != len(want) {
		t.Fatalf("tables = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("tables = %v, want %v", names, want)
		}
	}
}

func TestSQLiteDownForkingDeletesDependentRows(t *testing.T) {
	sqldb, err := sql.Open("sqlite", "file:"+t.Name()+"?mode=memory&cache=shared&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	defer sqldb.Close()

	goose.SetBaseFS(migrationsFS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatal(err)
	}
	if err := goose.UpTo(sqldb, "migrations/sqlite", 15); err != nil {
		t.Fatalf("migrate up to 15: %v", err)
	}

	execSQL := func(stmt string, args ...any) {
		t.Helper()
		if _, err := sqldb.Exec(stmt, args...); err != nil {
			t.Fatalf("exec %q: %v", stmt, err)
		}
	}
	execSQL("INSERT INTO owners (id, email, created_at) VALUES ('alice', '', 1)")
	execSQL("INSERT INTO apps (id, display_name, summary, tags, visibility, listed, created_at, creator_id) VALUES ('app', 'app', '', '', 'public', 1, 1, 'alice')")
	execSQL("INSERT INTO spawns (id, owner_id, app_id, app_version, app_ref, model, status, created_at, last_used_at, fork_capture_deadline) VALUES ('forking-spawn', 'alice', 'app', '1', 'ref', 'model', 'forking', 1, 1, 10)")
	execSQL("INSERT INTO spawns (id, owner_id, app_id, app_version, app_ref, model, status, created_at, last_used_at, fork_capture_deadline) VALUES ('active-spawn', 'alice', 'app', '1', 'ref', 'model', 'active', 1, 1, NULL)")
	for _, id := range []string{"forking-spawn", "active-spawn"} {
		execSQL("INSERT INTO spawn_containers (spawn_id, generation, node_id, phase, started_at) VALUES (?, 1, 'node', 'active', 1)", id)
		execSQL("INSERT INTO spawn_mounts (spawn_id, name, backend_uri) VALUES (?, 'main', 'scratch')", id)
		execSQL("INSERT INTO spawn_artifacts (spawn_id, artifact_id, dest_path) VALUES (?, 'artifact', '/tmp/artifact')", id)
		execSQL("INSERT INTO migration_transfer_sets (id, spawn_id, source_generation, target_generation, source_node_id, target_node_id, transfer_key_status, status, created_at, updated_at) VALUES (?, ?, 1, 2, 'source', 'target', 'pending', 'pending', 1, 1)", "ts-"+id, id)
	}

	if err := goose.DownTo(sqldb, "migrations/sqlite", 14); err != nil {
		t.Fatalf("migrate down to 14: %v", err)
	}

	assertCount := func(table, spawnID string, want int) {
		t.Helper()
		var got int
		if err := sqldb.QueryRow("SELECT COUNT(*) FROM "+table+" WHERE spawn_id = ?", spawnID).Scan(&got); err != nil {
			t.Fatalf("count %s for %s: %v", table, spawnID, err)
		}
		if got != want {
			t.Fatalf("%s rows for %s = %d, want %d", table, spawnID, got, want)
		}
	}
	for _, table := range []string{"spawn_containers", "spawn_mounts", "spawn_artifacts", "migration_transfer_sets"} {
		assertCount(table, "forking-spawn", 0)
		assertCount(table, "active-spawn", 1)
	}
	var forkRows int
	if err := sqldb.QueryRow("SELECT COUNT(*) FROM spawns WHERE id = 'forking-spawn'").Scan(&forkRows); err != nil {
		t.Fatal(err)
	}
	if forkRows != 0 {
		t.Fatalf("forking spawn rows after rollback = %d, want 0", forkRows)
	}
	rows, err := sqldb.Query("PRAGMA foreign_key_check")
	if err != nil {
		t.Fatalf("foreign_key_check: %v", err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("foreign_key_check returned violations after rollback")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("foreign_key_check rows: %v", err)
	}
}
