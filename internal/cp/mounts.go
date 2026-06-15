package cp

import (
	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/cp/store"
)

// storeToNodeMounts converts persisted spawn mounts to the node StartSpawn wire form.
func storeToNodeMounts(in []store.Mount) []*nodev1.MountBinding {
	if len(in) == 0 {
		return nil
	}
	out := make([]*nodev1.MountBinding, len(in))
	for i, m := range in {
		out[i] = &nodev1.MountBinding{Name: m.Name, BackendUri: m.BackendURI}
	}
	return out
}
