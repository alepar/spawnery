package store

import (
	"context"
	"testing"
)

// TestPing_HealthyThenClosed proves Ping returns nil on an open store and non-nil after Close.
// This guarantees the store-down path surfaces an error to /readyz.
func TestPing_HealthyThenClosed(t *testing.T) {
	ctx := context.Background()
	st := NewTestStore(t)

	if err := st.Ping(ctx); err != nil {
		t.Fatalf("Ping on open store: %v", err)
	}

	// Close the pool; t.Cleanup registered by NewTestStore also calls Close, but a double-close
	// is harmless — the test must see the post-close error before cleanup runs.
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if err := st.Ping(ctx); err == nil {
		t.Fatal("Ping after Close: expected non-nil error, got nil")
	}
}
