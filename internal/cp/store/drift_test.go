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
	check("apps", "id", "display_name", "created_at")
	check("app_versions", "app_id", "version", "ref", "reviewed", "created_at")
	check("app_version_mounts", "app_id", "version", "name", "required")
	check("spawns", "id", "owner_id", "app_id", "app_version", "app_ref", "pinned", "model", "status", "recovered", "created_at", "last_used_at", "suspended_at", "deleted_at")
	check("spawn_containers", "spawn_id", "generation", "node_id", "phase", "started_at", "ended_at")
	check("spawn_mounts", "spawn_id", "name", "backend_uri", "persist_marker")
}
