package node

import (
	"sync/atomic"
	"testing"
	"time"
)

type heldTimer struct{ callback func() }

func (t *heldTimer) Stop() bool { return true }

func TestSessionAuthReauthCannotReplaceExpiredRecordBeforeDelayedTimerRuns(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	var timers []*heldTimer
	r := newSessionAuthRegistryWithClock(func() time.Time { return now }, func(_ time.Duration, callback func()) sessionAuthTimer {
		timer := &heldTimer{callback: callback}
		timers = append(timers, timer)
		return timer
	})
	key := sessionAuthKey{spawnID: "sp", sessionID: "s", clientID: "c"}
	var closed atomic.Int32
	record := sessionAuthRecord{accountID: "alice", tokenID: "old", expiresAt: now.Add(time.Second), sessionKeyHash: []byte("key"), generation: 1, nodeID: "node"}
	r.register(key, record, func(string) { closed.Add(1) })
	now = record.expiresAt
	next := record
	next.tokenID = "new"
	next.expiresAt = now.Add(time.Minute)
	if replaced, _ := r.replace(key, next, "alice"); replaced {
		t.Fatal("replacement resurrected an attachment after its old signed deadline")
	}
	if closed.Load() != 1 || r.contains(key) {
		t.Fatalf("expired record close=%d present=%v timers=%d", closed.Load(), r.contains(key), len(timers))
	}
}

func TestSessionAuthOldTimerCannotCloseSameTokenReplacement(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	var timers []*heldTimer
	r := newSessionAuthRegistryWithClock(func() time.Time { return now }, func(_ time.Duration, callback func()) sessionAuthTimer {
		timer := &heldTimer{callback: callback}
		timers = append(timers, timer)
		return timer
	})
	key := sessionAuthKey{spawnID: "sp", sessionID: "s", clientID: "c"}
	var closed atomic.Int32
	record := sessionAuthRecord{accountID: "alice", tokenID: "same-token", expiresAt: now.Add(time.Minute), sessionKeyHash: []byte("key"), generation: 1, nodeID: "node"}
	r.register(key, record, func(string) { closed.Add(1) })
	next := record
	next.expiresAt = now.Add(2 * time.Minute)
	if replaced, _ := r.replace(key, next, "alice"); !replaced {
		t.Fatal("same-token replacement rejected")
	}
	timers[0].callback()
	if closed.Load() != 0 || !r.contains(key) {
		t.Fatalf("old timer closed replacement: close=%d present=%v", closed.Load(), r.contains(key))
	}
	timers[1].callback()
	if closed.Load() != 1 || r.contains(key) {
		t.Fatalf("current timer close=%d present=%v", closed.Load(), r.contains(key))
	}
}

func TestSessionAuthLateOlderOpenCannotReplaceNewerAttachment(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	var timers []*heldTimer
	r := newSessionAuthRegistryWithClock(func() time.Time { return now }, func(_ time.Duration, callback func()) sessionAuthTimer {
		timer := &heldTimer{callback: callback}
		timers = append(timers, timer)
		return timer
	})
	key := sessionAuthKey{spawnID: "sp", sessionID: "s", clientID: "c"}
	newer := sessionAuthRecord{accountID: "alice", expiresAt: now.Add(time.Hour), attachmentID: "new", attachmentSequence: 2}
	if !r.registerIfNewer(key, newer, func(string) {}) {
		t.Fatal("newer open rejected")
	}
	older := newer
	older.attachmentID = "old"
	older.attachmentSequence = 1
	if r.registerIfNewer(key, older, func(string) { t.Fatal("older open installed close callback") }) {
		t.Fatal("late older open replaced newer attachment")
	}
	if len(timers) != 1 {
		t.Fatalf("timers = %d, stale open created expiry damage", len(timers))
	}
	if attachment, ok := r.attachment(key); !ok || attachment != "new" {
		t.Fatalf("current attachment = %q/%v", attachment, ok)
	}
}

func TestSessionAuthReauthCancelsOldDeadline(t *testing.T) {
	r := newSessionAuthRegistry()
	key := sessionAuthKey{spawnID: "sp", sessionID: "s", clientID: "c"}
	var closed atomic.Int32
	r.register(key, sessionAuthRecord{accountID: "alice", tokenID: "old", expiresAt: time.Now().Add(40 * time.Millisecond), sessionKeyHash: []byte("key"), generation: 1, nodeID: "node"}, func(string) { closed.Add(1) })
	if replaced, _ := r.replace(key, sessionAuthRecord{accountID: "alice", tokenID: "new", expiresAt: time.Now().Add(time.Second), sessionKeyHash: []byte("key"), generation: 1, nodeID: "node"}, "alice"); !replaced {
		t.Fatal("valid replacement rejected")
	}
	time.Sleep(80 * time.Millisecond)
	if got := closed.Load(); got != 0 {
		t.Fatalf("old deadline closed replacement %d times", got)
	}
	r.close(key, "done")
	if got := closed.Load(); got != 1 {
		t.Fatalf("close count = %d", got)
	}
}

func TestSessionAuthInvalidReauthClosesOnlyAddressedClient(t *testing.T) {
	r := newSessionAuthRegistry()
	var first, second atomic.Int32
	a := sessionAuthKey{spawnID: "sp", sessionID: "s", clientID: "a"}
	b := sessionAuthKey{spawnID: "sp", sessionID: "s", clientID: "b"}
	rec := sessionAuthRecord{accountID: "alice", tokenID: "old", expiresAt: time.Now().Add(time.Second), sessionKeyHash: []byte("key"), generation: 1, nodeID: "node"}
	r.register(a, rec, func(string) { first.Add(1) })
	r.register(b, rec, func(string) { second.Add(1) })
	bad := rec
	bad.accountID = "mallory"
	if replaced, _ := r.replace(a, bad, "alice"); replaced {
		t.Fatal("wrong account replacement accepted")
	}
	if first.Load() != 1 || second.Load() != 0 || !r.contains(b) {
		t.Fatalf("addressed closes = %d/%d sibling=%v", first.Load(), second.Load(), r.contains(b))
	}
}

func TestSessionAuthExpiryClosesExactlyOnce(t *testing.T) {
	r := newSessionAuthRegistry()
	key := sessionAuthKey{spawnID: "sp", sessionID: "s", clientID: "c"}
	var closed atomic.Int32
	r.register(key, sessionAuthRecord{accountID: "alice", tokenID: "tok", expiresAt: time.Now().Add(20 * time.Millisecond), sessionKeyHash: []byte("key"), generation: 1, nodeID: "node"}, func(string) { closed.Add(1) })
	time.Sleep(60 * time.Millisecond)
	if replaced, _ := r.replace(key, sessionAuthRecord{accountID: "alice", tokenID: "new", expiresAt: time.Now().Add(time.Second), sessionKeyHash: []byte("key"), generation: 1, nodeID: "node"}, "alice"); replaced {
		t.Fatal("late reauthentication resurrected expired attachment")
	}
	r.close(key, "again")
	if got := closed.Load(); got != 1 {
		t.Fatalf("close count = %d", got)
	}
}
