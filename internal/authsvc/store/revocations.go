package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/uptrace/bun"
)

type revocationRepo struct{ db bun.IDB }

func (r *revocationRepo) Append(ctx context.Context, ev RevocationEvent) (int64, error) {
	if top, ok := r.db.(*bun.DB); ok {
		var seq int64
		err := top.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			var err error
			seq, err = (&revocationRepo{db: tx}).append(ctx, ev)
			return err
		})
		return seq, err
	}
	return r.append(ctx, ev)
}

func (r *revocationRepo) append(ctx context.Context, ev RevocationEvent) (int64, error) {
	if ev.AccountID == "" || ev.RevokedAt <= 0 {
		return 0, errors.New("authsvc/store: invalid revocation event")
	}
	if ev.FamilyID == "" {
		if ev.RevokeTokensIssuedBefore <= 0 {
			return 0, errors.New("authsvc/store: account revocation cutoff required")
		}
	} else if ev.RevokeTokensIssuedBefore != 0 || len(ev.RevokedTokens) == 0 {
		return 0, errors.New("authsvc/store: invalid family revocation")
	}
	seen := make(map[string]struct{}, len(ev.RevokedTokens))
	for _, token := range ev.RevokedTokens {
		if token.TokenID == "" || token.RetainUntil <= ev.RevokedAt {
			return 0, errors.New("authsvc/store: invalid revoked token")
		}
		if _, ok := seen[token.TokenID]; ok {
			return 0, errors.New("authsvc/store: duplicate revoked token")
		}
		seen[token.TokenID] = struct{}{}
	}
	ev.Seq = 0 // assigned by AUTOINCREMENT
	if _, err := r.db.NewInsert().Model(&ev).Exec(ctx); err != nil {
		return 0, err
	}
	for i := range ev.RevokedTokens {
		ev.RevokedTokens[i].EventSeq = ev.Seq
	}
	if len(ev.RevokedTokens) > 0 {
		if _, err := r.db.NewInsert().Model(&ev.RevokedTokens).Exec(ctx); err != nil {
			return 0, err
		}
	}
	if ev.FamilyID == "" {
		if _, err := r.db.ExecContext(ctx, `
			DELETE FROM revocation_events
			WHERE account_id = ? AND family_id = '' AND seq NOT IN (
				SELECT seq FROM revocation_events
				WHERE account_id = ? AND family_id = ''
				ORDER BY revoke_tokens_issued_before DESC, seq DESC LIMIT 1
			)`, ev.AccountID, ev.AccountID); err != nil {
			return 0, err
		}
	}
	return ev.Seq, nil
}

func (r *revocationRepo) PageAfter(ctx context.Context, seq int64, limit int, now int64) ([]RevocationEvent, bool, error) {
	if seq < 0 || limit <= 0 || now < 0 {
		return nil, false, errors.New("authsvc/store: invalid revocation page request")
	}
	if top, ok := r.db.(*bun.DB); ok {
		var events []RevocationEvent
		var hasMore bool
		err := top.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			var err error
			events, hasMore, err = (&revocationRepo{db: tx}).pageAfter(ctx, seq, limit, now)
			return err
		})
		return events, hasMore, err
	}
	return r.pageAfter(ctx, seq, limit, now)
}

func (r *revocationRepo) pageAfter(ctx context.Context, seq int64, limit int, now int64) ([]RevocationEvent, bool, error) {
	if _, err := r.db.NewDelete().Model((*RevokedToken)(nil)).Where("retain_until <= ?", now).Exec(ctx); err != nil {
		return nil, false, fmt.Errorf("prune revoked tokens: %w", err)
	}
	if _, err := r.db.ExecContext(ctx, `
		DELETE FROM revocation_events
		WHERE family_id <> ''
		  AND NOT EXISTS (
			SELECT 1 FROM revocation_event_tokens
			WHERE revocation_event_tokens.event_seq = revocation_events.seq
		  )`); err != nil {
		return nil, false, fmt.Errorf("prune empty family revocations: %w", err)
	}
	if _, err := r.db.ExecContext(ctx, `
		DELETE FROM revocation_events AS stale
		WHERE stale.family_id = ''
		  AND EXISTS (
			SELECT 1 FROM revocation_events AS keep
			WHERE keep.account_id = stale.account_id AND keep.family_id = ''
			  AND (keep.revoke_tokens_issued_before > stale.revoke_tokens_issued_before
			    OR (keep.revoke_tokens_issued_before = stale.revoke_tokens_issued_before AND keep.seq > stale.seq))
		  )`); err != nil {
		return nil, false, fmt.Errorf("compact account revocations: %w", err)
	}

	var events []RevocationEvent
	if err := r.db.NewSelect().Model(&events).
		Where("seq > ?", seq).OrderExpr("seq ASC").Limit(limit + 1).Scan(ctx); err != nil {
		return nil, false, fmt.Errorf("read revocation page: %w", err)
	}
	hasMore := len(events) > limit
	if hasMore {
		events = events[:limit]
	}
	for i := range events {
		if err := r.db.NewSelect().Model(&events[i].RevokedTokens).
			Where("event_seq = ?", events[i].Seq).OrderExpr("token_id ASC").Scan(ctx); err != nil {
			return nil, false, fmt.Errorf("read revocation tokens: %w", err)
		}
	}
	return events, hasMore, nil
}
