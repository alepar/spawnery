package authsvc

import (
	"context"
	"errors"

	"spawnery/internal/authsvc/store"
)

var errForcedStoreFailure = errors.New("forced store failure")

type storeFaults struct {
	failInsert       bool
	failOldestFamily bool
	failRevoke       bool
	failAppend       bool
}

type failingStore struct {
	store.Store
	faults *storeFaults
}

func (s *failingStore) WithTx(ctx context.Context, fn func(store.Store) error) error {
	return s.Store.WithTx(ctx, func(tx store.Store) error {
		return fn(&failingStore{Store: tx, faults: s.faults})
	})
}

func (s *failingStore) RefreshSessions() store.RefreshSessionRepo {
	return &failingRefreshSessions{RefreshSessionRepo: s.Store.RefreshSessions(), faults: s.faults}
}

func (s *failingStore) Revocations() store.RevocationRepo {
	return &failingRevocations{RevocationRepo: s.Store.Revocations(), faults: s.faults}
}

type failingRefreshSessions struct {
	store.RefreshSessionRepo
	faults *storeFaults
}

func (r *failingRefreshSessions) Insert(ctx context.Context, session store.RefreshSession) error {
	if r.faults.failInsert {
		return errForcedStoreFailure
	}
	return r.RefreshSessionRepo.Insert(ctx, session)
}

func (r *failingRefreshSessions) OldestFamily(ctx context.Context, accountID string) (string, error) {
	if r.faults.failOldestFamily {
		return "", errForcedStoreFailure
	}
	return r.RefreshSessionRepo.OldestFamily(ctx, accountID)
}

func (r *failingRefreshSessions) RevokeFamily(ctx context.Context, familyID string) ([]string, error) {
	if r.faults.failRevoke {
		return nil, errForcedStoreFailure
	}
	return r.RefreshSessionRepo.RevokeFamily(ctx, familyID)
}

type failingRevocations struct {
	store.RevocationRepo
	faults *storeFaults
}

func (r *failingRevocations) Append(ctx context.Context, event store.RevocationEvent) (int64, error) {
	if r.faults.failAppend {
		return 0, errForcedStoreFailure
	}
	return r.RevocationRepo.Append(ctx, event)
}
