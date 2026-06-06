package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/uptrace/bun"
)

type agentImageRepo struct{ db bun.IDB }

var _ AgentImageRepo = (*agentImageRepo)(nil)

func (r *agentImageRepo) Upsert(ctx context.Context, img AgentImage, binaries []string) error {
	return errors.New("store: agentImageRepo.Upsert not implemented")
}

func (r *agentImageRepo) Get(ctx context.Context, image string) (AgentImage, error) {
	return AgentImage{}, ErrNotFound
}

func (r *agentImageRepo) Binaries(ctx context.Context, image string) ([]string, error) {
	return nil, nil
}

func (r *agentImageRepo) List(ctx context.Context) ([]AgentImage, error) {
	return nil, nil
}

// referenced in Task 2; keeps the sql import used once Get is implemented.
var _ = sql.ErrNoRows
