package store

import (
	"context"
	"errors"

	"github.com/uptrace/bun"
)

type revocationRepo struct{ db bun.IDB }

func (r *revocationRepo) Append(ctx context.Context, ev RevocationEvent) (int64, error) {
	if ev.TokenIDs != "" {
		return 0, errors.New("authsvc/store: legacy token ids are not accepted")
	}
	if ev.AccountID == "" || ev.RevokedAt <= 0 {
		return 0, errors.New("authsvc/store: invalid revocation event")
	}
	for _, token := range ev.RevokedTokens {
		if token.TokenID == "" || token.RetainUntil <= ev.RevokedAt {
			return 0, errors.New("authsvc/store: invalid revoked token")
		}
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
	return ev.Seq, nil
}

func (r *revocationRepo) Since(ctx context.Context, seq int64) ([]RevocationEvent, error) {
	var evs []RevocationEvent
	err := r.db.NewSelect().Model(&evs).
		Where("seq > ?", seq).
		OrderExpr("seq ASC").
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	for i := range evs {
		if err := r.db.NewSelect().Model(&evs[i].RevokedTokens).
			Where("event_seq = ?", evs[i].Seq).OrderExpr("token_id ASC").Scan(ctx); err != nil {
			return nil, err
		}
	}
	return evs, err
}
