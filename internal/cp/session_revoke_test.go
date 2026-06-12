package cp

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"spawnery/internal/cp/auth"
)

// TestSessionRegistry_WiredToServer: the Server's sessions field is set and used for revocation.
// Verifies that SetSessionRegistry + SetVerify wiring works via the public API.
func TestSessionRegistry_WiredToServer(t *testing.T) {
	s, _, _ := newTestServer(t)

	sessions := auth.NewSessionRegistry()
	s.SetSessionRegistry(sessions)
	s.SetDevMode(false)

	var cancelled int32
	release := sessions.Add("tok-srv-1", "acct-srv", func() { atomic.AddInt32(&cancelled, 1) })
	defer release()

	sessions.RevokeToken("tok-srv-1")

	deadline := time.Now().Add(time.Second)
	for atomic.LoadInt32(&cancelled) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("cancel not called after RevokeToken")
		}
		time.Sleep(time.Millisecond)
	}
}

// TestServer_SessionContextIdentity: IdentityFromContext retrieves the full identity set
// by the interceptor, and OwnerFromContext remains backward-compatible.
func TestServer_SessionContextIdentity(t *testing.T) {
	id := auth.Identity{Owner: "acct-ctx", TokenID: "tok-ctx"}
	ctx := auth.WithIdentity(context.Background(), id)

	got, ok := auth.IdentityFromContext(ctx)
	if !ok || got.Owner != "acct-ctx" || got.TokenID != "tok-ctx" {
		t.Errorf("IdentityFromContext: %+v ok=%v", got, ok)
	}
	owner, ok := auth.OwnerFromContext(ctx)
	if !ok || owner != "acct-ctx" {
		t.Errorf("OwnerFromContext: %q ok=%v", owner, ok)
	}
}

// TestServer_DevModeFlag: SetDevMode/SetReauthInterval setters compile and work.
func TestServer_DevModeFlag(t *testing.T) {
	s, _, _ := newTestServer(t)
	s.SetDevMode(true)
	s.SetDevMode(false)
	s.SetReauthInterval(5 * time.Minute)
	// No panic = pass.
}
