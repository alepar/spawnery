package auth

import (
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type feedSignerRevocations struct{ revoked atomic.Bool }

func (r *feedSignerRevocations) Generation() uint64 {
	if r.revoked.Load() {
		return 1
	}
	return 0
}
func (r *feedSignerRevocations) RejectSigner(*x509.Certificate) error {
	if r.revoked.Load() {
		return errors.New("signer revoked")
	}
	return nil
}

// fakeDoer simulates the AS revocation HTTP endpoint.
type fakeDoer struct {
	responses []fakeResponse
	calls     int32
	request   *http.Request
}

type fakeResponse struct {
	status  int
	entries []SignedFeedEntry
}

func (f *fakeDoer) Do(req *http.Request) (*http.Response, error) {
	f.request = req.Clone(req.Context())
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
	fixture := newArtifactFixture(t)

	sessions := NewSessionRegistry()
	var cancelled int32
	release := sessions.Add("tok-live", "acct-live", func() { atomic.AddInt32(&cancelled, 1) })
	defer release()

	revreg := NewRevocationRegistry(sessions)

	entry := signedEntry(t, fixture.credential, 1, "acct-live", []string{"tok-live"})
	doer := &fakeDoer{responses: []fakeResponse{{status: 200, entries: []SignedFeedEntry{entry}}}}

	poller := NewFeedPoller(doer, "http://fake/revocations", fixture.verifier, revreg, time.Minute)
	poller.now = func() time.Time { return testNow }
	ctx := t.Context()
	if err := poller.pollOnce(ctx); err != nil {
		t.Fatalf("pollOnce: %v", err)
	}

	// Checkpoint should advance.
	if poller.checkpoint != 1 {
		t.Errorf("checkpoint: got %d want 1", poller.checkpoint)
	}
	if got := doer.request.Header.Get("Authorization"); got != "" {
		t.Fatalf("revocation feed sent bearer authorization %q", got)
	}
	if got := doer.request.Header.Get("X-Spawnery-AS-" + "Secret"); got != "" {
		t.Fatalf("revocation feed sent retired service secret %q", got)
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
	fixture := newArtifactFixture(t)
	revreg := NewRevocationRegistry(nil)

	entries := []SignedFeedEntry{
		signedEntry(t, fixture.credential, 10, "a1", []string{"t1"}),
		signedEntry(t, fixture.credential, 20, "a2", []string{"t2"}),
	}
	doer := &fakeDoer{responses: []fakeResponse{{status: 200, entries: entries}}}
	poller := NewFeedPoller(doer, "http://fake/revocations", fixture.verifier, revreg, time.Minute)
	poller.now = func() time.Time { return testNow }
	ctx := t.Context()
	if err := poller.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if poller.checkpoint != 20 {
		t.Errorf("checkpoint: got %d want 20", poller.checkpoint)
	}
}

func TestFeedPoller_PollOnce_BadEntry_NoCheckpointCorruption(t *testing.T) {
	fixture := newArtifactFixture(t)
	evil := newArtifactFixture(t)
	revreg := NewRevocationRegistry(nil)

	goodEntry := signedEntry(t, fixture.credential, 5, "acct-good", []string{"tok-good"})
	badEntry := signedEntry(t, evil.credential, 6, "acct-bad", []string{"tok-bad"})

	doer := &fakeDoer{responses: []fakeResponse{{status: 200, entries: []SignedFeedEntry{goodEntry, badEntry}}}}
	poller := NewFeedPoller(doer, "http://fake/revocations", fixture.verifier, revreg, time.Minute)
	poller.now = func() time.Time { return testNow }
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
	fixture := newArtifactFixture(t)
	doer := &fakeDoer{responses: []fakeResponse{{status: 401, entries: nil}}}
	poller := NewFeedPoller(doer, "http://fake/revocations", fixture.verifier, revreg, time.Minute)
	ctx := t.Context()
	err := poller.pollOnce(ctx)
	if err == nil {
		t.Fatal("expected error for 401 response")
	}
	if poller.checkpoint != 0 {
		t.Errorf("checkpoint should not advance on error: %d", poller.checkpoint)
	}
}

func TestFeedPollerObservesSignerRevocationWithoutRestart(t *testing.T) {
	revocations := &feedSignerRevocations{}
	fixture := newArtifactFixtureWithRevocations(t, revocations)
	registry := NewRevocationRegistry(nil)
	first := signedEntry(t, fixture.credential, 1, "acct-1", []string{"tok-1"})
	second := signedEntry(t, fixture.credential, 2, "acct-2", []string{"tok-2"})
	doer := &fakeDoer{responses: []fakeResponse{{status: 200, entries: []SignedFeedEntry{first}}, {status: 200, entries: []SignedFeedEntry{second}}}}
	poller := NewFeedPoller(doer, "http://fake/revocations", fixture.verifier, registry, time.Minute)
	poller.now = func() time.Time { return testNow }
	if err := poller.pollOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	revocations.revoked.Store(true)
	if err := poller.pollOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if poller.checkpoint != 1 || registry.IsRevoked("tok-2", "") {
		t.Fatalf("revoked signer advanced feed: checkpoint=%d token2=%v", poller.checkpoint, registry.IsRevoked("tok-2", ""))
	}
}
