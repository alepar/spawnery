package store

import (
	"context"
	"testing"
)

// NewTestStore returns a fresh :memory: store, migrated, isolated per test by name, closed on cleanup.
func NewTestStore(t *testing.T) Store {
	t.Helper()
	dsn := "file:" + t.Name() + "?mode=memory&cache=shared&_pragma=foreign_keys(1)"
	st, err := Open(context.Background(), Config{Driver: "sqlite", DSN: dsn})
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// inTx runs fn in a transaction and fails the test on error. For value-returning transitions,
// capture the value via a closure variable.
func inTx(t *testing.T, st Store, fn func(tx Store) error) {
	t.Helper()
	if err := st.WithTx(context.Background(), fn); err != nil {
		t.Fatalf("WithTx: %v", err)
	}
}
