package auth

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestSessionRegistry_RevokeToken(t *testing.T) {
	r := NewSessionRegistry()
	var cancelled int32

	release := r.Add("tok-1", "acct-1", func() { atomic.AddInt32(&cancelled, 1) })
	defer release()

	r.RevokeToken("tok-1")

	deadline := time.Now().Add(time.Second)
	for atomic.LoadInt32(&cancelled) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("cancel not called after RevokeToken")
		}
		time.Sleep(time.Millisecond)
	}
	if n := atomic.LoadInt32(&cancelled); n != 1 {
		t.Errorf("cancel called %d times, want 1", n)
	}
}

func TestSessionRegistry_RevokeToken_OnlyMatchingSession(t *testing.T) {
	r := NewSessionRegistry()
	var cancelledA, cancelledB int32

	releaseA := r.Add("tok-A", "acct-1", func() { atomic.AddInt32(&cancelledA, 1) })
	defer releaseA()
	releaseB := r.Add("tok-B", "acct-1", func() { atomic.AddInt32(&cancelledB, 1) })
	defer releaseB()

	r.RevokeToken("tok-A")
	time.Sleep(5 * time.Millisecond)

	if atomic.LoadInt32(&cancelledA) != 1 {
		t.Error("tok-A not cancelled")
	}
	if atomic.LoadInt32(&cancelledB) != 0 {
		t.Error("tok-B should NOT be cancelled")
	}
}

func TestSessionRegistry_RevokeAccount(t *testing.T) {
	r := NewSessionRegistry()
	var cancelledA, cancelledB int32

	releaseA := r.Add("tok-A", "acct-X", func() { atomic.AddInt32(&cancelledA, 1) })
	defer releaseA()
	releaseB := r.Add("tok-B", "acct-X", func() { atomic.AddInt32(&cancelledB, 1) })
	defer releaseB()

	r.RevokeAccount("acct-X")

	deadline := time.Now().Add(time.Second)
	for atomic.LoadInt32(&cancelledA) == 0 || atomic.LoadInt32(&cancelledB) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("not all sessions cancelled: A=%d B=%d",
				atomic.LoadInt32(&cancelledA), atomic.LoadInt32(&cancelledB))
		}
		time.Sleep(time.Millisecond)
	}
}

func TestSessionRegistry_Release_Removes(t *testing.T) {
	r := NewSessionRegistry()
	var calls int32
	release := r.Add("tok-1", "acct-1", func() { atomic.AddInt32(&calls, 1) })
	release() // remove session before revocation
	r.RevokeToken("tok-1")
	time.Sleep(5 * time.Millisecond)
	if atomic.LoadInt32(&calls) != 0 {
		t.Error("released session should not be cancelled by revocation")
	}
}

func TestSessionRegistry_DevSession_NotRevokedByToken(t *testing.T) {
	// Dev sessions have empty token_id; they should not be found by RevokeToken("").
	r := NewSessionRegistry()
	var calls int32
	release := r.Add("", "acct-dev", func() { atomic.AddInt32(&calls, 1) })
	defer release()

	r.RevokeToken("") // revoke empty token should not cancel dev sessions
	time.Sleep(5 * time.Millisecond)
	if atomic.LoadInt32(&calls) != 0 {
		t.Error("dev session should NOT be cancelled by RevokeToken(\"\")")
	}
}

func TestSessionRegistry_DevSession_CancelledByAccount(t *testing.T) {
	r := NewSessionRegistry()
	var calls int32
	release := r.Add("", "acct-dev", func() { atomic.AddInt32(&calls, 1) })
	defer release()

	r.RevokeAccount("acct-dev")
	deadline := time.Now().Add(time.Second)
	for atomic.LoadInt32(&calls) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("dev session not cancelled by account revocation")
		}
		time.Sleep(time.Millisecond)
	}
}
