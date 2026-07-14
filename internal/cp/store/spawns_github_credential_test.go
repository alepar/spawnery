package store

// spawns_github_credential_test.go: tests for SetGitHubCredentialStatus (sp-2tx8.9.2, spec §4.1).
// The GitHub credential condition is persisted on the spawn row (unlike the ephemeral
// provisioning/skill-install progress) so RELINK_REQUIRED survives a CP restart.

import (
	"context"
	"errors"
	"testing"
)

// F1: the column defaults to unset and SetGitHubCredentialStatus round-trips via Get.
func TestGitHubCredentialStatusDefaultAndSet(t *testing.T) {
	st := NewTestStore(t)
	seedAppAndOwner(t, st)
	ctx := context.Background()
	inTx(t, st, func(tx Store) error { return tx.Spawns().Create(ctx, newSpawn("sp1"), nil) })

	// Default must be the empty string (NOT NULL DEFAULT '' from the migration): a spawn with no
	// GitHub mount never reports a condition at all.
	s, err := st.Spawns().Get(ctx, "sp1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if s.GitHubCredentialStatus != GitHubCredUnset {
		t.Fatalf("fresh spawn: github_credential_status = %q, want unset", s.GitHubCredentialStatus)
	}

	// Each value round-trips, and a later value overwrites the earlier one (self-healing: a
	// successful push reports OK and clears a prior STALE).
	for _, want := range []GitHubCredentialStatus{GitHubCredOK, GitHubCredStale, GitHubCredRelinkRequired, GitHubCredOK} {
		if err := st.Spawns().SetGitHubCredentialStatus(ctx, "sp1", want); err != nil {
			t.Fatalf("SetGitHubCredentialStatus(%q): %v", want, err)
		}
		s, err := st.Spawns().Get(ctx, "sp1")
		if err != nil {
			t.Fatalf("Get after set: %v", err)
		}
		if s.GitHubCredentialStatus != want {
			t.Fatalf("github_credential_status = %q, want %q", s.GitHubCredentialStatus, want)
		}
	}
}

// F2: SetGitHubCredentialStatus on a missing or deleted spawn returns ErrNotFound.
func TestGitHubCredentialStatusNotFound(t *testing.T) {
	st := NewTestStore(t)
	seedAppAndOwner(t, st)
	ctx := context.Background()

	if err := st.Spawns().SetGitHubCredentialStatus(ctx, "ghost", GitHubCredStale); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetGitHubCredentialStatus(missing): want ErrNotFound, got %v", err)
	}

	inTx(t, st, func(tx Store) error { return tx.Spawns().Create(ctx, newSpawn("sp1"), nil) })
	inTx(t, st, func(tx Store) error { return tx.Spawns().MarkDeleted(ctx, "sp1", 9) })
	if err := st.Spawns().SetGitHubCredentialStatus(ctx, "sp1", GitHubCredStale); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetGitHubCredentialStatus(deleted): want ErrNotFound, got %v", err)
	}
}
