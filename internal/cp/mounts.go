package cp

import (
	"fmt"
	"strings"

	cpv1 "spawnery/gen/cp/v1"
	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/cp/store"
	"spawnery/internal/storage"
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
	declared := make(map[string]store.MountDecl, len(decls))
	out := make([]store.Mount, len(decls))
	for i, decl := range decls {
		declared[decl.Name] = decl
		out[i] = store.Mount{Name: decl.Name, BackendURI: "scratch"}
	}

	byName := make(map[string]store.Mount, len(req))
	bound := make(map[string]struct{}, len(req))
	for _, binding := range req {
		if binding == nil {
			continue
		}
		name := strings.TrimSpace(binding.GetName())
		if name == "" {
			return nil, fmt.Errorf("mount binding name must not be empty")
		}
		decl, ok := declared[name]
		if !ok {
			return nil, fmt.Errorf("mount binding %q does not match any declared mount", name)
		}
		if _, dup := byName[name]; dup {
			return nil, fmt.Errorf("duplicate mount binding %q", name)
		}
		bound[name] = struct{}{}
		backendURI := strings.TrimSpace(binding.GetBackendUri())

		if decl.Github {
			// GitHub SLOT (D1): the user supplies owner/repo; the credential is auto-resolved by the
			// CP (T3) into a node-JIT-mint link-ref — NOT named by the client here (no token in T4).
			if backendURI == "" || !strings.HasPrefix(backendURI, "github:") {
				return nil, fmt.Errorf("github mount slot %q requires a github:owner/repo binding", name)
			}
			cfg, perr := storage.ParseGitHubURI(backendURI)
			if perr != nil {
				return nil, fmt.Errorf("github mount slot %q: %w", name, perr)
			}
			byName[name] = store.Mount{
				Name:            name,
				BackendURI:      "github:" + cfg.Owner + "/" + cfg.Repo,
				CreateIfMissing: binding.GetCreateIfMissing(),
				RepositoryID:    strings.TrimSpace(binding.GetRepositoryId()),
				// CredentialSecretID intentionally empty: T3 (CP auto-resolve) sets the link-ref;
				// no token/credential is named by the client for a slot.
			}
			continue
		}

		// Non-slot declared mount: legacy behavior preserved (additive). A github: backend on a
		// non-slot mount still requires an explicit credential_secret_id; otherwise default scratch.
		if backendURI == "" {
			backendURI = "scratch"
		}
		if strings.HasPrefix(backendURI, "github:") {
			if strings.TrimSpace(binding.GetCredentialSecretId()) == "" {
				return nil, fmt.Errorf("mount %q is not a github slot; a github:owner/repo binding requires credential_secret_id", name)
			}
		}
		byName[name] = store.Mount{
			Name:               name,
			BackendURI:         backendURI,
			CredentialSecretID: strings.TrimSpace(binding.GetCredentialSecretId()),
			CreateIfMissing:    binding.GetCreateIfMissing(),
			RepositoryID:       strings.TrimSpace(binding.GetRepositoryId()),
		}
	}

	// A github slot has no default repo — the user MUST bind owner/repo at create (D1).
	for _, decl := range decls {
		if decl.Github {
			if _, ok := bound[decl.Name]; !ok {
				return nil, fmt.Errorf("github mount slot %q requires owner/repo (supply --mount %s=github:owner/repo)", decl.Name, decl.Name)
			}
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
