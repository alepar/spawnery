package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	authv1 "spawnery/gen/auth/v1"
	"spawnery/internal/authsvc/token"
)

type revocationDoer func(*http.Request) (*http.Response, error)

func (f revocationDoer) Do(r *http.Request) (*http.Response, error) { return f(r) }

func signedRevocation(t *testing.T, fixture artifactFixture, body *authv1.RevocationEntry) signedUserRevocationEntry {
	t.Helper()
	payload, err := (proto.MarshalOptions{Deterministic: true}).Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := fixture.credential.Sign(token.ArtifactTypeRevocation, payload)
	if err != nil {
		t.Fatal(err)
	}
	return signedUserRevocationEntry{Seq: body.Seq, Sig: wire}
}

func signedRawRevocation(t *testing.T, fixture artifactFixture, seq int64, artifactType string, payload []byte) signedUserRevocationEntry {
	t.Helper()
	wire, err := fixture.credential.Sign(artifactType, payload)
	if err != nil {
		t.Fatal(err)
	}
	return signedUserRevocationEntry{Seq: seq, Sig: wire}
}

func familyRevocation(seq int64, account, family, tokenID string, revokedAt, retainUntil int64) *authv1.RevocationEntry {
	return &authv1.RevocationEntry{
		Seq: seq, AccountId: account, FamilyId: family, RevokedAt: revokedAt,
		RevokedTokens: []*authv1.RevokedToken{{TokenId: tokenID, RetainUntil: retainUntil}},
	}
}

func accountRevocation(seq int64, account, tokenID string, revokedAt, retainUntil, cutoff int64) *authv1.RevocationEntry {
	body := &authv1.RevocationEntry{Seq: seq, AccountId: account, RevokedAt: revokedAt, RevokeTokensIssuedBefore: cutoff}
	if tokenID != "" {
		body.RevokedTokens = []*authv1.RevokedToken{{TokenId: tokenID, RetainUntil: retainUntil}}
	}
	return body
}

func revocationResponse(page signedUserRevocationPage) *http.Response {
	raw, _ := json.Marshal(page)
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(raw)))}
}

func TestRevocationConsumerDrainsBoundedPagesAndCommitsBeforeFanout(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	fixture := newArtifactFixture(t, now, "prod")
	store, err := OpenUserRevocationStore(filepath.Join(t.TempDir(), "state", "revocations.db"), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	pages := []signedUserRevocationPage{
		{Entries: []signedUserRevocationEntry{signedRevocation(t, fixture, familyRevocation(2, "alice", "family", "one", now.Unix()-10, now.Unix()+60))}, HasMore: true},
		{Entries: []signedUserRevocationEntry{signedRevocation(t, fixture, accountRevocation(7, "bob", "two", now.Unix()-5, now.Unix()+70, now.Unix()))}},
	}
	var requests atomic.Int32
	doer := revocationDoer(func(r *http.Request) (*http.Response, error) {
		index := int(requests.Add(1) - 1)
		wantURL := fmt.Sprintf("https://as.internal/revocations?limit=256&since=%d", []int64{0, 2}[index])
		if r.URL.String() != wantURL {
			t.Fatalf("url: want %q, got %q", wantURL, r.URL.String())
		}
		if _, ok := r.Context().Deadline(); !ok {
			t.Fatal("request has no deadline")
		}
		return revocationResponse(pages[index]), nil
	})
	consumer, err := NewRevocationConsumer(doer, "https://as.internal/revocations", fixture.verifier, store, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var nowCalls atomic.Int32
	consumer.now = func() time.Time { nowCalls.Add(1); return now }
	var callbacks atomic.Int32
	if err := consumer.pollOnce(context.Background(), func(page []VerifiedUserRevocation) {
		call := callbacks.Add(1)
		if call == 1 && (store.Checkpoint() != 2 || !store.IsRevoked("one", "none", now.Unix()+1)) {
			t.Fatal("first page callback ran before commit")
		}
		if call == 2 && (store.Checkpoint() != 7 || !store.IsRevoked("fresh", "bob", now.Unix()-1)) {
			t.Fatal("second page callback ran before commit")
		}
	}); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 2 || callbacks.Load() != 2 || nowCalls.Load() != 2 {
		t.Fatalf("requests=%d callbacks=%d verification-times=%d", requests.Load(), callbacks.Load(), nowCalls.Load())
	}
}

func TestRevocationConsumerInvalidLaterPagePreservesCommittedEarlierPage(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	fixture := newArtifactFixture(t, now, "prod")
	store, err := OpenUserRevocationStore(filepath.Join(t.TempDir(), "state", "revocations.db"), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	first := signedRevocation(t, fixture, familyRevocation(2, "alice", "family", "committed", now.Unix()-1, now.Unix()+60))
	bad := signedRevocation(t, fixture, familyRevocation(6, "alice", "family", "not-committed", now.Unix()-1, now.Unix()+60))
	bad.Seq = 5
	var request atomic.Int32
	consumer, err := NewRevocationConsumer(revocationDoer(func(*http.Request) (*http.Response, error) {
		if request.Add(1) == 1 {
			return revocationResponse(signedUserRevocationPage{Entries: []signedUserRevocationEntry{first}, HasMore: true}), nil
		}
		return revocationResponse(signedUserRevocationPage{Entries: []signedUserRevocationEntry{bad}}), nil
	}), "https://as/revocations", fixture.verifier, store, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	consumer.now = func() time.Time { return now }
	var callbacks atomic.Int32
	if err := consumer.pollOnce(context.Background(), func([]VerifiedUserRevocation) { callbacks.Add(1) }); err == nil {
		t.Fatal("invalid later page accepted")
	}
	if store.Checkpoint() != 2 || !store.IsRevoked("committed", "none", now.Unix()) || store.IsRevoked("not-committed", "none", now.Unix()) || callbacks.Load() != 1 {
		t.Fatalf("checkpoint=%d committed=%v invalid=%v callbacks=%d", store.Checkpoint(), store.IsRevoked("committed", "none", now.Unix()), store.IsRevoked("not-committed", "none", now.Unix()), callbacks.Load())
	}
}

func TestRevocationConsumerRejectsWholeInvalidPageWithoutPublication(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	fixture := newArtifactFixture(t, now, "prod")
	otherRoot := newArtifactFixture(t, now, "prod")
	valid := signedRevocation(t, fixture, familyRevocation(2, "alice", "family", "prefix", now.Unix()-1, now.Unix()+60))
	validBody := familyRevocation(3, "alice", "family", "invalid", now.Unix()-1, now.Unix()+60)
	validPayload, err := (proto.MarshalOptions{Deterministic: true}).Marshal(validBody)
	if err != nil {
		t.Fatal(err)
	}
	unknownPayload := append(append([]byte(nil), validPayload...), 0xa0, 0x06, 0x01)
	nonCanonicalPayload := append(append([]byte(nil), validPayload...), 0x08, 0x03)
	duplicateBody := familyRevocation(3, "alice", "family", "duplicate", now.Unix()-1, now.Unix()+60)
	duplicateBody.RevokedTokens = append(duplicateBody.RevokedTokens, duplicateBody.RevokedTokens[0])
	nonIncreasing := familyRevocation(2, "alice", "family", "duplicate-seq", now.Unix()-1, now.Unix()+60)
	tests := map[string]signedUserRevocationEntry{
		"wrong artifact type": signedRawRevocation(t, fixture, 3, token.ArtifactTypeSession, validPayload),
		"wrong root":          signedRevocation(t, otherRoot, validBody),
		"unknown proto field": signedRawRevocation(t, fixture, 3, token.ArtifactTypeRevocation, unknownPayload),
		"noncanonical proto":  signedRawRevocation(t, fixture, 3, token.ArtifactTypeRevocation, nonCanonicalPayload),
		"malformed proto":     signedRawRevocation(t, fixture, 3, token.ArtifactTypeRevocation, []byte{0xff}),
		"invalid family":      signedRevocation(t, fixture, &authv1.RevocationEntry{Seq: 3, AccountId: "alice", FamilyId: "family", RevokedAt: now.Unix() - 1}),
		"duplicate token":     signedRevocation(t, fixture, duplicateBody),
		"non-increasing seq":  signedRevocation(t, fixture, nonIncreasing),
	}
	corrupt := signedRevocation(t, fixture, validBody)
	corrupt.Sig = mutateArtifactWire(t, corrupt.Sig, func(artifact *authv1.SignedAuthArtifact) { artifact.Signature[0] ^= 1 })
	tests["bad signature"] = corrupt
	outerMismatch := signedRevocation(t, fixture, validBody)
	outerMismatch.Seq++
	tests["outer sequence"] = outerMismatch

	for name, invalid := range tests {
		t.Run(name, func(t *testing.T) {
			store, err := OpenUserRevocationStore(filepath.Join(t.TempDir(), "state", "revocations.db"), func() time.Time { return now })
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			consumer, err := NewRevocationConsumer(revocationDoer(func(*http.Request) (*http.Response, error) {
				return revocationResponse(signedUserRevocationPage{Entries: []signedUserRevocationEntry{valid, invalid}}), nil
			}), "https://as/revocations", fixture.verifier, store, 5*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			consumer.now = func() time.Time { return now }
			var callbacks atomic.Int32
			if err := consumer.pollOnce(context.Background(), func([]VerifiedUserRevocation) { callbacks.Add(1) }); err == nil {
				t.Fatal("invalid page accepted")
			}
			if store.Checkpoint() != 0 || store.IsRevoked("prefix", "alice", now.Unix()) || callbacks.Load() != 0 {
				t.Fatalf("checkpoint=%d prefix=%v callbacks=%d", store.Checkpoint(), store.IsRevoked("prefix", "alice", now.Unix()), callbacks.Load())
			}
		})
	}
}

func TestRevocationConsumerRejectsMalformedBoundedPages(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	fixture := newArtifactFixture(t, now, "prod")
	valid := signedRevocation(t, fixture, familyRevocation(2, "alice", "family", "token", now.Unix()-1, now.Unix()+60))
	validRaw, _ := json.Marshal(signedUserRevocationPage{Entries: []signedUserRevocationEntry{valid}})
	tooMany := make([]signedUserRevocationEntry, 257)
	for index := range tooMany {
		tooMany[index] = valid
	}
	tests := map[string]string{
		"empty has more":      `{"entries":[],"has_more":true}`,
		"unknown page field":  `{"entries":[],"has_more":false,"unknown":1}`,
		"unknown entry field": fmt.Sprintf(`{"entries":[{"seq":2,"sig":%q,"unknown":1}],"has_more":false}`, valid.Sig),
		"trailing data":       string(validRaw) + `{}`,
		"too many entries":    string(mustJSONValue(t, signedUserRevocationPage{Entries: tooMany})),
		"oversized body":      `{"entries":[],"has_more":false,"padding":"` + strings.Repeat("x", maxRevocationPageBytes) + `"}`,
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			store, err := OpenUserRevocationStore(filepath.Join(t.TempDir(), "state", "revocations.db"), func() time.Time { return now })
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			consumer, err := NewRevocationConsumer(revocationDoer(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(raw))}, nil
			}), "https://as/revocations", fixture.verifier, store, 5*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			if err := consumer.pollOnce(context.Background(), nil); err == nil {
				t.Fatal("malformed page accepted")
			}
		})
	}
}

func mustJSONValue(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestRevocationConsumerRequestDeadlineRetriesAndConverges(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	fixture := newArtifactFixture(t, now, "prod")
	store, err := OpenUserRevocationStore(filepath.Join(t.TempDir(), "state", "revocations.db"), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var attempts atomic.Int32
	entry := signedRevocation(t, fixture, familyRevocation(2, "alice", "family", "after-timeout", now.Unix()-1, now.Unix()+60))
	doer := revocationDoer(func(r *http.Request) (*http.Response, error) {
		if _, ok := r.Context().Deadline(); !ok {
			t.Fatal("request deadline missing")
		}
		if attempts.Add(1) == 1 {
			<-r.Context().Done()
			return nil, r.Context().Err()
		}
		return revocationResponse(signedUserRevocationPage{Entries: []signedUserRevocationEntry{entry}}), nil
	})
	consumer, err := NewRevocationConsumer(doer, "https://as/revocations", fixture.verifier, store, 5*time.Second,
		WithRevocationRequestTimeout(10*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	consumer.now = func() time.Time { return now }
	ctx, cancel := context.WithCancel(context.Background())
	var delays []time.Duration
	consumer.wait = func(_ context.Context, delay time.Duration) error {
		delays = append(delays, delay)
		return nil
	}
	consumer.Run(ctx, func([]VerifiedUserRevocation) { cancel() })
	if attempts.Load() != 2 || store.Checkpoint() != 2 || !reflect.DeepEqual(delays, []time.Duration{5 * time.Second}) {
		t.Fatalf("attempts=%d checkpoint=%d delays=%v", attempts.Load(), store.Checkpoint(), delays)
	}
}

func TestRevocationConsumerExponentialBackoffCapsAndResets(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	fixture := newArtifactFixture(t, now, "prod")
	store, err := OpenUserRevocationStore(filepath.Join(t.TempDir(), "state", "revocations.db"), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var attempts atomic.Int32
	consumer, err := NewRevocationConsumer(revocationDoer(func(*http.Request) (*http.Response, error) {
		attempt := attempts.Add(1)
		if attempt == 4 {
			return revocationResponse(signedUserRevocationPage{Entries: []signedUserRevocationEntry{}}), nil
		}
		return nil, errors.New("unavailable")
	}), "https://as/revocations", fixture.verifier, store, 5*time.Second, WithRevocationMaxBackoff(20*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var delays []time.Duration
	consumer.wait = func(_ context.Context, delay time.Duration) error {
		delays = append(delays, delay)
		if len(delays) == 5 {
			cancel()
			return context.Canceled
		}
		return nil
	}
	consumer.Run(ctx, nil)
	want := []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 5 * time.Second, 5 * time.Second}
	if !reflect.DeepEqual(delays, want) {
		t.Fatalf("backoff: want %v, got %v", want, delays)
	}
}

func TestRevocationConsumerParentCancellationIsImmediate(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	fixture := newArtifactFixture(t, now, "prod")
	store, err := OpenUserRevocationStore(filepath.Join(t.TempDir(), "state", "revocations.db"), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	started := make(chan struct{})
	var once sync.Once
	consumer, err := NewRevocationConsumer(revocationDoer(func(r *http.Request) (*http.Response, error) {
		once.Do(func() { close(started) })
		<-r.Context().Done()
		return nil, r.Context().Err()
	}), "https://as/revocations", fixture.verifier, store, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { consumer.Run(ctx, nil); close(done) }()
	<-started
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("parent cancellation did not stop the consumer")
	}
}
