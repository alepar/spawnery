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
	check("spawns", "id", "owner_id", "name", "app_id", "app_version", "app_ref", "pinned", "model", "image", "runnable_id", "mode", "status", "recovered", "created_at", "last_used_at", "suspended_at", "deleted_at", "status_seq", "claim_holder", "claim_lease_id", "claim_deadline", "fork_capture_deadline", "parent_spawn_id", "forked_at")
	check("spawn_containers", "spawn_id", "generation", "node_id", "phase", "started_at", "ended_at")
	check("spawn_mounts", "spawn_id", "name", "backend_uri", "credential_secret_id", "create_if_missing", "repository_id", "persist_marker")
	check("spawn_artifacts", "spawn_id", "artifact_id", "inline", "content_type", "target_container", "dest_path", "mode", "sensitive", "env_var_name")
	check("migration_transfer_sets", "id", "kind", "spawn_id", "source_spawn_id", "fork_spawn_id", "source_generation", "target_generation", "source_node_id", "target_node_id", "base_image_digest", "mount_manifest_pins", "rootfs_artifact_pins", "transfer_key_ciphertext_metadata", "transfer_key_status", "status", "created_at", "updated_at")
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
		{"spawns", "parent_spawn_id"}:       "TEXT",
		{"spawns", "forked_at"}:             "INTEGER",
		{"migration_transfer_sets", "kind"}: "TEXT",
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
