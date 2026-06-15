package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/uptrace/bun"
)

type transferSetRepo struct{ db bun.IDB }

func (r *transferSetRepo) Create(ctx context.Context, ts TransferSet) error {
	if ts.ID == "" || ts.SpawnID == "" {
		return fmt.Errorf("store: transfer set id and spawn id are required")
	}
	if ts.Kind == "" {
		ts.Kind = TransferSetMigration
	}
	switch ts.Kind {
	case TransferSetMigration:
		if ts.SourceSpawnID == "" {
			ts.SourceSpawnID = ts.SpawnID
		}
	case TransferSetFork:
		if ts.SourceSpawnID == "" || ts.ForkSpawnID == "" {
			return fmt.Errorf("store: fork transfer set requires source and fork spawn ids")
		}
		if ts.SpawnID != ts.ForkSpawnID {
			return fmt.Errorf("store: fork transfer set spawn id must match fork spawn id")
		}
	default:
		return fmt.Errorf("store: unknown transfer set kind %q", ts.Kind)
	}
	if ts.TransferKeyStatus == "" {
		ts.TransferKeyStatus = TransferKeyPending
	}
	if ts.Status == "" {
		ts.Status = TransferSetPending
	}
	if ts.MountManifestPinsJSON == "" {
		b, err := json.Marshal(ts.MountManifestPins)
		if err != nil {
			return err
		}
		ts.MountManifestPinsJSON = string(b)
	}
	if ts.RootfsArtifactPinsJSON == "" {
		b, err := json.Marshal(ts.RootfsArtifactPins)
		if err != nil {
			return err
		}
		ts.RootfsArtifactPinsJSON = string(b)
	}
	if ts.TransferKeyMetadataJSON == "" {
		b, err := json.Marshal(ts.TransferKeyCiphertextMetadata)
		if err != nil {
			return err
		}
		ts.TransferKeyMetadataJSON = string(b)
	}
	_, err := r.db.NewInsert().Model(&ts).Exec(ctx)
	return err
}

func (r *transferSetRepo) Get(ctx context.Context, id string) (TransferSet, error) {
	var ts TransferSet
	err := r.db.NewSelect().Model(&ts).Where("id = ?", id).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return TransferSet{}, ErrNotFound
	}
	if err != nil {
		return TransferSet{}, err
	}
	if err := decodeTransferSetPins(&ts); err != nil {
		return TransferSet{}, err
	}
	return ts, nil
}

func (r *transferSetRepo) ListFailedForks(ctx context.Context) ([]TransferSet, error) {
	return r.listForks(ctx, func(q *bun.SelectQuery) *bun.SelectQuery {
		return q.Where("mts.status = ?", TransferSetFailed)
	})
}

func (r *transferSetRepo) ListReclaimableForks(ctx context.Context, staleRestoringBefore int64) ([]TransferSet, error) {
	return r.listForks(ctx, func(q *bun.SelectQuery) *bun.SelectQuery {
		return q.WhereGroup(" AND ", func(q *bun.SelectQuery) *bun.SelectQuery {
			return q.WhereOr("mts.status = ?", TransferSetFailed).
				WhereOr("(mts.status = ? AND mts.updated_at < ?)", TransferSetRestoring, staleRestoringBefore)
		})
	})
}

func (r *transferSetRepo) listForks(ctx context.Context, where func(*bun.SelectQuery) *bun.SelectQuery) ([]TransferSet, error) {
	var out []TransferSet
	q := r.db.NewSelect().Model(&out).
		Join("JOIN spawns AS fork ON fork.id = mts.fork_spawn_id").
		Where("mts.kind = ?", TransferSetFork).
		Where("fork.status <> ?", Deleted).
		Order("mts.created_at ASC", "mts.id ASC")
	err := where(q).Scan(ctx)
	if err != nil {
		return nil, err
	}
	for i := range out {
		if err := decodeTransferSetPins(&out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (r *transferSetRepo) SetPins(ctx context.Context, id string, sourceGeneration uint64, mountPins map[string]string, rootfsPins []RootfsArtifactPin, updatedAt int64) error {
	mounts, err := json.Marshal(mountPins)
	if err != nil {
		return err
	}
	rootfs, err := json.Marshal(rootfsPins)
	if err != nil {
		return err
	}
	res, err := r.db.NewUpdate().Model((*TransferSet)(nil)).
		Set("mount_manifest_pins = ?", string(mounts)).
		Set("rootfs_artifact_pins = ?", string(rootfs)).
		Set("updated_at = ?", updatedAt).
		Where("id = ?", id).
		Where("source_generation = ?", sourceGeneration).
		Exec(ctx)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrConflict
	}
	return nil
}

func (r *transferSetRepo) SetTargetNode(ctx context.Context, id string, targetNodeID string, updatedAt int64) error {
	return r.updateOne(ctx, id, func(q *bun.UpdateQuery) *bun.UpdateQuery {
		return q.Set("target_node_id = ?", targetNodeID).Set("updated_at = ?", updatedAt)
	})
}

func (r *transferSetRepo) SetStatus(ctx context.Context, id string, status TransferSetStatus, updatedAt int64) error {
	return r.updateOne(ctx, id, func(q *bun.UpdateQuery) *bun.UpdateQuery {
		return q.Set("status = ?", status).Set("updated_at = ?", updatedAt)
	})
}

func (r *transferSetRepo) SetTransferKeyStatus(ctx context.Context, id string, status TransferKeyStatus, updatedAt int64) error {
	return r.updateOne(ctx, id, func(q *bun.UpdateQuery) *bun.UpdateQuery {
		return q.Set("transfer_key_status = ?", status).Set("updated_at = ?", updatedAt)
	})
}

func (r *transferSetRepo) updateOne(ctx context.Context, id string, set func(*bun.UpdateQuery) *bun.UpdateQuery) error {
	res, err := set(r.db.NewUpdate().Model((*TransferSet)(nil)).Where("id = ?", id)).Exec(ctx)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrNotFound
	}
	return nil
}

func decodeTransferSetPins(ts *TransferSet) error {
	if ts.MountManifestPinsJSON == "" {
		ts.MountManifestPins = map[string]string{}
	} else if err := json.Unmarshal([]byte(ts.MountManifestPinsJSON), &ts.MountManifestPins); err != nil {
		return fmt.Errorf("store: decode transfer-set mount pins: %w", err)
	}
	if ts.RootfsArtifactPinsJSON == "" {
		ts.RootfsArtifactPins = nil
	} else if err := json.Unmarshal([]byte(ts.RootfsArtifactPinsJSON), &ts.RootfsArtifactPins); err != nil {
		return fmt.Errorf("store: decode transfer-set rootfs pins: %w", err)
	}
	if ts.TransferKeyMetadataJSON == "" {
		ts.TransferKeyCiphertextMetadata = map[string]string{}
	} else if err := json.Unmarshal([]byte(ts.TransferKeyMetadataJSON), &ts.TransferKeyCiphertextMetadata); err != nil {
		return fmt.Errorf("store: decode transfer-set key metadata: %w", err)
	}
	return nil
}
