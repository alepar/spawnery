package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"testing"
)

func TestGitHubLinksRoundTripRotateAndRevoke(t *testing.T) {
	st := NewTestStore(t)

	link := GitHubLink{
		SecretID:             "gh-main",
		AccountID:            "acct-1",
		Host:                 "github.com",
		Login:                "alice",
		GithubUserID:         "123456",
		AppClientID:          "Iv1.spawnerytest",
		RefreshToken:         "ghr_old",
		RefreshExpiresAtUnix: 2000,
		AccessToken:          "ghu_old",
		AccessExpiresAtUnix:  1000,
		TokenType:            "bearer",
		Version:              11,
		DeliveryID:           "delivery-sp1-gen3-gh-main-v11",
		UpdatedAt:            10,
	}
	if err := st.GitHubLinks().Upsert(ctxT(), link); err != nil {
		t.Fatalf("upsert link: %v", err)
	}
	got, err := st.GitHubLinks().Get(ctxT(), "gh-main")
	if err != nil {
		t.Fatalf("get link: %v", err)
	}
	if got.RefreshToken != "ghr_old" || got.AccessToken != "ghu_old" || got.Version != 11 {
		t.Fatalf("link round-trip lost custodial token fields: %+v", got)
	}

	rotated, err := st.GitHubLinks().Rotate(ctxT(), "gh-main", GitHubTokenRotation{
		RefreshToken:         "ghr_new",
		RefreshExpiresAtUnix: 3000,
		AccessToken:          "ghu_new",
		AccessExpiresAtUnix:  1900,
		TokenType:            "bearer",
		Version:              12,
		DeliveryID:           "github-access-gh-main-v12",
		UpdatedAt:            20,
	})
	if err != nil {
		t.Fatalf("rotate link: %v", err)
	}
	if rotated.RefreshToken != "ghr_new" || rotated.AccessToken != "ghu_new" ||
		rotated.Version != 12 || rotated.DeliveryID != "github-access-gh-main-v12" || rotated.UpdatedAt != 20 {
		t.Fatalf("rotate returned stale row: %+v", rotated)
	}
	got, err = st.GitHubLinks().Get(ctxT(), "gh-main")
	if err != nil {
		t.Fatalf("get rotated link: %v", err)
	}
	if got.RefreshToken != "ghr_new" || got.AccessToken != "ghu_new" ||
		got.AccessExpiresAtUnix != 1900 || got.Version != 12 || got.DeliveryID != "github-access-gh-main-v12" {
		t.Fatalf("rotation not persisted before return: %+v", got)
	}

	if err := st.GitHubLinks().Revoke(ctxT(), "gh-main", 30); err != nil {
		t.Fatalf("revoke link: %v", err)
	}
	if _, err := st.GitHubLinks().Get(ctxT(), "gh-main"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked link must not mint, got %v", err)
	}
}

func TestGitHubLinksRotateRequiresLiveLink(t *testing.T) {
	st := NewTestStore(t)
	if _, err := st.GitHubLinks().Rotate(ctxT(), "missing", GitHubTokenRotation{
		RefreshToken: "ghr_new",
		AccessToken:  "ghu_new",
		TokenType:    "bearer",
		UpdatedAt:    1,
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rotate missing link: want ErrNotFound, got %v", err)
	}
}

func TestGitHubLinksEncryptedAtRest(t *testing.T) {
	ctx := context.Background()
	dsn := "file:as_atrest?mode=memory&cache=shared&_pragma=foreign_keys(1)"
	cipher, err := NewAESGCMTokenCipher(bytesRepeat32(0x2a))
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	st, err := Open(ctx, Config{Driver: "sqlite", DSN: dsn, TokenCipher: cipher})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	link := GitHubLink{
		SecretID: "gh-main", AccountID: "acct-1", Host: "github.com", Login: "alice",
		GithubUserID: "123456", AppClientID: "Iv1.test",
		RefreshToken: "ghr_plain", RefreshExpiresAtUnix: 2000,
		AccessToken: "ghu_plain", AccessExpiresAtUnix: 1000,
		TokenType: "bearer", Version: 1, DeliveryID: "d1", UpdatedAt: 10,
	}
	if err := st.GitHubLinks().Upsert(ctx, link); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// Read the RAW columns on a second handle to the same shared in-memory DB.
	raw, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	defer raw.Close()
	var rt, at string
	if err := raw.QueryRowContext(ctx,
		"SELECT refresh_token, access_token FROM github_links WHERE secret_id = ?", "gh-main",
	).Scan(&rt, &at); err != nil {
		t.Fatalf("raw select: %v", err)
	}
	if rt == "ghr_plain" || at == "ghu_plain" || rt == "" || at == "" {
		t.Fatalf("tokens stored in plaintext at rest: refresh=%q access=%q", rt, at)
	}
	if _, err := base64.RawStdEncoding.DecodeString(rt); err != nil {
		t.Fatalf("stored refresh_token is not base64 ciphertext: %v", err)
	}

	// A store opened with a DIFFERENT key on the same DB cannot read the tokens.
	otherCipher, _ := NewAESGCMTokenCipher(bytesRepeat32(0x55))
	st2, err := Open(ctx, Config{Driver: "sqlite", DSN: dsn, TokenCipher: otherCipher})
	if err != nil {
		t.Fatalf("open store2: %v", err)
	}
	t.Cleanup(func() { st2.Close() })
	if _, err := st2.GitHubLinks().Get(ctx, "gh-main"); err == nil {
		t.Fatal("Get with the wrong key must fail (tokens are key-bound at rest)")
	}
}

func TestGitHubLinksRequireCipher(t *testing.T) {
	ctx := context.Background()
	dsn := "file:as_nocipher?mode=memory&cache=shared&_pragma=foreign_keys(1)"
	st, err := Open(ctx, Config{Driver: "sqlite", DSN: dsn}) // no TokenCipher
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	err = st.GitHubLinks().Upsert(ctx, GitHubLink{SecretID: "x", RefreshToken: "r", TokenType: "bearer"})
	if !errors.Is(err, ErrCipherRequired) {
		t.Fatalf("Upsert without cipher must fail-closed with ErrCipherRequired, got %v", err)
	}
}

func bytesRepeat32(b byte) []byte {
	out := make([]byte, 32)
	for i := range out {
		out[i] = b
	}
	return out
}

// seedLink upserts a canonical test GitHub link with secret_id "gh-main" (version 11).
func seedLink(t *testing.T, st Store) {
	t.Helper()
	if err := st.GitHubLinks().Upsert(ctxT(), GitHubLink{
		SecretID:             "gh-main",
		AccountID:            "acct-1",
		Host:                 "github.com",
		Login:                "alice",
		GithubUserID:         "123456",
		AppClientID:          "Iv1.spawnerytest",
		RefreshToken:         "ghr_old",
		RefreshExpiresAtUnix: 2200000000,
		AccessToken:          "ghu_current",
		AccessExpiresAtUnix:  1770001000,
		TokenType:            "bearer",
		Version:              11,
		DeliveryID:           "delivery-sp1-gen3-gh-main-v11",
		UpdatedAt:            1770000000,
	}); err != nil {
		t.Fatalf("seedLink upsert: %v", err)
	}
}

func TestGitHubLinksStageRotationPersistsPendingAndGetDecrypts(t *testing.T) {
	ctx := context.Background()
	st := NewTestStore(t)
	seedLink(t, st)

	if err := st.GitHubLinks().StageRotation(ctx, "gh-main", GitHubStagedRotation{
		RefreshToken: "ghr_next", RefreshExpiresAtUnix: 2300000000,
		AccessToken: "ghu_next", AccessExpiresAtUnix: 2100000000,
		TokenType: "bearer", Version: 12,
	}); err != nil {
		t.Fatalf("stage: %v", err)
	}
	got, err := st.GitHubLinks().Get(ctx, "gh-main")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.PendingRefreshToken != "ghr_next" || got.PendingAccessToken != "ghu_next" ||
		got.PendingVersion != 12 || got.PendingRefreshExpiresAtUnix != 2300000000 {
		t.Fatalf("pending not staged/decrypted: %+v", got)
	}
	// live tuple must be unchanged by staging
	if got.RefreshToken == "ghr_next" || got.Version == 12 {
		t.Fatalf("staging must not touch the live tuple: %+v", got)
	}
}

func TestGitHubLinksRotatePromotesAndClearsPending(t *testing.T) {
	ctx := context.Background()
	st := NewTestStore(t)
	seedLink(t, st)
	if err := st.GitHubLinks().StageRotation(ctx, "gh-main", GitHubStagedRotation{
		RefreshToken: "ghr_next", RefreshExpiresAtUnix: 2300000000,
		AccessToken: "ghu_next", AccessExpiresAtUnix: 2100000000, TokenType: "bearer", Version: 12,
	}); err != nil {
		t.Fatalf("stage: %v", err)
	}
	if _, err := st.GitHubLinks().Rotate(ctx, "gh-main", GitHubTokenRotation{
		RefreshToken: "ghr_next", RefreshExpiresAtUnix: 2300000000,
		AccessToken: "ghu_next", AccessExpiresAtUnix: 2100000000, TokenType: "bearer",
		Version: 12, DeliveryID: "github-access-gh-main-v12", UpdatedAt: 1770000001,
	}); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	got, err := st.GitHubLinks().Get(ctx, "gh-main")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Version != 12 || got.RefreshToken != "ghr_next" {
		t.Fatalf("rotate did not promote: %+v", got)
	}
	if got.PendingRefreshToken != "" || got.PendingVersion != 0 || got.PendingAccessToken != "" {
		t.Fatalf("rotate must clear pending: %+v", got)
	}
}

func TestGitHubLinksMarkRelinkRequired(t *testing.T) {
	ctx := context.Background()
	st := NewTestStore(t)
	seedLink(t, st)
	if err := st.GitHubLinks().MarkRelinkRequired(ctx, "gh-main", 1770000002); err != nil {
		t.Fatalf("mark: %v", err)
	}
	got, err := st.GitHubLinks().Get(ctx, "gh-main")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !got.RelinkRequired {
		t.Fatalf("relink_required not set: %+v", got)
	}
}

// newLinksTestStore opens an isolated in-memory store with cipher for github_link tests.
func newLinksTestStore(t *testing.T) Store {
	t.Helper()
	name := strings.NewReplacer("/", "_", "#", "_").Replace(t.Name())
	dsn := "file:as_links_" + name + "?mode=memory&cache=shared&_pragma=foreign_keys(1)"
	cipher, err := NewAESGCMTokenCipher(bytesRepeat32(0x2b))
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	st, err := Open(context.Background(), Config{Driver: "sqlite", DSN: dsn, TokenCipher: cipher})
	if err != nil {
		t.Fatalf("open links test store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestGitHubLinksPeekMetaIgnoresRevoked(t *testing.T) {
	ctx := context.Background()
	st := newLinksTestStore(t)
	if err := st.GitHubLinks().Upsert(ctx, GitHubLink{
		SecretID: "gh:a", AccountID: "acct-a", Host: "github.com", Login: "alice",
		GithubUserID: "42", AppClientID: "app", RefreshToken: "ghr_x", AccessToken: "ghu_x",
		TokenType: "bearer", Version: 1, DeliveryID: "github-access-gh:a-v1", UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := st.GitHubLinks().Revoke(ctx, "gh:a", 2); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	// PeekMeta must still return the row even though it's revoked.
	m, err := st.GitHubLinks().PeekMeta(ctx, "gh:a")
	if err != nil {
		t.Fatalf("PeekMeta after revoke: %v", err)
	}
	if m.AccountID != "acct-a" || m.GithubUserID != "42" || !m.Revoked {
		t.Fatalf("PeekMeta unexpected result: %+v", m)
	}
}

func TestGitHubLinksListSurfacesStatuses(t *testing.T) {
	ctx := context.Background()
	st := newLinksTestStore(t)
	if err := st.GitHubLinks().Upsert(ctx, GitHubLink{
		SecretID: "gh:a", AccountID: "acct-a", Host: "github.com", Login: "alice",
		GithubUserID: "42", AppClientID: "app", RefreshToken: "ghr_x", AccessToken: "ghu_x",
		TokenType: "bearer", Version: 1, DeliveryID: "github-access-gh:a-v1", UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := st.GitHubLinks().MarkRelinkRequired(ctx, "gh:a", 2); err != nil {
		t.Fatalf("mark relink: %v", err)
	}
	rows, err := st.GitHubLinks().List(ctx, "acct-a")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("List returned %d rows, want 1", len(rows))
	}
	if !rows[0].RelinkRequired || rows[0].Revoked {
		t.Fatalf("List row unexpected: %+v", rows[0])
	}
	// Account-scoped: another account sees nothing.
	other, err := st.GitHubLinks().List(ctx, "acct-other")
	if err != nil {
		t.Fatalf("List other: %v", err)
	}
	if len(other) != 0 {
		t.Fatalf("List other returned %d rows, want 0", len(other))
	}
}

func TestRedeemUpsertGenesisAndRelinkBumpAcrossRevoke(t *testing.T) {
	ctx := context.Background()
	st := newLinksTestStore(t)

	base := GitHubLink{
		SecretID: "gh:a", AccountID: "acct-a", Host: "github.com", Login: "alice",
		GithubUserID: "42", AppClientID: "app", RefreshToken: "ghr_r1", AccessToken: "ghu_a1",
		TokenType: "bearer", UpdatedAt: 1,
	}
	got, err := st.GitHubLinks().RedeemUpsert(ctx, base)
	if err != nil {
		t.Fatalf("RedeemUpsert genesis: %v", err)
	}
	if got.Version != 1 {
		t.Fatalf("genesis version = %d, want 1", got.Version)
	}
	wantDelivery := "github-access-gh:a-v1"
	if got.DeliveryID != wantDelivery {
		t.Fatalf("genesis deliveryID = %q, want %q", got.DeliveryID, wantDelivery)
	}
	if got.RefreshToken != "ghr_r1" || got.AccessToken != "ghu_a1" {
		t.Fatalf("genesis tokens not plaintext: %+v", got)
	}

	if err := st.GitHubLinks().Revoke(ctx, "gh:a", 2); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	base.RefreshToken, base.AccessToken = "ghr_r2", "ghu_a2"
	got2, err := st.GitHubLinks().RedeemUpsert(ctx, base)
	if err != nil {
		t.Fatalf("RedeemUpsert after revoke: %v", err)
	}
	if got2.Version != 2 {
		t.Fatalf("relink version = %d, want 2", got2.Version)
	}
	if got2.DeliveryID == wantDelivery {
		t.Fatalf("relink deliveryID must differ from v1: %q", got2.DeliveryID)
	}

	// Get filters revoked=0, so it should find the relinked row.
	live, err := st.GitHubLinks().Get(ctx, "gh:a")
	if err != nil {
		t.Fatalf("Get after relink: %v", err)
	}
	if live.Version != 2 || live.RefreshToken != "ghr_r2" {
		t.Fatalf("Get after relink: %+v", live)
	}
}

func TestRedeemUpsertConcurrentDistinctVersions(t *testing.T) {
	ctx := context.Background()
	st := newLinksTestStore(t)

	base := GitHubLink{
		SecretID: "gh:c", AccountID: "acct-c", Host: "github.com", Login: "carol",
		GithubUserID: "99", AppClientID: "app", RefreshToken: "ghr_c", AccessToken: "ghu_c",
		TokenType: "bearer", UpdatedAt: 1,
	}

	type result struct {
		version uint64
		err     error
	}
	const n = 8
	results := make(chan result, n)
	for i := range n {
		go func(i int) {
			link := base
			link.RefreshToken = "ghr_c" + strconv.Itoa(i)
			link.AccessToken = "ghu_c" + strconv.Itoa(i)
			res, err := st.GitHubLinks().RedeemUpsert(ctx, link)
			results <- result{version: res.Version, err: err}
		}(i)
	}
	seen := map[uint64]bool{}
	for range n {
		r := <-results
		if r.err != nil {
			t.Fatalf("concurrent RedeemUpsert: %v", r.err)
		}
		if seen[r.version] {
			t.Fatalf("duplicate version %d (versions not DB-serialized)", r.version)
		}
		seen[r.version] = true
	}
	if len(seen) != n {
		t.Fatalf("expected %d distinct versions, got %d", n, len(seen))
	}
}
