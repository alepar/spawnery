package store

import (
	"context"

	"github.com/uptrace/bun"
)

// bunStore implements Store over a bun.IDB (either *bun.DB at the top level or a bun.Tx inside WithTx).
type bunStore struct {
	db     bun.IDB
	closer *bun.DB // non-nil only for the top-level store (so WithTx children don't close the pool)
}

func (s *bunStore) Close() error {
	if s.closer != nil {
		return s.closer.Close()
	}
	return nil
}

func (s *bunStore) WithTx(ctx context.Context, fn func(tx Store) error) error {
	top, ok := s.db.(*bun.DB)
	if !ok {
		return fn(s) // already inside a tx — run inline (no nested tx)
	}
	return top.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		return fn(&bunStore{db: tx})
	})
}

// Owners and Apps — implemented in Task 4. Spawns — implemented in Task 5.
func (s *bunStore) Owners() OwnerRepo                         { return &ownerRepo{db: s.db} }
func (s *bunStore) Apps() AppRepo                             { return &appRepo{db: s.db} }
func (s *bunStore) Spawns() SpawnRepo                         { return &spawnRepo{db: s.db} }
func (s *bunStore) AgentImages() AgentImageRepo               { return &agentImageRepo{db: s.db} }
func (s *bunStore) TransferSets() TransferSetRepo             { return &transferSetRepo{db: s.db} }
func (s *bunStore) Secrets() SecretRepo                       { return &secretRepo{db: s.db} }
func (s *bunStore) Profiles() ProfileRepo                     { return &profileRepo{db: s.db} }
func (s *bunStore) CustomizationCatalog() CustomizationCatalogRepo {
	return &customizationCatalogRepo{db: s.db}
}
