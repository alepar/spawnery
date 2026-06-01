package store

import (
	"context"
	"testing"
)

// Proves every model maps to its migrated table (a wrong tag fails the SELECT).
func TestModelsBindToTables(t *testing.T) {
	st := NewTestStore(t)
	bs := st.(*bunStore)
	ctx := context.Background()
	mustCount := func(model interface{}) {
		if _, err := bs.db.NewSelect().Model(model).Count(ctx); err != nil {
			t.Fatalf("%T: %v", model, err)
		}
	}
	mustCount((*Owner)(nil))
	mustCount((*App)(nil))
	mustCount((*AppVersion)(nil))
	mustCount((*MountDecl)(nil))
	mustCount((*Spawn)(nil))
	mustCount((*Container)(nil))
	mustCount((*Mount)(nil))
}
