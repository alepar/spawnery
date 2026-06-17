package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/uptrace/bun"
)

type githubLinkRepo struct {
	db     bun.IDB
	cipher TokenCipher
}

func aadRefresh(secretID string) string { return "github_link/refresh/v1/" + secretID }
func aadAccess(secretID string) string  { return "github_link/access/v1/" + secretID }

// decryptTokens converts the on-disk ciphertext token columns back to plaintext
// in place. Callers receive plaintext GitHubLink fields, unchanged in shape.
// It also decrypts pending_* tokens when present (non-empty ciphertext).
func (r *githubLinkRepo) decryptTokens(l *GitHubLink) error {
	if r.cipher == nil {
		return ErrCipherRequired
	}
	rt, err := r.cipher.Decrypt(l.RefreshToken, aadRefresh(l.SecretID))
	if err != nil {
		return err
	}
	at, err := r.cipher.Decrypt(l.AccessToken, aadAccess(l.SecretID))
	if err != nil {
		return err
	}
	l.RefreshToken, l.AccessToken = rt, at
	if l.PendingRefreshToken != "" {
		if l.PendingRefreshToken, err = r.cipher.Decrypt(l.PendingRefreshToken, aadRefresh(l.SecretID)); err != nil {
			return err
		}
	}
	if l.PendingAccessToken != "" {
		if l.PendingAccessToken, err = r.cipher.Decrypt(l.PendingAccessToken, aadAccess(l.SecretID)); err != nil {
			return err
		}
	}
	return nil
}

func (r *githubLinkRepo) Get(ctx context.Context, secretID string) (GitHubLink, error) {
	var link GitHubLink
	err := r.db.NewSelect().Model(&link).
		Where("secret_id = ? AND revoked = 0", secretID).
		Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return GitHubLink{}, ErrNotFound
	}
	if err != nil {
		return GitHubLink{}, err
	}
	if err := r.decryptTokens(&link); err != nil {
		return GitHubLink{}, err
	}
	return link, nil
}

func (r *githubLinkRepo) Upsert(ctx context.Context, link GitHubLink) error {
	if r.cipher == nil {
		return ErrCipherRequired
	}
	enc := link
	var err error
	if enc.RefreshToken, err = r.cipher.Encrypt(link.RefreshToken, aadRefresh(link.SecretID)); err != nil {
		return err
	}
	if enc.AccessToken, err = r.cipher.Encrypt(link.AccessToken, aadAccess(link.SecretID)); err != nil {
		return err
	}
	_, err = r.db.NewInsert().Model(&enc).
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
	if r.cipher == nil {
		return GitHubLink{}, ErrCipherRequired
	}
	if rot.TokenType == "" {
		rot.TokenType = "bearer"
	}
	encRefresh, err := r.cipher.Encrypt(rot.RefreshToken, aadRefresh(secretID))
	if err != nil {
		return GitHubLink{}, err
	}
	encAccess, err := r.cipher.Encrypt(rot.AccessToken, aadAccess(secretID))
	if err != nil {
		return GitHubLink{}, err
	}
	var link GitHubLink
	q := r.db.NewUpdate().Model(&link).
		Set("refresh_token = ?", encRefresh).
		Set("refresh_expires_at_unix = ?", rot.RefreshExpiresAtUnix).
		Set("access_token = ?", encAccess).
		Set("access_expires_at_unix = ?", rot.AccessExpiresAtUnix).
		Set("token_type = ?", rot.TokenType).
		Set("updated_at = ?", rot.UpdatedAt).
		// promote = clear pending staging slot atomically
		Set("pending_refresh_token = NULL").
		Set("pending_refresh_expires_at_unix = NULL").
		Set("pending_access_token = NULL").
		Set("pending_access_expires_at_unix = NULL").
		Set("pending_token_type = NULL").
		Set("pending_version = NULL").
		Set("relink_required = 0")
	if rot.Version != 0 {
		q = q.Set("version = ?", rot.Version)
	}
	if rot.DeliveryID != "" {
		q = q.Set("delivery_id = ?", rot.DeliveryID)
	}
	err = q.Where("secret_id = ? AND revoked = 0", secretID).
		Returning("*").
		Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return GitHubLink{}, ErrNotFound
	}
	if err != nil {
		return GitHubLink{}, err
	}
	if err := r.decryptTokens(&link); err != nil {
		return GitHubLink{}, err
	}
	return link, nil
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

// StageRotation write-ahead persists a rotated tuple into the pending_* columns (encrypted),
// without changing the live tuple/version. Idempotent: re-staging overwrites.
func (r *githubLinkRepo) StageRotation(ctx context.Context, secretID string, stage GitHubStagedRotation) error {
	if r.cipher == nil {
		return ErrCipherRequired
	}
	if stage.TokenType == "" {
		stage.TokenType = "bearer"
	}
	encRefresh, err := r.cipher.Encrypt(stage.RefreshToken, aadRefresh(secretID))
	if err != nil {
		return err
	}
	encAccess, err := r.cipher.Encrypt(stage.AccessToken, aadAccess(secretID))
	if err != nil {
		return err
	}
	res, err := r.db.NewUpdate().Model((*GitHubLink)(nil)).
		Set("pending_refresh_token = ?", encRefresh).
		Set("pending_refresh_expires_at_unix = ?", stage.RefreshExpiresAtUnix).
		Set("pending_access_token = ?", encAccess).
		Set("pending_access_expires_at_unix = ?", stage.AccessExpiresAtUnix).
		Set("pending_token_type = ?", stage.TokenType).
		Set("pending_version = ?", stage.Version).
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

// MarkRelinkRequired flags the link's refresh chain as provably broken (terminal). Subsequent
// mints fast-fail; the owner must relink.
func (r *githubLinkRepo) MarkRelinkRequired(ctx context.Context, secretID string, at int64) error {
	res, err := r.db.NewUpdate().Model((*GitHubLink)(nil)).
		Set("relink_required = 1").
		Set("updated_at = ?", at).
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

// PeekMeta reads token-free metadata for secretID INCLUDING revoked rows (the redeem ownership
// guard + identity-continuity peek). ErrNotFound when absent.
func (r *githubLinkRepo) PeekMeta(ctx context.Context, secretID string) (GitHubLinkMeta, error) {
	var m GitHubLinkMeta
	err := r.db.NewSelect().Model(&m).Where("secret_id = ?", secretID).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return GitHubLinkMeta{}, ErrNotFound
	}
	if err != nil {
		return GitHubLinkMeta{}, err
	}
	return m, nil
}

// List returns token-free metadata for all links of accountID, including revoked/relink_required.
func (r *githubLinkRepo) List(ctx context.Context, accountID string) ([]GitHubLinkMeta, error) {
	var rows []GitHubLinkMeta
	if err := r.db.NewSelect().Model(&rows).Where("account_id = ?", accountID).Order("secret_id ASC").Scan(ctx); err != nil {
		return nil, err
	}
	return rows, nil
}

// RedeemUpsert inserts-or-relinks atomically: new row -> version=1; conflict (even a revoked row)
// -> version = existing.version+1 and clears revoked/relink_required, DB-side via RETURNING (no
// app Get->+1 race, no deliveryID collision across revoke->relink). Returns the persisted link with
// plaintext tokens (containment invariant: caller provides plaintext, tokens encrypted at rest).
func (r *githubLinkRepo) RedeemUpsert(ctx context.Context, link GitHubLink) (GitHubLink, error) {
	if r.cipher == nil {
		return GitHubLink{}, ErrCipherRequired
	}
	enc := link
	enc.Version = 1 // genesis for a brand-new row; ON CONFLICT overrides with ghl.version+1
	enc.DeliveryID = "github-access-" + link.SecretID + "-v1"
	enc.Revoked = false
	enc.RelinkRequired = false
	var err error
	if enc.RefreshToken, err = r.cipher.Encrypt(link.RefreshToken, aadRefresh(link.SecretID)); err != nil {
		return GitHubLink{}, err
	}
	if enc.AccessToken, err = r.cipher.Encrypt(link.AccessToken, aadAccess(link.SecretID)); err != nil {
		return GitHubLink{}, err
	}
	// out starts as a copy of enc (has all fields); RETURNING overwrites version+delivery_id.
	out := enc
	err = r.db.NewInsert().Model(&enc).
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
		Set("updated_at = EXCLUDED.updated_at").
		Set("version = ghl.version + 1").
		Set("delivery_id = 'github-access-' || ghl.secret_id || '-v' || (ghl.version + 1)").
		Set("revoked = 0").
		Set("revoked_at = NULL").
		Set("relink_required = 0").
		Returning("version, delivery_id").
		Scan(ctx, &out)
	if err != nil {
		return GitHubLink{}, err
	}
	// Return plaintext tokens to caller (not the ciphertext we stored).
	out.RefreshToken, out.AccessToken = link.RefreshToken, link.AccessToken
	return out, nil
}
