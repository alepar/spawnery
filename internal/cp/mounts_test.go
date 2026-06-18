package cp

import (
	"testing"

	"spawnery/internal/cp/store"
)

func TestGitHubMintLinkSecretID(t *testing.T) {
	t.Parallel()
	if got := githubMintLinkSecretID("alice"); got != "gh:alice" {
		t.Fatalf("githubMintLinkSecretID(alice) = %q, want %q", got, "gh:alice")
	}
	if got := githubMintLinkSecretID("  bob  "); got != "gh:bob" {
		t.Fatalf("githubMintLinkSecretID(whitespace) = %q, want trimmed", got)
	}
}

func TestIsGitHubMintLinkRef(t *testing.T) {
	t.Parallel()
	cases := []struct {
		id   string
		want bool
	}{
		{"gh:alice", true},
		{"gh:x", true},
		{"gh:", true},
		{"", false},
		{"tok-secret", false},
		{"github:owner/repo", false},
		{"GH:alice", false}, // case-sensitive
	}
	for _, tc := range cases {
		if got := isGitHubMintLinkRef(tc.id); got != tc.want {
			t.Errorf("isGitHubMintLinkRef(%q) = %v, want %v", tc.id, got, tc.want)
		}
	}
}

func TestStoreToNodeMounts(t *testing.T) {
	t.Parallel()

	if got := storeToNodeMounts(nil); got != nil {
		t.Fatalf("storeToNodeMounts(nil) = %+v, want nil", got)
	}
	if got := storeToNodeMounts([]store.Mount{}); got != nil {
		t.Fatalf("storeToNodeMounts(empty) = %+v, want nil", got)
	}

	got := storeToNodeMounts([]store.Mount{{
		Name: "main", BackendURI: "github:owner/repo", CredentialSecretID: "gh-main", CreateIfMissing: true, RepositoryID: "123",
	}})
	if len(got) != 1 ||
		got[0].GetName() != "main" ||
		got[0].GetBackendUri() != "github:owner/repo" ||
		got[0].GetCredentialSecretId() != "gh-main" ||
		!got[0].GetCreateIfMissing() ||
		got[0].GetRepositoryId() != "123" {
		t.Fatalf("storeToNodeMounts = %+v", got)
	}
}

// TestStoreToNodeMountsEmitsGitHubMintRefForLinkRef verifies that a github: mount whose
// credential_secret_id is a CP-derived gh: link-ref gets a GithubMintRef populated on the wire
// form, while a non-gh credential does not.
func TestStoreToNodeMountsEmitsGitHubMintRefForLinkRef(t *testing.T) {
	t.Parallel()

	// gh: link-ref: should get GithubMintRef populated and credential_secret_id preserved.
	got := storeToNodeMounts([]store.Mount{{
		Name: "repo", BackendURI: "github:owner/repo", CredentialSecretID: "gh:alice",
		CreateIfMissing: true, RepositoryID: "123",
	}})
	if len(got) != 1 {
		t.Fatalf("want 1 mount, got %d", len(got))
	}
	mb := got[0]
	if mb.GetGithubMintRef() == nil {
		t.Fatal("GithubMintRef is nil for gh: link-ref mount; want non-nil")
	}
	if mb.GetGithubMintRef().GetSecretId() != "gh:alice" {
		t.Fatalf("GithubMintRef.SecretId = %q, want %q", mb.GetGithubMintRef().GetSecretId(), "gh:alice")
	}
	if mb.GetCredentialSecretId() != "gh:alice" {
		t.Fatalf("CredentialSecretId = %q, want preserved %q", mb.GetCredentialSecretId(), "gh:alice")
	}
	if mb.GetRepositoryId() != "123" {
		t.Fatalf("RepositoryId = %q, want %q", mb.GetRepositoryId(), "123")
	}

	// Owner-sealed (non-gh) credential: GithubMintRef must be nil.
	got2 := storeToNodeMounts([]store.Mount{{
		Name: "repo2", BackendURI: "github:owner/repo2", CredentialSecretID: "my-gh-token",
	}})
	if len(got2) != 1 {
		t.Fatalf("want 1 mount, got %d", len(got2))
	}
	if got2[0].GetGithubMintRef() != nil {
		t.Fatalf("GithubMintRef must be nil for owner-sealed credential, got %+v", got2[0].GetGithubMintRef())
	}
	if got2[0].GetCredentialSecretId() != "my-gh-token" {
		t.Fatalf("CredentialSecretId = %q, want %q", got2[0].GetCredentialSecretId(), "my-gh-token")
	}
}
