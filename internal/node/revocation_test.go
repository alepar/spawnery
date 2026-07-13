package node

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	authv1 "spawnery/gen/auth/v1"
	"spawnery/internal/authsvc/token"
)

func TestUserRevocationStorePersistsGappedFamilyAndAccountRevocations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "state.json")
	store, err := OpenUserRevocationStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ApplyBatch([]VerifiedUserRevocation{
		{Seq: 2, AccountID: "alice", FamilyID: "family-1", TokenIDs: []string{"tok-old"}, RevokedAt: 10},
		{Seq: 5, AccountID: "bob", TokenIDs: []string{"tok-bob"}, RevokedAt: 11},
	}); err != nil {
		t.Fatal(err)
	}
	if store.Checkpoint() != 5 || !store.IsRevoked("tok-old", "nobody") || store.IsRevoked("tok-new", "alice") || !store.IsRevoked("tok-new", "bob") {
		t.Fatalf("checkpoint=%d old=%v alice=%v bob=%v", store.Checkpoint(), store.IsRevoked("tok-old", "nobody"), store.IsRevoked("tok-new", "alice"), store.IsRevoked("tok-new", "bob"))
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenUserRevocationStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if reopened.Checkpoint() != 5 || !reopened.IsRevoked("tok-old", "x") || !reopened.IsRevoked("fresh", "bob") {
		t.Fatal("persisted snapshot not restored")
	}
}

func TestUserRevocationStoreRejectsBatchAtomically(t *testing.T) {
	store, err := OpenUserRevocationStore(filepath.Join(t.TempDir(), "state", "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	err = store.ApplyBatch([]VerifiedUserRevocation{{Seq: 3, AccountID: "alice", FamilyID: "f", TokenIDs: []string{"ok"}}, {Seq: 3, AccountID: "alice", FamilyID: "f", TokenIDs: []string{"bad"}}})
	if err == nil || store.Checkpoint() != 0 || store.IsRevoked("ok", "alice") {
		t.Fatalf("err=%v checkpoint=%d", err, store.Checkpoint())
	}
}

func TestUserRevocationStoreOwnsPathAndRejectsMalformedState(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	path := filepath.Join(dir, "state.json")
	store, err := OpenUserRevocationStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenUserRevocationStore(path); !errors.Is(err, ErrUserRevocationStoreLocked) {
		t.Fatalf("second open err=%v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("directory mode=%v", info.Mode())
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenUserRevocationStore(path); err == nil {
		t.Fatal("malformed state accepted")
	}
}

func TestUserRevocationStorePersistenceFailureBoundaries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "state.json")
	store, err := OpenUserRevocationStore(path)
	if err != nil {
		t.Fatal(err)
	}
	store.beforeRename = func() error { return errors.New("injected") }
	if err := store.ApplyBatch([]VerifiedUserRevocation{{Seq: 2, AccountID: "alice", FamilyID: "f", TokenIDs: []string{"old"}}}); err == nil {
		t.Fatal("pre-rename failure accepted")
	}
	if store.Checkpoint() != 0 || store.IsRevoked("old", "alice") {
		t.Fatal("pre-rename failure published")
	}
	store.beforeRename = nil
	store.afterRename = func() error { return errors.New("injected") }
	if err := store.ApplyBatch([]VerifiedUserRevocation{{Seq: 3, AccountID: "alice", FamilyID: "f", TokenIDs: []string{"old"}}}); !errors.Is(err, ErrUserRevocationStorePoisoned) {
		t.Fatalf("post-rename err=%v", err)
	}
	if store.Checkpoint() != 3 || !store.IsRevoked("old", "x") {
		t.Fatal("renamed snapshot was not published conservatively")
	}
	if err := store.ApplyBatch([]VerifiedUserRevocation{{Seq: 4, AccountID: "alice", FamilyID: "f", TokenIDs: []string{"new"}}}); !errors.Is(err, ErrUserRevocationStorePoisoned) {
		t.Fatalf("poison not sticky: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenUserRevocationStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.Checkpoint() != 3 || !reopened.IsRevoked("old", "x") {
		t.Fatal("renamed snapshot not restored")
	}
}

type revocationDoer func(*http.Request) (*http.Response, error)

func (f revocationDoer) Do(r *http.Request) (*http.Response, error) { return f(r) }

func signedRevocation(t *testing.T, fixture artifactFixture, seq int64, account, family string, ids []string) map[string]any {
	t.Helper()
	idsRaw, err := json.Marshal(ids)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(struct {
		Seq       int64  `json:"seq"`
		AccountID string `json:"account_id"`
		FamilyID  string `json:"family_id"`
		TokenIDs  string `json:"token_ids"`
		RevokedAt int64  `json:"revoked_at"`
	}{seq, account, family, string(idsRaw), 123})
	if err != nil {
		t.Fatal(err)
	}
	sig, err := fixture.credential.Sign(token.ArtifactTypeRevocation, payload)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{"seq": seq, "account_id": account, "family_id": family, "token_ids": string(idsRaw), "revoked_at": 123, "sig": sig}
}

func TestRevocationConsumerVerifiesWholeGappedBatch(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	fixture := newArtifactFixture(t, now, "prod")
	store, err := OpenUserRevocationStore(filepath.Join(t.TempDir(), "state", "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	entries := []map[string]any{signedRevocation(t, fixture, 2, "alice", "f", []string{"one"}), signedRevocation(t, fixture, 7, "bob", "", []string{"two"})}
	raw, _ := json.Marshal(entries)
	doer := revocationDoer(func(r *http.Request) (*http.Response, error) {
		if got := r.URL.String(); got != "https://as.internal/revocations?since=0" {
			t.Fatalf("url=%q", got)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(raw)))}, nil
	})
	consumer, err := NewRevocationConsumer(doer, "https://as.internal/revocations", fixture.verifier, store, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	consumer.now = func() time.Time { return now }
	var calls atomic.Int32
	if err := consumer.pollOnce(context.Background(), func(batch []VerifiedUserRevocation) { calls.Add(1) }); err != nil {
		t.Fatal(err)
	}
	if store.Checkpoint() != 7 || !store.IsRevoked("one", "x") || !store.IsRevoked("fresh", "bob") || calls.Load() != 1 {
		t.Fatal("verified batch not committed and published")
	}
}

func TestRevocationConsumerInvalidTailDoesNotAdvance(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	fixture := newArtifactFixture(t, now, "prod")
	store, err := OpenUserRevocationStore(filepath.Join(t.TempDir(), "state", "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	entries := []map[string]any{signedRevocation(t, fixture, 2, "alice", "f", []string{"one"}), signedRevocation(t, fixture, 3, "alice", "f", []string{"two"})}
	entries[1]["seq"] = int64(4)
	raw, _ := json.Marshal(entries)
	consumer, err := NewRevocationConsumer(revocationDoer(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(raw)))}, nil
	}), "https://as/revocations", fixture.verifier, store, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	consumer.now = func() time.Time { return now }
	if err := consumer.pollOnce(context.Background(), nil); err == nil {
		t.Fatal("tampered outer sequence accepted")
	}
	if store.Checkpoint() != 0 || store.IsRevoked("one", "alice") {
		t.Fatal("invalid batch advanced state")
	}
}

func TestRevocationConsumerFansOutRenamedCheckpointBeforeReportingPoison(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	fixture := newArtifactFixture(t, now, "prod")
	store, err := OpenUserRevocationStore(filepath.Join(t.TempDir(), "state", "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	store.afterRename = func() error { return errors.New("directory sync failed") }
	raw, _ := json.Marshal([]map[string]any{signedRevocation(t, fixture, 2, "alice", "family", []string{"old"})})
	consumer, err := NewRevocationConsumer(revocationDoer(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(raw)))}, nil
	}), "https://as/revocations", fixture.verifier, store, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	consumer.now = func() time.Time { return now }
	var calls atomic.Int32
	err = consumer.pollOnce(t.Context(), func([]VerifiedUserRevocation) { calls.Add(1) })
	if !errors.Is(err, ErrUserRevocationStorePoisoned) {
		t.Fatalf("poll err=%v", err)
	}
	if store.Checkpoint() != 2 || calls.Load() != 1 {
		t.Fatalf("checkpoint=%d callbacks=%d", store.Checkpoint(), calls.Load())
	}
}

func TestRevocationConsumerRejectsWholeInvalidBatchWithoutPublication(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	fixture := newArtifactFixture(t, now, "prod")
	validPrefix := signedRevocation(t, fixture, 12, "prefix-account", "prefix-family", []string{"prefix-token"})
	validInvalidSlot := signedRevocation(t, fixture, 13, "invalid-account", "invalid-family", []string{"invalid-token"})
	encode := func(entries ...map[string]any) []byte {
		raw, err := json.Marshal(entries)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	mutateSig := func(entry map[string]any, mutate func(*authv1.SignedAuthArtifact)) map[string]any {
		clone := make(map[string]any, len(entry))
		for k, v := range entry {
			clone[k] = v
		}
		clone["sig"] = mutateArtifactWire(t, entry["sig"].(string), mutate)
		return clone
	}
	signedPayload := func(payload []byte, seq int64, artifactType string) map[string]any {
		sig, err := fixture.credential.Sign(artifactType, payload)
		if err != nil {
			t.Fatal(err)
		}
		return map[string]any{"seq": seq, "account_id": "outer", "family_id": "outer", "token_ids": "[]", "revoked_at": 1, "sig": sig}
	}
	payloadWithTokenIDs := func(seq int64, tokenIDs string) []byte {
		raw, err := json.Marshal(struct {
			Seq       int64  `json:"seq"`
			AccountID string `json:"account_id"`
			FamilyID  string `json:"family_id"`
			TokenIDs  string `json:"token_ids"`
			RevokedAt int64  `json:"revoked_at"`
		}{seq, "invalid-account", "family", tokenIDs, 1})
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	wrongRoot := newArtifactFixture(t, now, "prod")
	wrongPurposeLeaf := replacementArtifactLeaf(t, fixture, now, fixture.credential.PrivateKey.Public(), func(cert *x509.Certificate) { cert.Policies = nil })

	tests := []struct {
		name           string
		response       func() (*http.Response, error)
		seedCheckpoint bool
	}{
		{name: "wrong root", response: func() (*http.Response, error) {
			return jsonResponse(encode(validPrefix, signedRevocation(t, wrongRoot, 13, "invalid-account", "family", []string{"invalid-token"}))), nil
		}},
		{name: "wrong artifact type", response: func() (*http.Response, error) {
			return jsonResponse(encode(validPrefix, mutateSig(validInvalidSlot, func(a *authv1.SignedAuthArtifact) { a.ArtifactType = token.ArtifactTypeSession }))), nil
		}},
		{name: "wrong signer purpose", response: func() (*http.Response, error) {
			return jsonResponse(encode(validPrefix, mutateSig(validInvalidSlot, func(a *authv1.SignedAuthArtifact) { a.SignerChain[0] = wrongPurposeLeaf.Raw }))), nil
		}},
		{name: "wrong signer key", response: func() (*http.Response, error) {
			return jsonResponse(encode(validPrefix, mutateSig(validInvalidSlot, func(a *authv1.SignedAuthArtifact) { a.KeyId[0] ^= 1 }))), nil
		}},
		{name: "invalid signature", response: func() (*http.Response, error) {
			return jsonResponse(encode(validPrefix, mutateSig(validInvalidSlot, func(a *authv1.SignedAuthArtifact) { a.Signature[0] ^= 1 }))), nil
		}},
		{name: "malformed verified payload", response: func() (*http.Response, error) {
			return jsonResponse(encode(validPrefix, signedPayload([]byte("{"), 13, token.ArtifactTypeRevocation))), nil
		}},
		{name: "null verified token ids", response: func() (*http.Response, error) {
			return jsonResponse(encode(validPrefix, signedPayload(payloadWithTokenIDs(13, "null"), 13, token.ArtifactTypeRevocation))), nil
		}},
		{name: "invalid verified token id", response: func() (*http.Response, error) {
			return jsonResponse(encode(validPrefix, signedPayload(payloadWithTokenIDs(13, `["","duplicate","duplicate"]`), 13, token.ArtifactTypeRevocation))), nil
		}},
		{name: "rollback after prefix", seedCheckpoint: true, response: func() (*http.Response, error) {
			return jsonResponse(encode(validPrefix, signedRevocation(t, fixture, 9, "invalid-account", "family", []string{"invalid-token"}))), nil
		}},
		{name: "non 200", response: func() (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: io.NopCloser(strings.NewReader("unavailable"))}, nil
		}},
		{name: "oversized response", response: func() (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(strings.Repeat(" ", maxRevocationFeedSize+1)))}, nil
		}},
		{name: "network failure", response: func() (*http.Response, error) { return nil, errors.New("network unavailable") }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, err := OpenUserRevocationStore(filepath.Join(t.TempDir(), "state", "state.json"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			if test.seedCheckpoint {
				if err := store.ApplyBatch([]VerifiedUserRevocation{{Seq: 10, AccountID: "baseline-account", FamilyID: "baseline-family", TokenIDs: []string{"baseline-token"}}}); err != nil {
					t.Fatal(err)
				}
			}
			wantCheckpoint := store.Checkpoint()
			consumer, err := NewRevocationConsumer(revocationDoer(func(*http.Request) (*http.Response, error) { return test.response() }), "https://as/revocations", fixture.verifier, store, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			consumer.now = func() time.Time { return now }
			var callbacks atomic.Int32
			if err := consumer.pollOnce(t.Context(), func([]VerifiedUserRevocation) { callbacks.Add(1) }); err == nil {
				t.Fatal("invalid response accepted")
			}
			if store.Checkpoint() != wantCheckpoint || store.IsRevoked("prefix-token", "prefix-account") || store.IsRevoked("invalid-token", "invalid-account") || callbacks.Load() != 0 {
				t.Fatalf("checkpoint=%d want=%d prefix=%v invalid=%v callbacks=%d", store.Checkpoint(), wantCheckpoint, store.IsRevoked("prefix-token", "prefix-account"), store.IsRevoked("invalid-token", "invalid-account"), callbacks.Load())
			}
			if test.seedCheckpoint && !store.IsRevoked("baseline-token", "none") {
				t.Fatal("baseline deny index changed")
			}
		})
	}
}

func jsonResponse(raw []byte) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(raw)))}
}

func TestRevocationConsumerRunPollsImmediatelyAndCancelsBlockedRequest(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	fixture := newArtifactFixture(t, now, "prod")
	store, err := OpenUserRevocationStore(filepath.Join(t.TempDir(), "state", "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	called := make(chan struct{})
	doer := revocationDoer(func(r *http.Request) (*http.Response, error) {
		close(called)
		<-r.Context().Done()
		return nil, r.Context().Err()
	})
	consumer, err := NewRevocationConsumer(doer, "https://as/revocations", fixture.verifier, store, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { consumer.Run(ctx, nil); close(done) }()
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("first poll was not immediate")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("consumer did not stop after cancellation")
	}
}
