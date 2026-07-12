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
	want := []string{"agent_image_binaries", "agent_images", "app_version_mounts", "app_versions", "apps", "customization_catalog", "migration_transfer_sets", "owners", "profile_entries", "profile_secrets", "profiles", "secrets", "skill_bundle", "skill_bundle_member", "skill_bundle_version", "spawn_artifacts", "spawn_containers", "spawn_mounts", "spawns"}
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
	if err := goose.UpTo(sqldb, "migrations/sqlite", 18); err != nil {
		t.Fatalf("migrate up to 18: %v", err)
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

	if err := goose.DownTo(sqldb, "migrations/sqlite", 17); err != nil {
		t.Fatalf("migrate down to 17: %v", err)
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

func TestSQLiteDownForkContractDropsAddedColumns(t *testing.T) {
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
	if err := goose.UpTo(sqldb, "migrations/sqlite", 19); err != nil {
		t.Fatalf("migrate up to 19: %v", err)
	}
	if err := goose.DownTo(sqldb, "migrations/sqlite", 18); err != nil {
		t.Fatalf("migrate down to 18: %v", err)
	}

	cols := func(table string) map[string]bool {
		t.Helper()
		rows, err := sqldb.Query("PRAGMA table_info(" + table + ")")
		if err != nil {
			t.Fatalf("table_info %s: %v", table, err)
		}
		defer rows.Close()
		out := map[string]bool{}
		for rows.Next() {
			var cid int
			var name, typ string
			var notnull int
			var dflt any
			var pk int
			if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
				t.Fatalf("scan table_info %s: %v", table, err)
			}
			out[name] = true
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("table_info rows %s: %v", table, err)
		}
		return out
	}
	spawns := cols("spawns")
	for _, col := range []string{"parent_spawn_id", "forked_at"} {
		if spawns[col] {
			t.Fatalf("spawns.%s still present after rollback", col)
		}
	}
	transferSets := cols("migration_transfer_sets")
	for _, col := range []string{"kind", "source_spawn_id", "fork_spawn_id"} {
		if transferSets[col] {
			t.Fatalf("migration_transfer_sets.%s still present after rollback", col)
		}
	}
}

// TestMigration0025BackfillsNullProvenance is the bead's "re-paste key matches" criterion at the
// SQL level: a pre-0025 catalog row with NULL source_ref/source_subdir must read back as empty after
// the migration, and must be selectable by the exact equality predicate the unique index and the
// repo's GetByKey lookup use — proving a NULL-bearing key can no longer silently never match.
func TestMigration0025BackfillsNullProvenance(t *testing.T) {
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
	if err := goose.UpTo(sqldb, "migrations/sqlite", 24); err != nil {
		t.Fatalf("migrate up to 24: %v", err)
	}

	if _, err := sqldb.Exec(
		`INSERT INTO customization_catalog
			(catalog_id, creator_id, kind, name, description, listed, created_at, updated_at,
			 source_url, source_ref, source_subdir, sha256, size)
		 VALUES (?, ?, 'skill', 'my-skill', 'desc', 1, 1, 1, ?, NULL, NULL, NULL, NULL)`,
		"cat-null-prov", "alice", "owner/repo",
	); err != nil {
		t.Fatalf("insert pre-migration row: %v", err)
	}

	if err := goose.UpTo(sqldb, "migrations/sqlite", 25); err != nil {
		t.Fatalf("migrate up to 25: %v", err)
	}

	var sourceRef, sourceSubdir string
	if err := sqldb.QueryRow(
		"SELECT source_ref, source_subdir FROM customization_catalog WHERE catalog_id = ?", "cat-null-prov",
	).Scan(&sourceRef, &sourceSubdir); err != nil {
		t.Fatalf("select backfilled row: %v", err)
	}
	if sourceRef != "" || sourceSubdir != "" {
		t.Fatalf("source_ref/source_subdir = %q/%q, want empty strings", sourceRef, sourceSubdir)
	}

	var catalogID string
	if err := sqldb.QueryRow(
		"SELECT catalog_id FROM customization_catalog WHERE source_ref = '' AND source_subdir = ''",
	).Scan(&catalogID); err != nil {
		t.Fatalf("select by empty-string predicate: %v", err)
	}
	if catalogID != "cat-null-prov" {
		t.Fatalf("catalog_id = %q, want cat-null-prov", catalogID)
	}
}

// TestMigration0025Down verifies the sqlite table-rebuild rollback is honest: DownTo(24) succeeds,
// drops the three bundle tables, and leaves customization_catalog rows intact.
func TestMigration0025Down(t *testing.T) {
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
	if err := goose.UpTo(sqldb, "migrations/sqlite", 25); err != nil {
		t.Fatalf("migrate up to 25: %v", err)
	}

	if _, err := sqldb.Exec(
		`INSERT INTO customization_catalog
			(catalog_id, creator_id, kind, name, description, listed, created_at, updated_at,
			 source_url, source_ref, source_subdir, sha256, size, bundle_member, source_commit)
		 VALUES (?, ?, 'skill', 'my-skill', 'desc', 1, 1, 1, 'owner/repo', '', '', NULL, NULL, 0, '')`,
		"cat-down", "alice",
	); err != nil {
		t.Fatalf("insert post-migration row: %v", err)
	}

	if err := goose.DownTo(sqldb, "migrations/sqlite", 24); err != nil {
		t.Fatalf("migrate down to 24: %v", err)
	}

	var name string
	if err := sqldb.QueryRow("SELECT name FROM customization_catalog WHERE catalog_id = ?", "cat-down").Scan(&name); err != nil {
		t.Fatalf("select surviving row: %v", err)
	}
	if name != "my-skill" {
		t.Fatalf("name = %q, want my-skill", name)
	}

	var tableCount int
	if err := sqldb.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name IN
			('skill_bundle', 'skill_bundle_version', 'skill_bundle_member')`,
	).Scan(&tableCount); err != nil {
		t.Fatal(err)
	}
	if tableCount != 0 {
		t.Fatalf("bundle tables remaining after rollback = %d, want 0", tableCount)
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
