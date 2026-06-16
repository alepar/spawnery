package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/uptrace/bun"
)

type githubLinkRepo struct{ db bun.IDB }

func (r *githubLinkRepo) Get(ctx context.Context, secretID string) (GitHubLink, error) {
	var link GitHubLink
	err := r.db.NewSelect().Model(&link).
		Where("secret_id = ? AND revoked = 0", secretID).
		Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return GitHubLink{}, ErrNotFound
	}
	return link, err
}

func (r *githubLinkRepo) Upsert(ctx context.Context, link GitHubLink) error {
	_, err := r.db.NewInsert().Model(&link).
		On("CONFLICT (secret_id) DO UPDATE").
		Set("account_id = EXCLUDED.account_id").
		Set("host = EXCLUDED.host").
		Set("login = EXCLUDED.login").
		Set("github_user_id = EXCLUDED.github_user_id").
		Set("app_client_id = EXCLUDED.app_client_id").
		Set("refresh_token = EXCLUDED.refresh_token").
		Set("refresh_expires_at_unix = EXCLUDED.refresh_expires_at_unix").
		Set("access_token = EXCLUDED.access_token").
		Set("access_expires_at_unix = EXCLUDED.access_expires_at_unix").
		Set("token_type = EXCLUDED.token_type").
		Set("version = EXCLUDED.version").
		Set("delivery_id = EXCLUDED.delivery_id").
		Set("updated_at = EXCLUDED.updated_at").
		Set("revoked = 0").
		Set("revoked_at = NULL").
		Exec(ctx)
	return err
}

func (r *githubLinkRepo) Rotate(ctx context.Context, secretID string, rot GitHubTokenRotation) (GitHubLink, error) {
	if rot.TokenType == "" {
		rot.TokenType = "bearer"
	}
	var link GitHubLink
	q := r.db.NewUpdate().Model(&link).
		Set("refresh_token = ?", rot.RefreshToken).
		Set("refresh_expires_at_unix = ?", rot.RefreshExpiresAtUnix).
		Set("access_token = ?", rot.AccessToken).
		Set("access_expires_at_unix = ?", rot.AccessExpiresAtUnix).
		Set("token_type = ?", rot.TokenType).
		Set("updated_at = ?", rot.UpdatedAt)
	if rot.Version != 0 {
		q = q.Set("version = ?", rot.Version)
	}
	if rot.DeliveryID != "" {
		q = q.Set("delivery_id = ?", rot.DeliveryID)
	}
	err := q.Where("secret_id = ? AND revoked = 0", secretID).
		Returning("*").
		Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return GitHubLink{}, ErrNotFound
	}
	return link, err
}

func (r *githubLinkRepo) Revoke(ctx context.Context, secretID string, revokedAt int64) error {
	res, err := r.db.NewUpdate().Model((*GitHubLink)(nil)).
		Set("revoked = 1").
		Set("revoked_at = ?", revokedAt).
		Set("updated_at = ?", revokedAt).
		Where("secret_id = ? AND revoked = 0", secretID).
		Exec(ctx)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
