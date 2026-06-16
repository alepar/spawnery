package store

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// NewTestStore returns a fresh :memory: store, migrated, isolated per test by name, closed on cleanup.
func NewTestStore(t *testing.T) Store {
	t.Helper()
	name := strings.NewReplacer("/", "_", "#", "_").Replace(t.Name())
	dsn := "file:as_" + name + "?mode=memory&cache=shared&_pragma=foreign_keys(1)"
	cipher, err := NewAESGCMTokenCipher(bytes.Repeat([]byte{0x2a}, 32))
	if err != nil {
		t.Fatalf("new test token cipher: %v", err)
	}
	st, err := Open(context.Background(), Config{Driver: "sqlite", DSN: dsn, TokenCipher: cipher})
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}
