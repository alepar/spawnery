package authsvc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	authv1 "spawnery/gen/auth/v1"
	"spawnery/internal/authsvc/store"
	"spawnery/internal/pki"
)

// rotateFailOnceStore wraps a real store and forces the FIRST Rotate (the commit/promote) to fail,
// exercising the write-ahead recovery path. StageRotation and every other op delegate untouched.
type rotateFailOnceStore struct {
	store.Store
	links *rotateFailOnceLinks
}

func (s *rotateFailOnceStore) GitHubLinks() store.GitHubLinkRepo { return s.links }

type rotateFailOnceLinks struct {
	store.GitHubLinkRepo
	failNextRotate bool
}

func (l *rotateFailOnceLinks) Rotate(ctx context.Context, secretID string, rot store.GitHubTokenRotation) (store.GitHubLink, error) {
	if l.failNextRotate {
		l.failNextRotate = false
		return store.GitHubLink{}, errors.New("injected commit failure")
	}
	return l.GitHubLinkRepo.Rotate(ctx, secretID, rot)
}

type testGitHubMintProvider struct {
	calls       int
	wantRefresh string
	next        GitHubUserToken
	err         error
}

func (p *testGitHubMintProvider) AuthorizeURL(string, string, string) string { return "" }
func (p *testGitHubMintProvider) Exchange(context.Context, string, string, string) (string, error) {
	return "", errors.New("exchange unused in mint tests")
}
func (p *testGitHubMintProvider) FetchUser(context.Context, string) (GitHubUser, error) {
	return GitHubUser{}, errors.New("fetch user unused in mint tests")
}
func (p *testGitHubMintProvider) RefreshUserAccessToken(_ context.Context, refreshToken string) (GitHubUserToken, error) {
	p.calls++
	if p.wantRefresh != "" && refreshToken != p.wantRefresh {
		return GitHubUserToken{}, errors.New("unexpected refresh token")
	}
	if p.err != nil {
		return GitHubUserToken{}, p.err
	}
	return p.next, nil
}

func seedGitHubLink(t *testing.T, st store.Store, accessExpiresAt int64) {
	t.Helper()
	if err := st.GitHubLinks().Upsert(context.Background(), store.GitHubLink{
		SecretID:             "gh-main",
		AccountID:            "acct-1",
		Host:                 "github.com",
		Login:                "alice",
		GithubUserID:         "123456",
		AppClientID:          "Iv1.spawnerytest",
		RefreshToken:         "ghr_old",
		RefreshExpiresAtUnix: 2200000000,
		AccessToken:          "ghu_current",
		AccessExpiresAtUnix:  accessExpiresAt,
		TokenType:            "bearer",
		Version:              11,
		DeliveryID:           "delivery-sp1-gen3-gh-main-v11",
		UpdatedAt:            1770000000,
	}); err != nil {
		t.Fatalf("seed github link: %v", err)
	}
}

func newMintAS(t *testing.T, opts ...Option) *Service {
	t.Helper()
	root, _ := pki.NewRootCA("R")
	inter, _ := root.NewIntermediate(pki.ClassSelfHosted)
	return New(root.Cert, inter, opts...)
}

func mintReq() *authv1.MintGitHubAccessTokenRequest {
	return &authv1.MintGitHubAccessTokenRequest{
		RequestId:    "mint-sp1-gen3-gh-main-repo987",
		SpawnId:      "sp1",
		Generation:   3,
		RepositoryId: "987",
		LinkRef: &authv1.GitHubLinkRef{
			SecretId:   "gh-main",
			Version:    11,
			DeliveryId: "delivery-sp1-gen3-gh-main-v11",
		},
	}
}

func TestMintGitHubAccessTokenRotatesCustodialRefreshAndReturnsOnlyAccess(t *testing.T) {
	now := time.Unix(1770000000, 0)
	st := store.NewTestStore(t)
	seedGitHubLink(t, st, now.Add(time.Minute).Unix())
	provider := &testGitHubMintProvider{
		wantRefresh: "ghr_old",
		next: GitHubUserToken{
			AccessToken:          "ghu_rotated",
			AccessExpiresAtUnix:  now.Add(8 * time.Hour).Unix(),
			RefreshToken:         "ghr_rotated",
			RefreshExpiresAtUnix: now.Add(180 * 24 * time.Hour).Unix(),
			TokenType:            "bearer",
		},
	}

	var authz GitHubMintAuthorization
	var fanout GitHubAccessTokenFanout
	svc := newMintAS(t,
		WithClock(func() time.Time { return now }),
		WithGitHubMinting(st, provider),
		WithNodeIdentityExtractor(func(context.Context) (string, bool) { return "node-1", true }),
		WithGitHubMintAuthorizer(GitHubMintAuthorizerFunc(func(_ context.Context, got GitHubMintAuthorization) error {
			authz = got
			return nil
		})),
		WithGitHubAccessTokenFanout(GitHubAccessTokenFanoutFunc(func(_ context.Context, got GitHubAccessTokenFanout) error {
			fanout = got
			return nil
		})),
	)

	resp, err := svc.MintGitHubAccessToken(context.Background(), connect.NewRequest(mintReq()))
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if resp.Msg.GetAccessToken() != "ghu_rotated" || !resp.Msg.GetRefreshed() {
		t.Fatalf("mint response = %+v", resp.Msg)
	}
	if provider.calls != 1 {
		t.Fatalf("provider calls = %d, want 1", provider.calls)
	}
	if authz.NodeID != "node-1" || authz.SpawnID != "sp1" || authz.SecretID != "gh-main" || authz.Generation != 3 || authz.Version != 11 {
		t.Fatalf("authz request = %+v", authz)
	}
	if fanout.SecretID != "gh-main" || fanout.Version != 12 || fanout.DeliveryID != "github-access-gh-main-v12" ||
		fanout.AccessToken != "ghu_rotated" || fanout.RepositoryID != "987" {
		t.Fatalf("fanout request = %+v", fanout)
	}
	got, err := st.GitHubLinks().Get(context.Background(), "gh-main")
	if err != nil {
		t.Fatalf("get link: %v", err)
	}
	if got.RefreshToken != "ghr_rotated" || got.AccessToken != "ghu_rotated" ||
		got.Version != 12 || got.DeliveryID != "github-access-gh-main-v12" {
		t.Fatalf("AS did not persist rotated refresh chain before returning: %+v", got)
	}
}

func TestMintGitHubAccessTokenRetryAfterFanoutFailureUsesCurrentFreshToken(t *testing.T) {
	now := time.Unix(1770000000, 0)
	st := store.NewTestStore(t)
	seedGitHubLink(t, st, now.Add(time.Minute).Unix())
	provider := &testGitHubMintProvider{
		wantRefresh: "ghr_old",
		next: GitHubUserToken{
			AccessToken:          "ghu_rotated",
			AccessExpiresAtUnix:  now.Add(8 * time.Hour).Unix(),
			RefreshToken:         "ghr_rotated",
			RefreshExpiresAtUnix: now.Add(180 * 24 * time.Hour).Unix(),
			TokenType:            "bearer",
		},
	}

	fanoutCalls := 0
	var fanouts []GitHubAccessTokenFanout
	svc := newMintAS(t,
		WithClock(func() time.Time { return now }),
		WithGitHubMinting(st, provider),
		WithNodeIdentityExtractor(func(context.Context) (string, bool) { return "node-1", true }),
		WithGitHubMintAuthorizer(GitHubMintAuthorizerFunc(func(context.Context, GitHubMintAuthorization) error {
			return nil
		})),
		WithGitHubAccessTokenFanout(GitHubAccessTokenFanoutFunc(func(_ context.Context, got GitHubAccessTokenFanout) error {
			fanoutCalls++
			fanouts = append(fanouts, got)
			if fanoutCalls == 1 {
				return errors.New("cp fanout unavailable")
			}
			return nil
		})),
	)

	_, err := svc.MintGitHubAccessToken(context.Background(), connect.NewRequest(mintReq()))
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("initial mint code = %v err=%v", connect.CodeOf(err), err)
	}
	got, err := st.GitHubLinks().Get(context.Background(), "gh-main")
	if err != nil {
		t.Fatalf("get link after failed fanout: %v", err)
	}
	if got.RefreshToken != "ghr_rotated" || got.AccessToken != "ghu_rotated" ||
		got.Version != 12 || got.DeliveryID != "github-access-gh-main-v12" {
		t.Fatalf("AS did not persist rotated link before fanout failure: %+v", got)
	}

	resp, err := svc.MintGitHubAccessToken(context.Background(), connect.NewRequest(mintReq()))
	if err != nil {
		t.Fatalf("retry mint with stale link_ref: %v", err)
	}
	if resp.Msg.GetAccessToken() != "ghu_rotated" || resp.Msg.GetRefreshed() {
		t.Fatalf("retry response = %+v", resp.Msg)
	}
	if provider.calls != 1 {
		t.Fatalf("provider calls = %d, want 1", provider.calls)
	}
	if len(fanouts) != 2 {
		t.Fatalf("fanouts = %d, want 2", len(fanouts))
	}
	retryFanout := fanouts[1]
	if retryFanout.SecretID != "gh-main" || retryFanout.Version != 12 || retryFanout.DeliveryID != "github-access-gh-main-v12" ||
		retryFanout.AccessToken != "ghu_rotated" || retryFanout.RepositoryID != "987" {
		t.Fatalf("retry fanout request = %+v", retryFanout)
	}
}

func TestMintGitHubAccessTokenRequiresNodeIdentity(t *testing.T) {
	now := time.Unix(1770000000, 0)
	st := store.NewTestStore(t)
	seedGitHubLink(t, st, now.Add(time.Minute).Unix())
	provider := &testGitHubMintProvider{}
	svc := newMintAS(t,
		WithClock(func() time.Time { return now }),
		WithGitHubMinting(st, provider),
		WithNodeIdentityExtractor(func(context.Context) (string, bool) { return "", false }),
	)

	_, err := svc.MintGitHubAccessToken(context.Background(), connect.NewRequest(mintReq()))
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("mint without node identity code = %v err=%v", connect.CodeOf(err), err)
	}
	if provider.calls != 0 {
		t.Fatalf("provider called without node identity")
	}
}

func TestMintGitHubAccessTokenUsesCurrentSharedTokenWhenFresh(t *testing.T) {
	now := time.Unix(1770000000, 0)
	st := store.NewTestStore(t)
	seedGitHubLink(t, st, now.Add(2*time.Hour).Unix())
	provider := &testGitHubMintProvider{}
	svc := newMintAS(t,
		WithClock(func() time.Time { return now }),
		WithGitHubMinting(st, provider),
		WithNodeIdentityExtractor(func(context.Context) (string, bool) { return "node-1", true }),
		WithGitHubMintAuthorizer(GitHubMintAuthorizerFunc(func(context.Context, GitHubMintAuthorization) error {
			return nil
		})),
	)

	resp, err := svc.MintGitHubAccessToken(context.Background(), connect.NewRequest(mintReq()))
	if err != nil {
		t.Fatalf("mint fresh: %v", err)
	}
	if resp.Msg.GetAccessToken() != "ghu_current" || resp.Msg.GetRefreshed() {
		t.Fatalf("fresh response = %+v", resp.Msg)
	}
	if provider.calls != 0 {
		t.Fatalf("provider calls = %d, want 0 for deduped fresh token", provider.calls)
	}
}

func TestMintGitHubAccessTokenHonorsCPHostConfirmation(t *testing.T) {
	now := time.Unix(1770000000, 0)
	st := store.NewTestStore(t)
	seedGitHubLink(t, st, now.Add(time.Minute).Unix())
	provider := &testGitHubMintProvider{}
	svc := newMintAS(t,
		WithClock(func() time.Time { return now }),
		WithGitHubMinting(st, provider),
		WithNodeIdentityExtractor(func(context.Context) (string, bool) { return "wrong-node", true }),
		WithGitHubMintAuthorizer(GitHubMintAuthorizerFunc(func(context.Context, GitHubMintAuthorization) error {
			return connect.NewError(connect.CodePermissionDenied, errors.New("node does not host link"))
		})),
	)

	_, err := svc.MintGitHubAccessToken(context.Background(), connect.NewRequest(mintReq()))
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("mint unauthorized node code = %v err=%v", connect.CodeOf(err), err)
	}
	if provider.calls != 0 {
		t.Fatalf("provider called before CP host confirmation")
	}
}

// GAP-B (post-rotation DB-write failure): GitHub rotates, the write-ahead stage succeeds, but the
// commit/promote fails. The staged tuple must survive; a retry promotes it WITHOUT re-calling GitHub.
func TestMintGitHubAccessTokenRecoversStagedRotationAfterCommitFailure(t *testing.T) {
	now := time.Unix(1770000000, 0)
	real := store.NewTestStore(t)
	st := &rotateFailOnceStore{Store: real, links: &rotateFailOnceLinks{GitHubLinkRepo: real.GitHubLinks(), failNextRotate: true}}
	seedGitHubLink(t, st, now.Add(time.Minute).Unix())
	provider := &testGitHubMintProvider{
		wantRefresh: "ghr_old",
		next: GitHubUserToken{
			AccessToken: "ghu_rotated", AccessExpiresAtUnix: now.Add(8 * time.Hour).Unix(),
			RefreshToken: "ghr_rotated", RefreshExpiresAtUnix: now.Add(180 * 24 * time.Hour).Unix(),
			TokenType: "bearer",
		},
	}
	svc := newMintAS(t,
		WithClock(func() time.Time { return now }),
		WithGitHubMinting(st, provider),
		WithNodeIdentityExtractor(func(context.Context) (string, bool) { return "node-1", true }),
		WithGitHubMintAuthorizer(GitHubMintAuthorizerFunc(func(context.Context, GitHubMintAuthorization) error { return nil })),
		WithGitHubAccessTokenFanout(GitHubAccessTokenFanoutFunc(func(context.Context, GitHubAccessTokenFanout) error { return nil })),
	)

	// First attempt: GitHub rotates, stage succeeds, commit fails -> error, but pending staged.
	if _, err := svc.MintGitHubAccessToken(context.Background(), connect.NewRequest(mintReq())); err == nil {
		t.Fatalf("expected commit-failure error on first attempt")
	}
	staged, err := real.GitHubLinks().Get(context.Background(), "gh-main")
	if err != nil {
		t.Fatalf("get after stage: %v", err)
	}
	if staged.PendingRefreshToken != "ghr_rotated" || staged.PendingVersion != 12 {
		t.Fatalf("write-ahead did not persist the rotation: %+v", staged)
	}
	if staged.Version != 11 {
		t.Fatalf("live version must not advance before commit: %+v", staged)
	}

	// Retry: must promote the staged tuple WITHOUT calling GitHub again.
	resp, err := svc.MintGitHubAccessToken(context.Background(), connect.NewRequest(mintReq()))
	if err != nil {
		t.Fatalf("retry mint: %v", err)
	}
	if resp.Msg.GetAccessToken() != "ghu_rotated" || !resp.Msg.GetRefreshed() {
		t.Fatalf("retry response = %+v", resp.Msg)
	}
	if provider.calls != 1 {
		t.Fatalf("provider calls = %d, want 1 (no GitHub re-call on recovery)", provider.calls)
	}
	got, err := real.GitHubLinks().Get(context.Background(), "gh-main")
	if err != nil {
		t.Fatalf("get after recovery: %v", err)
	}
	if got.Version != 12 || got.RefreshToken != "ghr_rotated" || got.PendingRefreshToken != "" {
		t.Fatalf("recovery did not promote+clear pending: %+v", got)
	}
}

// GAP-B (lost GitHub response): the rotation result was lost; on retry GitHub rejects the now-dead
// refresh token. The AS must surface a TERMINAL, non-retryable relink_required (CodeFailedPrecondition),
// mark the link, and NOT keep calling GitHub on further retries.
func TestMintGitHubAccessTokenLostRotationSurfacesTerminalRelink(t *testing.T) {
	now := time.Unix(1770000000, 0)
	st := store.NewTestStore(t)
	seedGitHubLink(t, st, now.Add(time.Minute).Unix())
	provider := &testGitHubMintProvider{
		wantRefresh: "ghr_old",
		err:         fmt.Errorf("github refresh rejected: %w", ErrRefreshRejected),
	}
	svc := newMintAS(t,
		WithClock(func() time.Time { return now }),
		WithGitHubMinting(st, provider),
		WithNodeIdentityExtractor(func(context.Context) (string, bool) { return "node-1", true }),
		WithGitHubMintAuthorizer(GitHubMintAuthorizerFunc(func(context.Context, GitHubMintAuthorization) error { return nil })),
	)

	_, err := svc.MintGitHubAccessToken(context.Background(), connect.NewRequest(mintReq()))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("lost-rotation code = %v err=%v, want FailedPrecondition", connect.CodeOf(err), err)
	}
	if !strings.Contains(err.Error(), "relink_required") {
		t.Fatalf("error must carry relink_required token: %v", err)
	}
	got, err := st.GitHubLinks().Get(context.Background(), "gh-main")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !got.RelinkRequired {
		t.Fatalf("link not marked relink_required: %+v", got)
	}
	// Retry on a marked link fast-fails terminally WITHOUT another GitHub call.
	_, err = svc.MintGitHubAccessToken(context.Background(), connect.NewRequest(mintReq()))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("retry code = %v, want FailedPrecondition", connect.CodeOf(err))
	}
	if provider.calls != 1 {
		t.Fatalf("provider calls = %d, want 1 (no re-call once relink-marked)", provider.calls)
	}
}
