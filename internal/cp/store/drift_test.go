package store

import (
	"context"
	"testing"
)

// Bun does not generate the DDL (goose does), so the struct tags and the SQL are independent.
// This asserts every column a model declares exists in its migrated table (catches a tag/DDL drift).
func TestSchemaDriftSqlite(t *testing.T) {
	st := NewTestStore(t)
	bs := st.(*bunStore)
	ctx := context.Background()

	cols := func(table string) map[string]bool {
		var rows []struct {
			Name string `bun:"name"`
		}
		if err := bs.db.NewRaw("SELECT name FROM pragma_table_info(?)", table).Scan(ctx, &rows); err != nil {
			t.Fatalf("pragma %s: %v", table, err)
		}
		set := map[string]bool{}
		for _, r := range rows {
			set[r.Name] = true
		}
		return set
	}
	check := func(table string, want ...string) {
		have := cols(table)
		for _, c := range want {
			if !have[c] {
				t.Fatalf("table %s missing column %s (have %v)", table, c, have)
			}
		}
	}
	check("owners", "id", "email", "created_at")
	check("apps", "id", "display_name", "summary", "tags", "visibility", "listed", "created_at", "creator_id")
	check("app_versions", "app_id", "version", "ref", "tier", "created_at", "manifest")
	check("app_version_mounts", "app_id", "version", "name", "required", "path", "seed")
	check("spawns", "id", "owner_id", "name", "app_id", "app_version", "app_ref", "pinned", "model", "image", "runnable_id", "mode", "status", "recovered", "created_at", "last_used_at", "suspended_at", "deleted_at", "status_seq", "claim_holder", "claim_lease_id", "claim_deadline", "fork_capture_deadline")
	check("spawn_containers", "spawn_id", "generation", "node_id", "phase", "started_at", "ended_at")
	check("spawn_mounts", "spawn_id", "name", "backend_uri", "persist_marker")
	check("spawn_artifacts", "spawn_id", "artifact_id", "inline", "content_type", "target_container", "dest_path", "mode", "sensitive", "env_var_name")
}

func TestSchemaDriftSqliteTypes(t *testing.T) {
	st := NewTestStore(t)
	bs := st.(*bunStore)
	ctx := context.Background()

	colType := func(table, col string) string {
		var rows []struct {
			Name string `bun:"name"`
			Type string `bun:"type"`
		}
		if err := bs.db.NewRaw("SELECT name, type FROM pragma_table_info(?)", table).Scan(ctx, &rows); err != nil {
			t.Fatalf("pragma %s: %v", table, err)
		}
		for _, r := range rows {
			if r.Name == col {
				return r.Type
			}
		}
		t.Fatalf("table %s has no column %s", table, col)
		return ""
	}
	// booleans are INTEGER, timestamps INTEGER, status/ids TEXT in the sqlite tree
	want := map[[2]string]string{
		{"spawns", "status"}:                "TEXT",
		{"spawns", "pinned"}:                "INTEGER",
		{"spawns", "recovered"}:             "INTEGER",
		{"spawns", "created_at"}:            "INTEGER",
		{"spawns", "status_seq"}:            "INTEGER",
		{"spawns", "claim_deadline"}:        "INTEGER",
		{"spawns", "fork_capture_deadline"}: "INTEGER",
		{"app_versions", "tier"}:            "TEXT",
		{"spawn_containers", "generation"}:  "INTEGER",
		{"spawn_containers", "ended_at"}:    "INTEGER",
	}
	for k, exp := range want {
		if got := colType(k[0], k[1]); got != exp {
			t.Fatalf("%s.%s type = %q, want %q", k[0], k[1], got, exp)
		}
	}
}
