package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"
)

func ctxT() context.Context { return context.Background() }

func mkUser(t *testing.T, st Store, accountID string, sub int64) {
	t.Helper()
	if err := st.Users().Create(ctxT(), User{
		AccountID: accountID, GithubSub: sub, Handle: "h", Status: UserActive, CreatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestUsers(t *testing.T) {
	st := NewTestStore(t)
	mkUser(t, st, "acct-1", 12345)

	u, err := st.Users().GetBySub(ctxT(), 12345)
	if err != nil || u.AccountID != "acct-1" {
		t.Fatalf("GetBySub: %+v %v", u, err)
	}
	if _, err := st.Users().GetBySub(ctxT(), 99); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	// github_sub is UNIQUE — second registration with the same sub conflicts.
	err = st.Users().Create(ctxT(), User{AccountID: "acct-2", GithubSub: 12345, Handle: "x", Status: UserActive, CreatedAt: 2})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("want ErrConflict, got %v", err)
	}
	if err := st.Users().SetHandle(ctxT(), "acct-1", "renamed"); err != nil {
		t.Fatal(err)
	}
	if err := st.Users().SetStatus(ctxT(), "acct-1", UserDisabled); err != nil {
		t.Fatal(err)
	}
	u, _ = st.Users().GetByID(ctxT(), "acct-1")
	if u.Handle != "renamed" || u.Status != UserDisabled {
		t.Fatalf("update lost: %+v", u)
	}
}

func TestRefreshSessionsRoundTrip(t *testing.T) {
	st := NewTestStore(t)
	mkUser(t, st, "acct-1", 1)
	spki := []byte{0x30, 0x59, 0x01, 0x02}
	row := RefreshSession{
		TokenHash: "hash-1", AccountID: "acct-1", FamilyID: "fam-1", ClientKind: ClientWeb,
		SessionPubkeySPKI: spki, CPAccessTokenID: "cp-1", NodeAccessTokenID: "node-1",
		CreatedAt: 10, LastUsedAt: 10, ExpiresAt: 100, FamilyCreatedAt: 10,
	}
	if err := st.RefreshSessions().Insert(ctxT(), row); err != nil {
		t.Fatal(err)
	}
	got, err := st.RefreshSessions().Get(ctxT(), "hash-1")
	if err != nil {
		t.Fatal(err)
	}
	if string(got.SessionPubkeySPKI) != string(spki) || got.CPAccessTokenID != "cp-1" || got.NodeAccessTokenID != "node-1" || got.SupersededBy != "" || got.Revoked {
		t.Fatalf("round trip: %+v", got)
	}
}

func TestSupersedeAndFamilyRevoke(t *testing.T) {
	st := NewTestStore(t)
	mkUser(t, st, "acct-1", 1)
	r1 := RefreshSession{TokenHash: "h1", AccountID: "acct-1", FamilyID: "fam", ClientKind: ClientCLI,
		SessionPubkeySPKI: []byte{1}, CPAccessTokenID: "cp1", NodeAccessTokenID: "node1", CreatedAt: 1, LastUsedAt: 1, ExpiresAt: 100, FamilyCreatedAt: 1}
	if err := st.RefreshSessions().Insert(ctxT(), r1); err != nil {
		t.Fatal(err)
	}
	r2 := r1
	r2.TokenHash, r2.CPAccessTokenID, r2.NodeAccessTokenID = "h2", "cp2", "node2"
	if err := st.RefreshSessions().Supersede(ctxT(), "h1", r2, `{"pair":1}`, 5); err != nil {
		t.Fatal(err)
	}
	got1, _ := st.RefreshSessions().Get(ctxT(), "h1")
	if got1.SupersededBy != "h2" || got1.SupersededAt != 5 || got1.SuccessorCache == "" {
		t.Fatalf("predecessor not stamped: %+v", got1)
	}
	// Superseding again from the same predecessor conflicts (already superseded).
	r3 := r1
	r3.TokenHash, r3.CPAccessTokenID, r3.NodeAccessTokenID = "h3", "cp3", "node3"
	if err := st.RefreshSessions().Supersede(ctxT(), "h1", r3, "{}", 6); !errors.Is(err, ErrConflict) {
		t.Fatalf("want ErrConflict, got %v", err)
	}
	// Next generation clears the grandparent's cache.
	r3.TokenHash, r3.CPAccessTokenID, r3.NodeAccessTokenID = "h3", "cp3", "node3"
	if err := st.RefreshSessions().Supersede(ctxT(), "h2", r3, `{"pair":2}`, 7); err != nil {
		t.Fatal(err)
	}
	got1, _ = st.RefreshSessions().Get(ctxT(), "h1")
	if got1.SuccessorCache != "" {
		t.Fatalf("grandparent cache not cleared: %+v", got1)
	}

	ids, err := st.RefreshSessions().RevokeFamily(ctxT(), "fam")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"cp1", "node1", "cp2", "node2", "cp3", "node3"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("live token ids: %v", ids)
	}
	got3, _ := st.RefreshSessions().Get(ctxT(), "h3")
	if !got3.Revoked {
		t.Fatal("family revoke missed h3")
	}
	// Idempotent: second revoke returns no live ids.
	ids, _ = st.RefreshSessions().RevokeFamily(ctxT(), "fam")
	if len(ids) != 0 {
		t.Fatalf("second revoke: %v", ids)
	}
}

func TestFamilyCounting(t *testing.T) {
	st := NewTestStore(t)
	mkUser(t, st, "acct-1", 1)
	for i, fam := range []string{"famA", "famB"} {
		err := st.RefreshSessions().Insert(ctxT(), RefreshSession{
			TokenHash: fam + "-h", AccountID: "acct-1", FamilyID: fam, ClientKind: ClientWeb,
			SessionPubkeySPKI: []byte{1}, CPAccessTokenID: fam + "-cp", NodeAccessTokenID: fam + "-node",
			CreatedAt: int64(i + 1), LastUsedAt: int64(i + 1), ExpiresAt: 100, FamilyCreatedAt: int64(i + 1),
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	n, err := st.RefreshSessions().CountFamilies(ctxT(), "acct-1")
	if err != nil || n != 2 {
		t.Fatalf("count: %d %v", n, err)
	}
	oldest, err := st.RefreshSessions().OldestFamily(ctxT(), "acct-1")
	if err != nil || oldest != "famA" {
		t.Fatalf("oldest: %s %v", oldest, err)
	}
	if _, err := st.RefreshSessions().RevokeFamily(ctxT(), "famA"); err != nil {
		t.Fatal(err)
	}
	n, _ = st.RefreshSessions().CountFamilies(ctxT(), "acct-1")
	if n != 1 {
		t.Fatalf("count after revoke: %d", n)
	}
}

func TestPairedAccessTokenMigrationInvalidatesLegacyFamilies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "authsvc.db")
	dsn := "file:" + path
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	goose.SetBaseFS(migrationsFS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatal(err)
	}
	if err := goose.UpTo(db, "migrations/sqlite", 6); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO users VALUES ('acct', 1, 'alice', 'active', 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO refresh_sessions
		(token_hash, account_id, family_id, client_kind, session_pubkey_spki, access_token_id, created_at, last_used_at, expires_at, family_created_at)
		VALUES ('legacy', 'acct', 'family', 'cli', x'01', 'cp-only', 1, 1, 100, 1)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	st, err := Open(ctxT(), Config{Driver: "sqlite", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.RefreshSessions().Get(ctxT(), "legacy"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("legacy family survived migration: %v", err)
	}
	if err := st.RefreshSessions().Insert(ctxT(), RefreshSession{
		TokenHash: "paired", AccountID: "acct", FamilyID: "new-family", ClientKind: ClientCLI,
		SessionPubkeySPKI: []byte{1}, CPAccessTokenID: "cp", NodeAccessTokenID: "node",
		CreatedAt: 2, LastUsedAt: 2, ExpiresAt: 100, FamilyCreatedAt: 2,
	}); err != nil {
		t.Fatalf("insert paired family after migration: %v", err)
	}
}

func TestPairedRevocationRollback(t *testing.T) {
	st := NewTestStore(t)
	mkUser(t, st, "acct-rollback", 99)
	row := RefreshSession{
		TokenHash: "rollback-hash", AccountID: "acct-rollback", FamilyID: "rollback-family", ClientKind: ClientCLI,
		SessionPubkeySPKI: []byte{1}, CPAccessTokenID: "rollback-cp", NodeAccessTokenID: "rollback-node",
		CreatedAt: 1, LastUsedAt: 1, ExpiresAt: 100, FamilyCreatedAt: 1,
	}
	if err := st.RefreshSessions().Insert(ctxT(), row); err != nil {
		t.Fatal(err)
	}
	if _, err := st.(*bunStore).db.ExecContext(ctxT(), `CREATE TRIGGER fail_revocation_insert BEFORE INSERT ON revocation_events BEGIN SELECT RAISE(ABORT, 'forced append failure'); END`); err != nil {
		t.Fatal(err)
	}
	err := st.WithTx(ctxT(), func(tx Store) error {
		ids, err := tx.RefreshSessions().RevokeFamily(ctxT(), row.FamilyID)
		if err != nil {
			return err
		}
		_, err = tx.Revocations().Append(ctxT(), RevocationEvent{
			AccountID: row.AccountID, FamilyID: row.FamilyID, TokenIDs: string(mustJSON(t, ids)), RevokedAt: 2,
		})
		return err
	})
	if err == nil {
		t.Fatal("forced revocation append succeeded")
	}
	got, err := st.RefreshSessions().Get(ctxT(), row.TokenHash)
	if err != nil {
		t.Fatal(err)
	}
	if got.Revoked {
		t.Fatal("family revocation survived failed event append")
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestOAuthStateSingleUse(t *testing.T) {
	st := NewTestStore(t)
	if err := st.OAuthStates().Create(ctxT(), OAuthState{
		State: "s1", FlowCookieHash: "f", ClientChallenge: "c", ClientRedirectURI: "r",
		ClientState: "cs", GhVerifier: "v", CreatedAt: 1, ExpiresAt: 100,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := st.OAuthStates().Consume(ctxT(), "s1")
	if err != nil || got.GhVerifier != "v" {
		t.Fatalf("consume: %+v %v", got, err)
	}
	if _, err := st.OAuthStates().Consume(ctxT(), "s1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second consume: want ErrNotFound, got %v", err)
	}
}

func TestDeviceGrantLifecycle(t *testing.T) {
	st := NewTestStore(t)
	g := DeviceGrant{DeviceCodeHash: "dch", UserCode: "AAAA-BBBB", SessionPubkeySPKI: []byte{1},
		ClientKind: ClientCLI, Status: GrantPending, CreatedAt: 1, ExpiresAt: 100}
	if err := st.DeviceGrants().Create(ctxT(), g); err != nil {
		t.Fatal(err)
	}
	// Redeem before approval conflicts.
	if _, err := st.DeviceGrants().Redeem(ctxT(), "dch"); !errors.Is(err, ErrConflict) {
		t.Fatalf("redeem pending: want ErrConflict, got %v", err)
	}
	if err := st.DeviceGrants().SetDecision(ctxT(), "AAAA-BBBB", "acct-1", GrantApproved); err != nil {
		t.Fatal(err)
	}
	// Second decision conflicts (not pending anymore).
	if err := st.DeviceGrants().SetDecision(ctxT(), "AAAA-BBBB", "acct-2", GrantDenied); !errors.Is(err, ErrConflict) {
		t.Fatalf("double decision: want ErrConflict, got %v", err)
	}
	got, err := st.DeviceGrants().Redeem(ctxT(), "dch")
	if err != nil || got.AccountID != "acct-1" {
		t.Fatalf("redeem: %+v %v", got, err)
	}
	if _, err := st.DeviceGrants().Redeem(ctxT(), "dch"); !errors.Is(err, ErrConflict) {
		t.Fatalf("double redeem: want ErrConflict, got %v", err)
	}
	n, err := st.DeviceGrants().BumpAttempt(ctxT(), "dch")
	if err != nil || n != 1 {
		t.Fatalf("bump: %d %v", n, err)
	}
}

func TestRevocationFeed(t *testing.T) {
	st := NewTestStore(t)
	s1, err := st.Revocations().Append(ctxT(), RevocationEvent{AccountID: "a", FamilyID: "f1", TokenIDs: `["t1"]`, RevokedAt: 1})
	if err != nil {
		t.Fatal(err)
	}
	s2, err := st.Revocations().Append(ctxT(), RevocationEvent{AccountID: "a", FamilyID: "f2", TokenIDs: `["t2"]`, RevokedAt: 2})
	if err != nil {
		t.Fatal(err)
	}
	if s2 <= s1 {
		t.Fatalf("seq not monotonic: %d %d", s1, s2)
	}
	evs, err := st.Revocations().Since(ctxT(), s1)
	if err != nil || len(evs) != 1 || evs[0].FamilyID != "f2" {
		t.Fatalf("since: %+v %v", evs, err)
	}
}

// Migration smoke: two independent opens both migrate cleanly.
func TestMigrationSmoke(t *testing.T) {
	_ = NewTestStore(t)
	st2, err := Open(ctxT(), Config{Driver: "sqlite", DSN: "file:smoke2?mode=memory&cache=shared"})
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	if _, err := st2.Users().GetBySub(ctxT(), 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("fresh store: %v", err)
	}
}

func TestWithTxRollsBack(t *testing.T) {
	st := NewTestStore(t)
	wantErr := errors.New("boom")
	err := st.WithTx(ctxT(), func(tx Store) error {
		mkUser(t, tx, "acct-tx", 7)
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("tx err: %v", err)
	}
	if _, err := st.Users().GetBySub(ctxT(), 7); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rollback failed: %v", err)
	}
}
