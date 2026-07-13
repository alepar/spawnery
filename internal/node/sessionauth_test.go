package node

import (
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestExpiredAttachmentCloseCannotDetachReplacement(t *testing.T) {
	a := newAttacher(nil, &fakeCPStream{})
	key := sessionAuthKey{spawnID: "sp", sessionID: "s", clientID: "client"}
	pump := newPump(io.Discard, strings.NewReader(""))
	a.pumps[sessionKey{spawnID: "sp", sessionID: "s"}] = pump
	oldSender := &capSender{}
	if !a.attachClient("sp", "s", "client", 0) {
		t.Fatal("old client did not attach")
	}
	// Replace the production sender with one observable by this test.
	pump.attachClient("client", 0, oldSender.send)

	closeEntered := make(chan struct{})
	releaseClose := make(chan struct{})
	old := sessionAuthRecord{expiresAt: time.Now().Add(time.Hour), attachmentID: "old", attachmentSequence: 1}
	a.auths.register(key, old, func(reason string) {
		close(closeEntered)
		<-releaseClose
		a.closeClientAuthorization(key, 1, reason, "old")
	})
	closeDone := make(chan struct{})
	go func() {
		defer close(closeDone)
		a.auths.close(key, "expired")
	}()
	<-closeEntered

	newSender := &capSender{}
	replacement := old
	replacement.attachmentID = "new"
	replacement.attachmentSequence = 2
	a.auths.register(key, replacement, func(string) {})
	if !a.attachClient("sp", "s", "client", 0) {
		t.Fatal("replacement client did not attach")
	}
	pump.attachClient("client", 0, newSender.send)
	close(releaseClose)
	<-closeDone

	if attachment, ok := a.auths.attachment(key); !ok || attachment != "new" {
		t.Fatalf("replacement auth = %q/%v", attachment, ok)
	}
	if !pump.attached() {
		t.Fatal("expired attachment detached replacement transport")
	}
	pump.appendFrames([]Frame{{Kind: "agent", Text: "still-live"}})
	newSender.waitLen(t, 1)
	if got := newSender.frames()[0].Text; got != "still-live" {
		t.Fatalf("replacement frame = %q", got)
	}
	pump.detachClient("client")
}

func TestCurrentAttachmentCloseStillDetachesTransport(t *testing.T) {
	a := newAttacher(nil, &fakeCPStream{})
	key := sessionAuthKey{spawnID: "sp", sessionID: "s", clientID: "client"}
	pump := newPump(io.Discard, strings.NewReader(""))
	a.pumps[sessionKey{spawnID: "sp", sessionID: "s"}] = pump
	pump.attachClient("client", 0, (&capSender{}).send)
	a.auths.register(key, sessionAuthRecord{
		expiresAt: time.Now().Add(time.Hour), attachmentID: "current", attachmentSequence: 1,
	}, func(reason string) { a.closeClientAuthorization(key, 1, reason, "current") })
	a.auths.close(key, "expired")
	if a.auths.contains(key) || pump.attached() {
		t.Fatalf("current close left auth=%v transport=%v", a.auths.contains(key), pump.attached())
	}
}

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

func TestSessionAuthRevocationClosesMatchesAndRejectsLaterRegistration(t *testing.T) {
	store, err := OpenUserRevocationStore(filepath.Join(t.TempDir(), "state", "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	r := newSessionAuthRegistry(store)
	var tokenClosed, accountClosed, siblingClosed atomic.Int32
	record := func(account, token string) sessionAuthRecord {
		return sessionAuthRecord{accountID: account, tokenID: token, expiresAt: time.Now().Add(time.Hour), attachmentID: token, attachmentSequence: 1}
	}
	r.register(sessionAuthKey{spawnID: "sp", clientID: "token"}, record("alice", "old"), func(string) { tokenClosed.Add(1) })
	r.register(sessionAuthKey{spawnID: "sp", clientID: "account"}, record("bob", "fresh"), func(string) { accountClosed.Add(1) })
	r.register(sessionAuthKey{spawnID: "sp", clientID: "sibling"}, record("carol", "other"), func(string) { siblingClosed.Add(1) })
	batch := []VerifiedUserRevocation{{Seq: 1, AccountID: "alice", FamilyID: "family", TokenIDs: []string{"old"}}, {Seq: 2, AccountID: "bob", TokenIDs: []string{"unused"}}}
	if err := store.ApplyBatch(batch); err != nil {
		t.Fatal(err)
	}
	r.revoke(batch)
	if tokenClosed.Load() != 1 || accountClosed.Load() != 1 || siblingClosed.Load() != 0 {
		t.Fatalf("closes token=%d account=%d sibling=%d", tokenClosed.Load(), accountClosed.Load(), siblingClosed.Load())
	}
	if r.registerIfNewer(sessionAuthKey{spawnID: "sp2", clientID: "late"}, record("alice", "old"), func(string) {}) {
		t.Fatal("revoked open registered")
	}
}

func TestRevocationFeedOutageNeverExtendsSignedExpiry(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	store, err := OpenUserRevocationStore(filepath.Join(t.TempDir(), "state", "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	fixture := newArtifactFixture(t, now, "prod")
	consumer, err := NewRevocationConsumer(revocationDoer(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("AS unavailable")
	}), "https://as.internal/revocations", fixture.verifier, store, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := consumer.pollOnce(t.Context(), nil); err == nil {
		t.Fatal("outage unexpectedly succeeded")
	}
	var timers []*heldTimer
	r := newSessionAuthRegistryWithClock(func() time.Time { return now }, func(_ time.Duration, callback func()) sessionAuthTimer {
		timer := &heldTimer{callback: callback}
		timers = append(timers, timer)
		return timer
	}, store)
	key := sessionAuthKey{spawnID: "sp", clientID: "client"}
	var closed atomic.Int32
	record := sessionAuthRecord{accountID: "alice", tokenID: "token", expiresAt: now.Add(time.Minute), attachmentID: "attachment"}
	r.register(key, record, func(string) { closed.Add(1) })
	now = record.expiresAt
	timers[0].callback()
	if closed.Load() != 1 || r.contains(key) {
		t.Fatalf("expiry close=%d live=%v", closed.Load(), r.contains(key))
	}
	next := record
	next.tokenID = "replacement"
	next.expiresAt = now.Add(time.Hour)
	if replaced, _ := r.replace(key, next, "alice"); replaced {
		t.Fatal("outage allowed expired authorization replacement")
	}
}
