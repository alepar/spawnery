package cp

import (
	"fmt"
	"strings"

	cpv1 "spawnery/gen/cp/v1"
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
		out[i] = &nodev1.MountBinding{
			Name:               m.Name,
			BackendUri:         m.BackendURI,
			CredentialSecretId: m.CredentialSecretID,
			CreateIfMissing:    m.CreateIfMissing,
			RepositoryId:       m.RepositoryID,
		}
	}
	return out
}

func mergeCreateSpawnMounts(decls []store.MountDecl, req []*cpv1.MountBinding) ([]store.Mount, error) {
	declared := make(map[string]struct{}, len(decls))
	out := make([]store.Mount, len(decls))
	for i, decl := range decls {
		declared[decl.Name] = struct{}{}
		out[i] = store.Mount{Name: decl.Name, BackendURI: "scratch"}
	}

	byName := make(map[string]store.Mount, len(req))
	for _, binding := range req {
		if binding == nil {
			continue
		}
		name := strings.TrimSpace(binding.GetName())
		if name == "" {
			return nil, fmt.Errorf("mount binding name must not be empty")
		}
		if _, ok := declared[name]; !ok {
			return nil, fmt.Errorf("mount binding %q does not match any declared mount", name)
		}
		if _, dup := byName[name]; dup {
			return nil, fmt.Errorf("duplicate mount binding %q", name)
		}
		backendURI := binding.GetBackendUri()
		if strings.TrimSpace(backendURI) == "" {
			backendURI = "scratch"
		}
		if strings.HasPrefix(backendURI, "github:") && strings.TrimSpace(binding.GetCredentialSecretId()) == "" {
			return nil, fmt.Errorf("github mount binding %q requires credential_secret_id", name)
		}
		byName[name] = store.Mount{
			Name:               name,
			BackendURI:         backendURI,
			CredentialSecretID: strings.TrimSpace(binding.GetCredentialSecretId()),
			CreateIfMissing:    binding.GetCreateIfMissing(),
			RepositoryID:       strings.TrimSpace(binding.GetRepositoryId()),
		}
	}

	for i := range out {
		if binding, ok := byName[out[i].Name]; ok {
			out[i].BackendURI = binding.BackendURI
			out[i].CredentialSecretID = binding.CredentialSecretID
			out[i].CreateIfMissing = binding.CreateIfMissing
			out[i].RepositoryID = binding.RepositoryID
		}
	}
	return out, nil
}
