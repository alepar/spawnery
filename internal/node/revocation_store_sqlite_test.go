package node

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestUserRevocationStoreSQLitePersistsGapsMaxRetentionAndCutoffs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "revocations.db")
	now := time.Unix(12, 0)
	clock := func() time.Time { return now }
	store, err := OpenUserRevocationStore(path, clock)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ApplyPage([]VerifiedUserRevocation{
		{Seq: 2, AccountID: "alice", FamilyID: "family", RevokedAt: 10,
			RevokedTokens: []VerifiedRevokedToken{{TokenID: "shared", RetainUntil: 20}}},
		{Seq: 5, AccountID: "bob", RevokedAt: 11, RevokeTokensIssuedBefore: 12,
			RevokedTokens: []VerifiedRevokedToken{{TokenID: "explicit", RetainUntil: 30}}},
	}, now); err != nil {
		t.Fatal(err)
	}
	if store.Checkpoint() != 5 || !store.IsRevoked("shared", "nobody", 99) || !store.IsRevoked("fresh", "bob", 11) || store.IsRevoked("fresh", "bob", 12) || !store.IsRevoked("explicit", "bob", 99) {
		t.Fatalf("checkpoint=%d shared=%v old=%v equal=%v explicit=%v", store.Checkpoint(), store.IsRevoked("shared", "nobody", 99), store.IsRevoked("fresh", "bob", 11), store.IsRevoked("fresh", "bob", 12), store.IsRevoked("explicit", "bob", 99))
	}
	if err := store.ApplyPage([]VerifiedUserRevocation{
		{Seq: 9, AccountID: "alice", FamilyID: "family", RevokedAt: 11,
			RevokedTokens: []VerifiedRevokedToken{{TokenID: "shared", RetainUntil: 40}}},
		{Seq: 10, AccountID: "bob", RevokedAt: 12, RevokeTokensIssuedBefore: 20},
		{Seq: 11, AccountID: "bob", RevokedAt: 13, RevokeTokensIssuedBefore: 15},
	}, now); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenUserRevocationStore(path, clock)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.Checkpoint() != 11 || !reopened.IsRevoked("shared", "x", 99) || !reopened.IsRevoked("x", "bob", 19) || reopened.IsRevoked("x", "bob", 20) {
		t.Fatal("persisted sqlite snapshot not restored")
	}
	now = time.Unix(40, 0)
	if err := reopened.ApplyPage(nil, now); err != nil {
		t.Fatal(err)
	}
	if reopened.IsRevoked("shared", "x", 99) || !reopened.IsRevoked("x", "bob", 11) {
		t.Fatal("expiry pruning removed the wrong binding")
	}
}

func TestUserRevocationStoreSQLiteRejectsPageAtomically(t *testing.T) {
	now := time.Unix(20, 0)
	store, err := OpenUserRevocationStore(filepath.Join(t.TempDir(), "state", "revocations.db"), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	bad := []VerifiedUserRevocation{
		{Seq: 3, AccountID: "alice", FamilyID: "family", RevokedAt: 10,
			RevokedTokens: []VerifiedRevokedToken{{TokenID: "valid-prefix", RetainUntil: 30}}},
		{Seq: 3, AccountID: "alice", RevokeTokensIssuedBefore: 10, RevokedAt: 10},
	}
	if err := store.ApplyPage(bad, now); err == nil {
		t.Fatal("non-increasing page accepted")
	}
	if store.Checkpoint() != 0 || store.IsRevoked("valid-prefix", "alice", 0) {
		t.Fatal("invalid page partially published")
	}
}

func TestUserRevocationStoreSQLiteOwnsFilesLocksAndFailsClosed(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	path := filepath.Join(dir, "revocations.db")
	store, err := OpenUserRevocationStore(path, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenUserRevocationStore(path, time.Now); !errors.Is(err, ErrUserRevocationStoreLocked) {
		t.Fatalf("second open: %v", err)
	}
	for _, check := range []struct {
		path string
		mode os.FileMode
	}{{dir, 0o700}, {path, 0o600}, {path + ".lock", 0o600}} {
		info, err := os.Stat(check.path)
		if err != nil {
			t.Fatalf("stat %s: %v", check.path, err)
		}
		if info.Mode().Perm() != check.mode {
			t.Fatalf("%s mode=%v", check.path, info.Mode().Perm())
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"version":1,"checkpoint":7}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenUserRevocationStore(path, time.Now); err == nil {
		t.Fatal("legacy JSON state opened as sqlite")
	}
}

func TestUserRevocationStoreSQLitePoisonsAmbiguousCommitUntilRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "revocations.db")
	now := time.Unix(10, 0)
	store, err := OpenUserRevocationStore(path, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	store.afterRename = func() error { return errors.New("injected post-commit failure") }
	page := []VerifiedUserRevocation{{Seq: 4, AccountID: "alice", FamilyID: "family", RevokedAt: 1,
		RevokedTokens: []VerifiedRevokedToken{{TokenID: "committed", RetainUntil: 20}}}}
	if err := store.ApplyPage(page, now); !errors.Is(err, ErrUserRevocationStorePoisoned) {
		t.Fatalf("ambiguous commit: %v", err)
	}
	if store.Checkpoint() != 0 || store.IsRevoked("committed", "alice", 0) {
		t.Fatal("ambiguous commit was published by the poisoned live store")
	}
	if err := store.ApplyPage(nil, now); !errors.Is(err, ErrUserRevocationStorePoisoned) {
		t.Fatalf("poison is not sticky: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenUserRevocationStore(path, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.Checkpoint() != 4 || !reopened.IsRevoked("committed", "alice", 0) {
		t.Fatal("restart did not load the durably committed page")
	}
}

func TestUserRevocationStoreSQLiteCompactsStateBeyondLegacyReadLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "revocations.db")
	now := time.Unix(10, 0)
	store, err := OpenUserRevocationStore(path, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	tokens := make([]VerifiedRevokedToken, 5_000)
	padding := strings.Repeat("x", 7_000)
	for index := range tokens {
		tokens[index] = VerifiedRevokedToken{TokenID: fmt.Sprintf("%05d-%s", index, padding), RetainUntil: 20}
	}
	if err := store.ApplyPage([]VerifiedUserRevocation{{
		Seq: 1, AccountID: "stress", FamilyID: "family", RevokedAt: 1, RevokedTokens: tokens,
	}}, now); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() <= 32<<20 {
		t.Fatalf("test did not exceed legacy 32 MiB state limit: %d", info.Size())
	}
	now = time.Unix(20, 0)
	if err := store.ApplyPage(nil, now); err != nil {
		t.Fatal(err)
	}
	if store.IsRevoked(tokens[0].TokenID, "none", 0) {
		t.Fatal("expired stress token remained denied")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenUserRevocationStore(path, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.Checkpoint() != 1 || reopened.IsRevoked(tokens[len(tokens)-1].TokenID, "none", 0) {
		t.Fatal("compacted state did not survive restart")
	}
}
