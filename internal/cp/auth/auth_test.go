package auth

import (
	"context"
	"testing"
)

func TestOwnerLookup(t *testing.T) {
	a := New(map[string]string{"dev-token": "alice"})
	if o, ok := a.Owner("dev-token"); !ok || o != "alice" {
		t.Fatalf("got %q ok=%v", o, ok)
	}
	if _, ok := a.Owner("bad"); ok {
		t.Fatal("bad token resolved")
	}
}

func TestContextRoundTrip(t *testing.T) {
	ctx := WithOwner(context.Background(), "alice")
	if o, ok := OwnerFromContext(ctx); !ok || o != "alice" {
		t.Fatalf("ctx owner: %q ok=%v", o, ok)
	}
	if _, ok := OwnerFromContext(context.Background()); ok {
		t.Fatal("empty ctx should have no owner")
	}
}
