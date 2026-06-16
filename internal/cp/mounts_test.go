package cp

import (
	"testing"

	"spawnery/internal/cp/store"
)

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
