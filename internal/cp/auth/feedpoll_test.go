package auth

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"spawnery/internal/authsvc/token"
)

// fakeDoer simulates the AS revocation HTTP endpoint.
type fakeDoer struct {
	responses []fakeResponse
	calls     int32
}

type fakeResponse struct {
	status  int
	entries []SignedFeedEntry
}

func (f *fakeDoer) Do(req *http.Request) (*http.Response, error) {
	idx := int(atomic.AddInt32(&f.calls, 1)) - 1
	if idx >= len(f.responses) {
		// No more responses — return empty list.
		body, _ := json.Marshal([]SignedFeedEntry{})
		return &http.Response{
			StatusCode: 200,
			Body:       io.NopCloser(strings.NewReader(string(body))),
		}, nil
	}
	r := f.responses[idx]
	body, _ := json.Marshal(r.entries)
	return &http.Response{
		StatusCode: r.status,
		Body:       io.NopCloser(strings.NewReader(string(body))),
	}, nil
}

func TestFeedPoller_PollOnce_AppliesEntries(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	ks, _ := token.NewKeySet(priv.Public().(ed25519.PublicKey))

	sessions := NewSessionRegistry()
	var cancelled int32
	release := sessions.Add("tok-live", "acct-live", func() { atomic.AddInt32(&cancelled, 1) })
	defer release()

	revreg := NewRevocationRegistry(sessions)

	entry := signedEntry(t, priv, 1, "acct-live", []string{"tok-live"})
	doer := &fakeDoer{responses: []fakeResponse{{status: 200, entries: []SignedFeedEntry{entry}}}}

	poller := NewFeedPoller(doer, "http://fake/revocations", "", ks, revreg, time.Minute)
	ctx := t.Context()
	if err := poller.pollOnce(ctx); err != nil {
		t.Fatalf("pollOnce: %v", err)
	}

	// Checkpoint should advance.
	if poller.checkpoint != 1 {
		t.Errorf("checkpoint: got %d want 1", poller.checkpoint)
	}

	// Session should be cancelled.
	deadline := time.Now().Add(time.Second)
	for atomic.LoadInt32(&cancelled) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("session not cancelled after feed poll")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestFeedPoller_PollOnce_AdvancesCheckpoint(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	ks, _ := token.NewKeySet(priv.Public().(ed25519.PublicKey))
	revreg := NewRevocationRegistry(nil)

	entries := []SignedFeedEntry{
		signedEntry(t, priv, 10, "a1", []string{"t1"}),
		signedEntry(t, priv, 20, "a2", []string{"t2"}),
	}
	doer := &fakeDoer{responses: []fakeResponse{{status: 200, entries: entries}}}
	poller := NewFeedPoller(doer, "http://fake/revocations", "", ks, revreg, time.Minute)
	ctx := t.Context()
	if err := poller.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if poller.checkpoint != 20 {
		t.Errorf("checkpoint: got %d want 20", poller.checkpoint)
	}
}

func TestFeedPoller_PollOnce_BadEntry_NoCheckpointCorruption(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	_, evil, _ := ed25519.GenerateKey(rand.Reader)
	ks, _ := token.NewKeySet(priv.Public().(ed25519.PublicKey))
	revreg := NewRevocationRegistry(nil)

	goodEntry := signedEntry(t, priv, 5, "acct-good", []string{"tok-good"})
	badEntry := signedEntry(t, evil, 6, "acct-bad", []string{"tok-bad"}) // signed by evil key

	doer := &fakeDoer{responses: []fakeResponse{{status: 200, entries: []SignedFeedEntry{goodEntry, badEntry}}}}
	poller := NewFeedPoller(doer, "http://fake/revocations", "", ks, revreg, time.Minute)
	ctx := t.Context()
	if err := poller.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}

	// Good entry applied; bad entry skipped.
	if !revreg.IsRevoked("tok-good", "") {
		t.Error("good entry should be applied")
	}
	if revreg.IsRevoked("tok-bad", "") {
		t.Error("bad entry must NOT be applied")
	}
	// Checkpoint: only advances past good entries (seq=5), bad entry (seq=6) skipped.
	if poller.checkpoint != 5 {
		t.Errorf("checkpoint: got %d want 5", poller.checkpoint)
	}
}

func TestFeedPoller_PollOnce_NonOKStatus(t *testing.T) {
	revreg := NewRevocationRegistry(nil)
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	ks, _ := token.NewKeySet(priv.Public().(ed25519.PublicKey))
	doer := &fakeDoer{responses: []fakeResponse{{status: 401, entries: nil}}}
	poller := NewFeedPoller(doer, "http://fake/revocations", "bad-bearer", ks, revreg, time.Minute)
	ctx := t.Context()
	err := poller.pollOnce(ctx)
	if err == nil {
		t.Fatal("expected error for 401 response")
	}
	if poller.checkpoint != 0 {
		t.Errorf("checkpoint should not advance on error: %d", poller.checkpoint)
	}
}
