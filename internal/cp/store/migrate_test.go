package store

import (
	"context"
	"sort"
	"testing"
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
	want := []string{"agent_image_binaries", "agent_images", "app_version_mounts", "app_versions", "apps", "migration_transfer_sets", "owners", "spawn_artifacts", "spawn_containers", "spawn_mounts", "spawns"}
	if len(names) != len(want) {
		t.Fatalf("tables = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("tables = %v, want %v", names, want)
		}
	}
}
