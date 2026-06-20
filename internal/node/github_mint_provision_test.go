package node

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	authv1 "spawnery/gen/auth/v1"
	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/spawnlet"
	"spawnery/internal/storage"
)

// happy path: node mints (initial link-ref: secret_id only), renders into the NODE-ONLY cred root,
// passes repository_id as audit only, Notes the link for the refresher, and seeds the git-env gitconfig
// with the linked identity (sp-m859.1 §1.2).
func TestMintGitHubMountAtProvision_RendersNodeTokenAndNotes(t *testing.T) {
	const nodeID, spawnID, gen = "node-1", "sp1", uint64(5)
	a, _, secretsRoot := secretTestRig(t, nodeID, spawnID, gen)
	fake := &fakeMintClient{resp: &authv1.MintGitHubAccessTokenResponse{
		AccessToken:         "ghu_minted_token",
		AccessExpiresAtUnix: time.Now().Add(8 * time.Hour).Unix(),
		Login:               "octocat",
		UserId:              583231,
	}}
	a.githubRefresh = newGitHubRefresher(fake)

	mounts := []*nodev1.MountBinding{{
		Name:          "main",
		BackendUri:    "github:octo/demo",
		RepositoryId:  "42",
		GithubMintRef: &nodev1.GitHubMintRef{SecretId: "gh:octo"},
	}}
	if err := a.mintGitHubMountsAtProvision(context.Background(), spawnID, gen, mounts); err != nil {
		t.Fatalf("mintGitHubMountsAtProvision: %v", err)
	}

	// 1) initial link-ref shape: secret_id only, no version/delivery_id; audit repository_id present.
	calls := fake.calls()
	if len(calls) != 1 {
		t.Fatalf("mint calls = %d, want 1", len(calls))
	}
	req := calls[0]
	if req.GetLinkRef().GetSecretId() != "gh:octo" || req.GetLinkRef().GetVersion() != 0 || req.GetLinkRef().GetDeliveryId() != "" {
		t.Fatalf("link_ref = %+v, want secret_id=gh:octo only (initial)", req.GetLinkRef())
	}
	if req.GetSpawnId() != spawnID || req.GetGeneration() != gen || req.GetRepositoryId() != "42" {
		t.Fatalf("mint req mismatch: %+v", req)
	}

	// 2) token rendered into the NODE cred root (read back via the storage provider seam).
	cred, err := a.mgr.TokenForGitHubMount(context.Background(), spawnID, "main", storage.GitHubConfig{})
	if err != nil {
		t.Fatalf("TokenForGitHubMount: %v", err)
	}
	tok, err := cred.Token()
	if err != nil || tok != "ghu_minted_token" {
		t.Fatalf("rendered token = %q, err=%v", tok, err)
	}

	// 3) CONTAINMENT (b): token must NOT appear in the agent-bind-mounted secrets root.
	if _, err := os.Stat(filepath.Join(secretsRoot, spawnID, "github", "token")); !os.IsNotExist(err) {
		t.Fatalf("node mint token must NOT land in the agent secrets dir, err=%v", err)
	}

	// 4) link Noted for the proactive refresher (same-package access to states).
	a.githubRefresh.mu.Lock()
	noteState := a.githubRefresh.states[spawnID]
	a.githubRefresh.mu.Unlock()
	if noteState == nil || noteState["gh:octo"] == nil {
		t.Fatalf("expected refresher to have Noted gh:octo for %s", spawnID)
	}

	// 5) IDENTITY (sp-m859.1 §1.2): git-env gitconfig seeded from the mint's Login/UserId.
	dataRoot := filepath.Dir(secretsRoot)
	gitconfigPath := filepath.Join(dataRoot, "git-env", spawnID, spawnlet.GitConfigName)
	gcBytes, err := os.ReadFile(gitconfigPath)
	if err != nil {
		t.Fatalf("read git-env gitconfig: %v", err)
	}
	if !strings.Contains(string(gcBytes), "583231+octocat@users.noreply.github.com") {
		t.Errorf("git-env gitconfig missing canonical email: %q", string(gcBytes))
	}
}

// no descriptor => no mint, no render, no Note.
func TestMintGitHubMountAtProvision_NoDescriptorIsNoop(t *testing.T) {
	const nodeID, spawnID, gen = "node-1", "sp1", uint64(5)
	a, _, _ := secretTestRig(t, nodeID, spawnID, gen)
	fake := &fakeMintClient{resp: &authv1.MintGitHubAccessTokenResponse{}}
	a.githubRefresh = newGitHubRefresher(fake)

	mounts := []*nodev1.MountBinding{{Name: "main", BackendUri: "github:octo/demo"}} // no GithubMintRef
	if err := a.mintGitHubMountsAtProvision(context.Background(), spawnID, gen, mounts); err != nil {
		t.Fatalf("noop path errored: %v", err)
	}
	if len(fake.calls()) != 0 {
		t.Fatalf("mint must not be called without a descriptor; got %d", len(fake.calls()))
	}
}

// owner has no link (AS FailedPrecondition: relink_required) => actionable "link your GitHub" error,
// and NO token is written anywhere.
func TestMintGitHubMountAtProvision_RelinkRequiredIsActionable(t *testing.T) {
	const nodeID, spawnID, gen = "node-1", "sp1", uint64(5)
	a, _, secretsRoot := secretTestRig(t, nodeID, spawnID, gen)
	fake := &fakeMintClient{err: connect.NewError(connect.CodeFailedPrecondition,
		errors.New("github link gh:octo: relink_required: refresh chain is broken"))}
	a.githubRefresh = newGitHubRefresher(fake)

	mounts := []*nodev1.MountBinding{{
		Name: "main", BackendUri: "github:octo/demo",
		GithubMintRef: &nodev1.GitHubMintRef{SecretId: "gh:octo"},
	}}
	err := a.mintGitHubMountsAtProvision(context.Background(), spawnID, gen, mounts)
	if err == nil {
		t.Fatal("expected an error when the link is missing")
	}
	if !strings.Contains(err.Error(), "link your GitHub") {
		t.Fatalf("error must be actionable: %v", err)
	}
	// no partial token rendered (node OR agent root).
	if _, statErr := a.mgr.TokenForGitHubMount(context.Background(), spawnID, "main", storage.GitHubConfig{}); statErr == nil {
		t.Fatal("no token should have been rendered on the failure path")
	}
	if _, statErr := os.Stat(filepath.Join(secretsRoot, spawnID, "github", "token")); !os.IsNotExist(statErr) {
		t.Fatalf("no token in agent secrets dir on failure, err=%v", statErr)
	}
}

// dev lane without a mint channel (nil client) => explicit error, never a silent no-op delivering nothing.
func TestMintGitHubMountAtProvision_NoMintChannelErrors(t *testing.T) {
	const nodeID, spawnID, gen = "node-1", "sp1", uint64(5)
	a, _, _ := secretTestRig(t, nodeID, spawnID, gen)
	a.githubRefresh = newGitHubRefresher(nil) // no node->AS mint client

	mounts := []*nodev1.MountBinding{{
		Name: "main", BackendUri: "github:octo/demo",
		GithubMintRef: &nodev1.GitHubMintRef{SecretId: "gh:octo"},
	}}
	if err := a.mintGitHubMountsAtProvision(context.Background(), spawnID, gen, mounts); err == nil {
		t.Fatal("expected an error when the mint channel is unconfigured")
	}
}
