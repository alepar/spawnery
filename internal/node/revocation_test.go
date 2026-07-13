package node

import (
	"context"
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
