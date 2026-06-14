package store

import "context"

func (r *spawnRepo) GetArtifacts(ctx context.Context, id string) ([]Artifact, error) {
	var out []Artifact
	err := r.db.NewSelect().Model(&out).Where("spawn_id = ?", id).Order("artifact_id ASC").Scan(ctx)
	return out, err
}

func (r *spawnRepo) AddArtifacts(ctx context.Context, id string, artifacts []Artifact) error {
	for i := range artifacts {
		artifacts[i].SpawnID = id
		if _, err := r.db.NewInsert().Model(&artifacts[i]).Exec(ctx); err != nil {
			return err
		}
	}
	return nil
}
