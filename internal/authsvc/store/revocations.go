package store

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/uptrace/bun"
	"google.golang.org/protobuf/proto"

	authv1 "spawnery/gen/auth/v1"
)

type revocationRepo struct{ db bun.IDB }

const (
	maxRevokedTokensPerEvent  = 1024
	maxRevocationPayloadBytes = 60 << 10
)

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
	if err := validateRevocationEvent(ev); err != nil {
		return 0, err
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
		if err := r.compactAccountRevocations(ctx, ev.AccountID); err != nil {
			return 0, err
		}
	}
	return ev.Seq, nil
}

func validateRevocationEvent(ev RevocationEvent) error {
	if ev.AccountID == "" || ev.RevokedAt <= 0 {
		return errors.New("authsvc/store: invalid revocation event")
	}
	if ev.FamilyID == "" {
		if ev.RevokeTokensIssuedBefore <= 0 {
			return errors.New("authsvc/store: account revocation cutoff required")
		}
	} else if ev.RevokeTokensIssuedBefore != 0 || len(ev.RevokedTokens) == 0 {
		return errors.New("authsvc/store: invalid family revocation")
	}
	if len(ev.RevokedTokens) > maxRevokedTokensPerEvent {
		return errors.New("authsvc/store: revocation event has too many tokens")
	}
	seen := make(map[string]struct{}, len(ev.RevokedTokens))
	revokedTokens := make([]*authv1.RevokedToken, 0, len(ev.RevokedTokens))
	for _, token := range ev.RevokedTokens {
		if token.TokenID == "" || token.RetainUntil <= ev.RevokedAt {
			return errors.New("authsvc/store: invalid revoked token")
		}
		if _, ok := seen[token.TokenID]; ok {
			return errors.New("authsvc/store: duplicate revoked token")
		}
		seen[token.TokenID] = struct{}{}
		revokedTokens = append(revokedTokens, &authv1.RevokedToken{
			TokenId: token.TokenID, RetainUntil: token.RetainUntil,
		})
	}
	body := &authv1.RevocationEntry{
		Seq: math.MaxInt64, AccountId: ev.AccountID, FamilyId: ev.FamilyID,
		RevokedAt: ev.RevokedAt, RevokeTokensIssuedBefore: ev.RevokeTokensIssuedBefore,
		RevokedTokens: revokedTokens,
	}
	if proto.Size(body) > maxRevocationPayloadBytes {
		return errors.New("authsvc/store: revocation event payload is too large")
	}
	return nil
}

func (r *revocationRepo) compactAccountRevocations(ctx context.Context, accountID string) error {
	var keep RevocationEvent
	if err := r.db.NewSelect().Model(&keep).
		Where("account_id = ? AND family_id = ''", accountID).
		OrderExpr("revoke_tokens_issued_before DESC, seq DESC").Limit(1).
		Scan(ctx, &keep); err != nil {
		return fmt.Errorf("select account revocation keeper: %w", err)
	}
	if _, err := r.db.ExecContext(ctx, `
		INSERT INTO revocation_event_tokens (event_seq, token_id, retain_until)
		SELECT ?, token.token_id, MAX(token.retain_until)
		FROM revocation_event_tokens AS token
		JOIN revocation_events AS source ON source.seq = token.event_seq
		WHERE source.account_id = ? AND source.family_id = ''
		  AND source.revoke_tokens_issued_before = ?
		GROUP BY token.token_id
		ON CONFLICT(event_seq, token_id) DO UPDATE SET
		  retain_until = MAX(revocation_event_tokens.retain_until, excluded.retain_until)`,
		keep.Seq, accountID, keep.RevokeTokensIssuedBefore); err != nil {
		return fmt.Errorf("merge account revocation tokens: %w", err)
	}
	if err := r.db.NewSelect().Model(&keep.RevokedTokens).
		Where("event_seq = ?", keep.Seq).OrderExpr("token_id ASC").Scan(ctx); err != nil {
		return fmt.Errorf("read compacted account revocation tokens: %w", err)
	}
	if err := validateRevocationEvent(keep); err != nil {
		return fmt.Errorf("validate compacted account revocation: %w", err)
	}
	if _, err := r.db.ExecContext(ctx, `
		DELETE FROM revocation_events
		WHERE account_id = ? AND family_id = '' AND seq <> ?`, accountID, keep.Seq); err != nil {
		return fmt.Errorf("delete superseded account revocations: %w", err)
	}
	return nil
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
	var accountIDs []string
	if err := r.db.NewSelect().Table("revocation_events").Distinct().Column("account_id").
		Where("family_id = ''").Scan(ctx, &accountIDs); err != nil {
		return nil, false, fmt.Errorf("select account revocations: %w", err)
	}
	for _, accountID := range accountIDs {
		if err := r.compactAccountRevocations(ctx, accountID); err != nil {
			return nil, false, fmt.Errorf("compact account revocations: %w", err)
		}
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
