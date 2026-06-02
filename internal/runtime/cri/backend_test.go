package cri

import (
	"context"
	"testing"
)

func TestPingAndPreflight(t *testing.T) {
	c, f := newFakeCRI(t)
	b := NewCRIPodBackend(c, "runsc")
	ctx := context.Background()

	if err := b.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if err := b.Preflight(ctx); err != nil {
		t.Fatalf("Preflight (ready): %v", err)
	}

	f.setNetworkReady(false)
	if err := b.Preflight(ctx); err == nil {
		t.Fatal("Preflight must fail when NetworkReady is false")
	}
}
