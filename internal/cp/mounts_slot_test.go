package cp

import (
	"strings"
	"testing"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/internal/cp/store"
)

func TestMergeCreateSpawnMounts_GitHubSlot(t *testing.T) {
	decls := []store.MountDecl{
		{Name: "repo", Github: true},
		{Name: "cache"},
	}

	t.Run("slot binding normalizes backend and leaves credential empty", func(t *testing.T) {
		out, err := mergeCreateSpawnMounts(decls, []*cpv1.MountBinding{{
			Name:            "repo",
			BackendUri:      "github:Owner/Repo.git",
			CreateIfMissing: true,
			RepositoryId:    "123",
		}})
		if err != nil {
			t.Fatalf("merge: %v", err)
		}
		byName := map[string]store.Mount{}
		for _, m := range out {
			byName[m.Name] = m
		}
		if got := byName["repo"]; got.BackendURI != "github:Owner/Repo" || got.CredentialSecretID != "" || !got.CreateIfMissing || got.RepositoryID != "123" {
			t.Fatalf("repo mount = %+v; want normalized github backend, empty credential", got)
		}
		if byName["cache"].BackendURI != "scratch" {
			t.Fatalf("cache backend = %q, want scratch", byName["cache"].BackendURI)
		}
	})

	t.Run("unbound github slot is rejected", func(t *testing.T) {
		_, err := mergeCreateSpawnMounts(decls, nil)
		if err == nil || !strings.Contains(err.Error(), "github mount slot") {
			t.Fatalf("want unbound-slot error, got %v", err)
		}
	})

	t.Run("github slot with non-github backend is rejected", func(t *testing.T) {
		_, err := mergeCreateSpawnMounts(decls, []*cpv1.MountBinding{{Name: "repo", BackendUri: "scratch:"}})
		if err == nil {
			t.Fatalf("want error binding scratch to a github slot")
		}
	})

	t.Run("github slot with malformed owner/repo is rejected", func(t *testing.T) {
		_, err := mergeCreateSpawnMounts(decls, []*cpv1.MountBinding{{Name: "repo", BackendUri: "github:bogus"}})
		if err == nil {
			t.Fatalf("want error for malformed github uri")
		}
	})

	t.Run("github backend on a non-slot mount is rejected", func(t *testing.T) {
		_, err := mergeCreateSpawnMounts([]store.MountDecl{{Name: "cache"}}, []*cpv1.MountBinding{{
			Name:       "cache",
			BackendUri: "github:owner/repo",
		}})
		if err == nil || !strings.Contains(err.Error(), "not a github slot") {
			t.Fatalf("want non-slot rejection, got %v", err)
		}
	})
}
