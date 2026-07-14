package auth

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
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

type fakeDoer struct {
	responses []SignedFeedPage
	status    int
	requests  []*http.Request
}

type feedDoerFunc func(*http.Request) (*http.Response, error)

func (f feedDoerFunc) Do(req *http.Request) (*http.Response, error) { return f(req) }

func (f *fakeDoer) Do(req *http.Request) (*http.Response, error) {
	f.requests = append(f.requests, req.Clone(req.Context()))
	status := f.status
	if status == 0 {
		status = http.StatusOK
	}
	index := len(f.requests) - 1
	page := SignedFeedPage{Entries: []SignedFeedEntry{}}
	if index < len(f.responses) {
		page = f.responses[index]
	}
	body, _ := json.Marshal(page)
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
}

func TestFeedPollerDrainsBoundedPagesAndAcceptsForwardGaps(t *testing.T) {
	fixture := newArtifactFixture(t)
	registry := NewRevocationRegistry(nil)
	first := signedEntry(t, fixture.credential, familyEntry(2, "alice", "family", "one", testNow.Unix()-1, testNow.Unix()+30))
	second := signedEntry(t, fixture.credential, familyEntry(7, "alice", "family", "two", testNow.Unix()-1, testNow.Unix()+30))
	doer := &fakeDoer{responses: []SignedFeedPage{{Entries: []SignedFeedEntry{first}, HasMore: true}, {Entries: []SignedFeedEntry{second}}}}
	poller := NewFeedPoller(doer, "https://as/revocations", fixture.verifier, registry, time.Minute)
	poller.now = func() time.Time { return testNow }
	if err := poller.pollOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if poller.checkpoint != 7 || len(doer.requests) != 2 {
		t.Fatalf("checkpoint=%d requests=%d", poller.checkpoint, len(doer.requests))
	}
	for index, want := range []string{"https://as/revocations?limit=256&since=0", "https://as/revocations?limit=256&since=2"} {
		if got := doer.requests[index].URL.String(); got != want {
			t.Fatalf("request %d=%q want=%q", index, got, want)
		}
		if got := doer.requests[index].Header.Get("Authorization"); got != "" {
			t.Fatalf("request %d sent bearer authorization %q", index, got)
		}
		if got := doer.requests[index].Header.Get("X-Spawnery-AS-" + "Secret"); got != "" {
			t.Fatalf("request %d sent retired service secret %q", index, got)
		}
	}
	if !registry.IsRevoked("one", "", 0, testNow) || !registry.IsRevoked("two", "", 0, testNow) {
		t.Fatal("drained revocations not applied")
	}
}

func TestFeedPollerRejectsWholeInvalidPageWithoutCheckpointAdvance(t *testing.T) {
	fixture := newArtifactFixture(t)
	registry := NewRevocationRegistry(nil)
	valid := signedEntry(t, fixture.credential, familyEntry(2, "alice", "family", "prefix", testNow.Unix()-1, testNow.Unix()+30))
	bad := signedEntry(t, fixture.credential, familyEntry(3, "alice", "family", "bad", testNow.Unix()-1, testNow.Unix()+30))
	bad.Seq = 4
	doer := &fakeDoer{responses: []SignedFeedPage{{Entries: []SignedFeedEntry{valid, bad}}}}
	poller := NewFeedPoller(doer, "https://as/revocations", fixture.verifier, registry, time.Minute)
	poller.now = func() time.Time { return testNow }
	if err := poller.pollOnce(t.Context()); err == nil {
		t.Fatal("invalid page accepted")
	}
	if poller.checkpoint != 0 || registry.IsRevoked("prefix", "", 0, testNow) {
		t.Fatalf("checkpoint=%d prefix=%v", poller.checkpoint, registry.IsRevoked("prefix", "", 0, testNow))
	}
}

func TestFeedPollerRejectsNonOKStatus(t *testing.T) {
	fixture := newArtifactFixture(t)
	poller := NewFeedPoller(&fakeDoer{status: http.StatusUnauthorized}, "https://as/revocations", fixture.verifier, NewRevocationRegistry(nil), time.Minute)
	if err := poller.pollOnce(t.Context()); err == nil || poller.checkpoint != 0 {
		t.Fatalf("err=%v checkpoint=%d", err, poller.checkpoint)
	}
}

func TestFeedPollerObservesSignerRevocationWithoutRestart(t *testing.T) {
	revocations := &feedSignerRevocations{}
	fixture := newArtifactFixtureWithRevocations(t, revocations)
	registry := NewRevocationRegistry(nil)
	first := signedEntry(t, fixture.credential, familyEntry(1, "alice", "family", "one", testNow.Unix()-1, testNow.Unix()+30))
	second := signedEntry(t, fixture.credential, familyEntry(2, "alice", "family", "two", testNow.Unix()-1, testNow.Unix()+30))
	doer := &fakeDoer{responses: []SignedFeedPage{{Entries: []SignedFeedEntry{first}}, {Entries: []SignedFeedEntry{second}}}}
	poller := NewFeedPoller(doer, "https://as/revocations", fixture.verifier, registry, time.Minute)
	poller.now = func() time.Time { return testNow }
	if err := poller.pollOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	revocations.revoked.Store(true)
	if err := poller.pollOnce(t.Context()); err == nil {
		t.Fatal("revoked signer page accepted")
	}
	if poller.checkpoint != 1 || registry.IsRevoked("two", "", 0, testNow) {
		t.Fatalf("checkpoint=%d token2=%v", poller.checkpoint, registry.IsRevoked("two", "", 0, testNow))
	}
}

func TestFeedPollerRequestDeadlineRetriesHungASAndConverges(t *testing.T) {
	fixture := newArtifactFixture(t)
	registry := NewRevocationRegistry(nil)
	entry := signedEntry(t, fixture.credential, familyEntry(1, "alice", "family", "token", testNow.Unix()-1, testNow.Unix()+30))
	var attempts atomic.Int32
	doer := feedDoerFunc(func(req *http.Request) (*http.Response, error) {
		if attempts.Add(1) == 1 {
			<-req.Context().Done()
			return nil, req.Context().Err()
		}
		body, _ := json.Marshal(SignedFeedPage{Entries: []SignedFeedEntry{entry}})
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
	})
	poller := NewFeedPoller(doer, "https://as/revocations", fixture.verifier, registry, time.Millisecond)
	poller.now = func() time.Time { return testNow }
	poller.requestTimeout = 5 * time.Millisecond
	poller.maxBackoff = 5 * time.Millisecond
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		poller.Run(ctx)
	}()
	for !registry.IsRevoked("token", "", 0, testNow) && ctx.Err() == nil {
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
	if !registry.IsRevoked("token", "", 0, testNow) || attempts.Load() < 2 {
		t.Fatalf("revoked=%v attempts=%d", registry.IsRevoked("token", "", 0, testNow), attempts.Load())
	}
}

func TestFeedPollerBackoffCapsAndResetsAfterHealthyPoll(t *testing.T) {
	fixture := newArtifactFixture(t)
	var attempts atomic.Int32
	doer := feedDoerFunc(func(*http.Request) (*http.Response, error) {
		attempt := attempts.Add(1)
		if attempt == 3 {
			body, _ := json.Marshal(SignedFeedPage{Entries: []SignedFeedEntry{}})
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
		}
		return nil, errors.New("AS unavailable")
	})
	poller := NewFeedPoller(doer, "https://as/revocations", fixture.verifier, NewRevocationRegistry(nil), time.Second)
	poller.maxBackoff = 4 * time.Second
	ctx, cancel := context.WithCancel(t.Context())
	var mu sync.Mutex
	var delays []time.Duration
	poller.wait = func(_ context.Context, delay time.Duration) error {
		mu.Lock()
		delays = append(delays, delay)
		stop := len(delays) == 4
		mu.Unlock()
		if stop {
			cancel()
			return context.Canceled
		}
		return nil
	}
	poller.Run(ctx)
	mu.Lock()
	defer mu.Unlock()
	want := []time.Duration{time.Second, 2 * time.Second, time.Second, time.Second}
	if !reflect.DeepEqual(delays, want) {
		t.Fatalf("delays: want %v, got %v", want, delays)
	}
}

func TestFeedPollerParentCancellationInterruptsHungRequest(t *testing.T) {
	fixture := newArtifactFixture(t)
	started := make(chan struct{})
	doer := feedDoerFunc(func(req *http.Request) (*http.Response, error) {
		close(started)
		<-req.Context().Done()
		return nil, req.Context().Err()
	})
	poller := NewFeedPoller(doer, "https://as/revocations", fixture.verifier, NewRevocationRegistry(nil), time.Minute)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		poller.Run(ctx)
	}()
	<-started
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("parent cancellation did not interrupt hung feed request")
	}
}
