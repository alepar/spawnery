package auth

import (
	"context"
	"testing"
)

func TestContextRoundTrip(t *testing.T) {
	ctx := WithOwner(context.Background(), "alice")
	if o, ok := OwnerFromContext(ctx); !ok || o != "alice" {
		t.Fatalf("ctx owner: %q ok=%v", o, ok)
	}
	if _, ok := OwnerFromContext(context.Background()); ok {
		t.Fatal("empty ctx should have no owner")
	}
}

func TestWithIdentity_ContextRoundTrip(t *testing.T) {
	id := Identity{Owner: "bob", TokenID: "tok-1"}
	ctx := WithIdentity(context.Background(), id)

	got, ok := IdentityFromContext(ctx)
	if !ok {
		t.Fatal("IdentityFromContext: not ok")
	}
	if got.Owner != "bob" || got.TokenID != "tok-1" {
		t.Errorf("got %+v", got)
	}

	// OwnerFromContext still works when set via WithIdentity.
	owner, ok := OwnerFromContext(ctx)
	if !ok || owner != "bob" {
		t.Errorf("OwnerFromContext after WithIdentity: %q ok=%v", owner, ok)
	}
}
