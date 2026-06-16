package store

import (
	"errors"
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
