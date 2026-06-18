package cp

import (
	"fmt"
	"strings"

	cpv1 "spawnery/gen/cp/v1"
	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/cp/store"
	"spawnery/internal/storage"
)

// githubLinkSecretIDPrefix is the reserved namespace for AS-custodial GitHub link secret ids.
// MUST stay equal to internal/authsvc.githubSecretIDPrefix ("gh:"): the node's at-provision mint
// presents this exact secret_id to the AS, which resolves it to the owner's custodial refresh chain.
// CP-derived only — clients may never name a gh: id (rejected in mergeCreateSpawnMounts).
const githubLinkSecretIDPrefix = "gh:"

// githubMintLinkSecretID is the per-owner JIT-mint link-ref id. The AS stores the link under
// gh:<accountID>; in every wired lane the CP spawn owner == the AS account principal (shared session
// key), so gh:<owner> resolves the creator's link. (Assumption validated end-to-end in T8.)
func githubMintLinkSecretID(owner string) string {
	return githubLinkSecretIDPrefix + strings.TrimSpace(owner)
}

// isGitHubMintLinkRef reports whether a credential id is a CP-derived gh: mint link-ref (vs an
// owner-sealed catalog secret id). gh: ids have no catalog row and are minted at provision (Approach 2).
func isGitHubMintLinkRef(id string) bool {
	return strings.HasPrefix(strings.TrimSpace(id), githubLinkSecretIDPrefix)
}

// storeToNodeMounts converts persisted spawn mounts to the node StartSpawn wire form.
func storeToNodeMounts(in []store.Mount) []*nodev1.MountBinding {
	if len(in) == 0 {
		return nil
	}
	out := make([]*nodev1.MountBinding, len(in))
	for i, m := range in {
		mb := &nodev1.MountBinding{
			Name:               m.Name,
			BackendUri:         m.BackendURI,
			CredentialSecretId: m.CredentialSecretID,
			CreateIfMissing:    m.CreateIfMissing,
			RepositoryId:       m.RepositoryID,
		}
		// Approach 2: a github mount whose credential is a CP-derived gh: link-ref carries a node
		// JIT-mint descriptor. The node mints the access token at provision (renders into its node-only
		// GitHubCredentialsRoot) — no owner-sealed delivery, no token via the CP. credential_secret_id is
		// intentionally left as gh:<owner> for signed-intent correspondence; the node ignores it on the
		// delivery path (it consumes only delivered SealedSecrets) and mints off github_mint_ref presence.
		if strings.HasPrefix(m.BackendURI, "github:") && isGitHubMintLinkRef(m.CredentialSecretID) {
			mb.GithubMintRef = &nodev1.GitHubMintRef{SecretId: strings.TrimSpace(m.CredentialSecretID)}
		}
		out[i] = mb
	}
	return out
}

func mergeCreateSpawnMounts(decls []store.MountDecl, req []*cpv1.MountBinding, owner string) ([]store.Mount, error) {
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
		// Containment guard: clients must not supply a gh: credential — that namespace is
		// CP-derived only (from the authenticated spawn owner). Reject early before any gate routing.
		if isGitHubMintLinkRef(binding.GetCredentialSecretId()) {
			return nil, fmt.Errorf("mount binding %q: credential_secret_id with the reserved %q prefix is not allowed (the github link is resolved by the control plane)", name, githubLinkSecretIDPrefix)
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
				Name:               name,
				BackendURI:         "github:" + cfg.Owner + "/" + cfg.Repo,
				CredentialSecretID: githubMintLinkSecretID(owner), // T3: CP-derived gh:<owner> mint link-ref (Approach 2)
				CreateIfMissing:    binding.GetCreateIfMissing(),
				RepositoryID:       strings.TrimSpace(binding.GetRepositoryId()),
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
