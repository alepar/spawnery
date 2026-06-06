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
	// Keep created_at on conflict (DO NOTHING), then replace the binary set.
	if _, err := r.db.NewInsert().Model(&img).
		On("CONFLICT (image) DO NOTHING").
		Exec(ctx); err != nil {
		return err
	}
	if _, err := r.db.NewDelete().Model((*AgentImageBinary)(nil)).
		Where("image = ?", img.Image).Exec(ctx); err != nil {
		return err
	}
	for _, b := range binaries {
		row := AgentImageBinary{Image: img.Image, Binary: b}
		if _, err := r.db.NewInsert().Model(&row).Exec(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (r *agentImageRepo) Get(ctx context.Context, image string) (AgentImage, error) {
	var img AgentImage
	err := r.db.NewSelect().Model(&img).Where("image = ?", image).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return AgentImage{}, ErrNotFound
	}
	return img, err
}

func (r *agentImageRepo) Binaries(ctx context.Context, image string) ([]string, error) {
	var rows []AgentImageBinary
	if err := r.db.NewSelect().Model(&rows).
		Where("image = ?", image).Order("binary_name ASC").Scan(ctx); err != nil {
		return nil, err
	}
	out := make([]string, len(rows))
	for i, row := range rows {
		out[i] = row.Binary
	}
	return out, nil
}

func (r *agentImageRepo) List(ctx context.Context) ([]AgentImage, error) {
	var out []AgentImage
	err := r.db.NewSelect().Model(&out).Order("image ASC").Scan(ctx)
	return out, err
}
