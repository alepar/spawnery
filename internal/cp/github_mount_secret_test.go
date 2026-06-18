package cp

// github_mount_secret_test.go: hermetic tests for github: mount credential type validation.
//
// Coverage:
//   - TestStartupSecretIDsForSpawnIncludesGithubMountCredentials — pure function test that
//     startupSecretIDsForSpawn includes github: mount CredentialSecretIDs in the required set.
//   - TestValidateGitHubMountCredentialType — table-driven unit test of the helper against all
//     cases (valid, wrong type, missing, non-github, empty id).
//   - TestIntentThreadedCreateFailsClosedOnWrongTypedGithubMountCredential — intent-flow
//     integration test: create fails closed (spawn → Error, no StartSpawn) when the referenced
//     credential secret exists but is the wrong type.

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/internal/cp/auth"
	"spawnery/internal/cp/store"
)

// TestStartupSecretIDsForSpawnIncludesGithubMountCredentials pins that startupSecretIDsForSpawn
// includes github: mount CredentialSecretIDs in the required startup-secret set (sorted, deduped).
func TestStartupSecretIDsForSpawnIncludesGithubMountCredentials(t *testing.T) {
	tests := []struct {
		name   string
		arts   []store.Artifact
		mounts []store.Mount
		want   []string
	}{
		{
			name: "github mount contributes credential",
			mounts: []store.Mount{
				{BackendURI: "github:owner/repo", CredentialSecretID: "gh-main"},
			},
			want: []string{"gh-main"},
		},
		{
			name: "non-github (scratch) mount contributes nothing",
			mounts: []store.Mount{
				{BackendURI: "scratch:", CredentialSecretID: "ignored"},
			},
			want: nil,
		},
		{
			name: "github mount with empty credential contributes nothing",
			mounts: []store.Mount{
				{BackendURI: "github:owner/repo", CredentialSecretID: ""},
			},
			want: nil,
		},
		{
			name: "github cred id deduped when it matches a sensitive artifact env-var name",
			arts: []store.Artifact{
				{Sensitive: true, EnvVarName: "gh-main"},
			},
			mounts: []store.Mount{
				{BackendURI: "github:owner/repo", CredentialSecretID: "gh-main"},
			},
			want: []string{"gh-main"},
		},
		{
			name: "output is sorted",
			mounts: []store.Mount{
				{BackendURI: "github:owner/repo1", CredentialSecretID: "z-cred"},
				{BackendURI: "github:owner/repo2", CredentialSecretID: "a-cred"},
			},
			want: []string{"a-cred", "z-cred"},
		},
		{
			// T3: a gh: link-ref is minted at provision by the node; it is not a catalog secret,
			// so it must NOT appear in the required startup-secret set.
			name: "gh: link-ref is NOT a required startup secret",
			mounts: []store.Mount{
				{BackendURI: "github:owner/repo", CredentialSecretID: "gh:alice"},
			},
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := startupSecretIDsForSpawn(tt.arts, tt.mounts)
			if len(got) == 0 && len(tt.want) == 0 {
				return // both empty
			}
			if !equalStringSlices(got, tt.want) {
				t.Fatalf("startupSecretIDsForSpawn = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// makeGitHubMountSecret seeds a catalog secret of the given type for use in github: mount tests.
// inference-key requires a non-empty Provider; other types must leave it empty.
func makeGitHubMountSecret(t *testing.T, s *Server, owner, id string, typ cpv1.UserSecretType) {
	t.Helper()
	provider := ""
	if typ == cpv1.UserSecretType_USER_SECRET_TYPE_INFERENCE_KEY {
		provider = "openrouter"
	}
	_, err := s.CreateSecret(auth.WithOwner(context.Background(), owner), connect.NewRequest(&cpv1.CreateSecretRequest{
		Secret: &cpv1.SecretWrite{
			SecretId:        id,
			Type:            typ,
			Name:            "Test secret " + id,
			TargetContainer: cpv1.ArtifactTarget_ARTIFACT_TARGET_AGENT,
			EnvVarName:      id,
			Provider:        provider,
			Envelope:        envelopeBytes(t, owner, id, 1),
		},
	}))
	if err != nil {
		t.Fatalf("CreateSecret(%s type=%v): %v", id, typ, err)
	}
}

// TestValidateGitHubMountCredentialType covers all branches of validateGitHubMountCredentialType:
// valid, wrong-typed, missing, non-github scheme, and empty credential id.
func TestValidateGitHubMountCredentialType(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name        string
		mounts      []store.Mount
		seedFn      func(t *testing.T, s *Server)
		wantNilErr  bool
		wantErrCode connect.Code
	}{
		{
			name:   "valid github-token passes",
			mounts: []store.Mount{{Name: "main", BackendURI: "github:o/r", CredentialSecretID: "gh1"}},
			seedFn: func(t *testing.T, s *Server) {
				makeGitHubMountSecret(t, s, "alice", "gh1", cpv1.UserSecretType_USER_SECRET_TYPE_GITHUB_TOKEN)
			},
			wantNilErr: true,
		},
		{
			name:   "wrong type generic-kv fails with FailedPrecondition",
			mounts: []store.Mount{{Name: "main", BackendURI: "github:o/r", CredentialSecretID: "gh1"}},
			seedFn: func(t *testing.T, s *Server) {
				makeGitHubMountSecret(t, s, "alice", "gh1", cpv1.UserSecretType_USER_SECRET_TYPE_GENERIC_KV)
			},
			wantErrCode: connect.CodeFailedPrecondition,
		},
		{
			name:   "wrong type inference-key fails with FailedPrecondition",
			mounts: []store.Mount{{Name: "main", BackendURI: "github:o/r", CredentialSecretID: "gh1"}},
			seedFn: func(t *testing.T, s *Server) {
				makeGitHubMountSecret(t, s, "alice", "gh1", cpv1.UserSecretType_USER_SECRET_TYPE_INFERENCE_KEY)
			},
			wantErrCode: connect.CodeFailedPrecondition,
		},
		{
			name:        "missing secret fails with NotFound",
			mounts:      []store.Mount{{Name: "main", BackendURI: "github:o/r", CredentialSecretID: "ghX"}},
			seedFn:      func(t *testing.T, s *Server) {},
			wantErrCode: connect.CodeNotFound,
		},
		{
			name:   "non-github mount is ignored",
			mounts: []store.Mount{{Name: "main", BackendURI: "scratch:", CredentialSecretID: "gh1"}},
			seedFn: func(t *testing.T, s *Server) {
				makeGitHubMountSecret(t, s, "alice", "gh1", cpv1.UserSecretType_USER_SECRET_TYPE_GENERIC_KV)
			},
			wantNilErr: true,
		},
		{
			name:       "empty credential id is ignored",
			mounts:     []store.Mount{{Name: "main", BackendURI: "github:o/r", CredentialSecretID: ""}},
			seedFn:     func(t *testing.T, s *Server) {},
			wantNilErr: true,
		},
		{
			// T3: gh: link-refs are CP-derived and have no catalog row — they must be routed past the
			// credential-type gate. A missing catalog entry must NOT cause NotFound here.
			name:       "gh: link-ref bypasses catalog type gate",
			mounts:     []store.Mount{{Name: "repo", BackendURI: "github:o/r", CredentialSecretID: "gh:alice"}},
			seedFn:     func(t *testing.T, s *Server) {},
			wantNilErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _, _ := newTestServer(t)
			tt.seedFn(t, s)
			err := s.validateGitHubMountCredentialType(ctx, "alice", tt.mounts)
			if tt.wantNilErr {
				if err != nil {
					t.Fatalf("want nil err, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("want error code %v, got nil", tt.wantErrCode)
			}
			if got := connect.CodeOf(err); got != tt.wantErrCode {
				t.Fatalf("error code = %v, want %v (err=%v)", got, tt.wantErrCode, err)
			}
		})
	}
}

// TestIntentThreadedCreateFailsClosedOnWrongTypedGithubMountCredential verifies that the
// secrets-ready gate blocks StartAgent when a github: mount's credential_secret_id references a
// secret that exists but is not a github-token. The spawn must reach Error before StartSpawn is
// ever sent — proving fail-closed behavior at the type-validation gate.
func TestIntentThreadedCreateFailsClosedOnWrongTypedGithubMountCredential(t *testing.T) {
	s, sender, stopACK := intentTestServer(t)
	defer stopACK()

	// Register an app with a declared "main" mount.
	seedCreateSpawnMountApp(t, s, "gh-app", "main")

	// Seed a GENERIC_KV secret (wrong type — github: mounts require github-token).
	createIntentCatalogSecret(t, s, "alice", "gh-cred",
		cpv1.ArtifactTarget_ARTIFACT_TARGET_AGENT, "github/token", "GH_TOKEN")

	ctx := auth.WithOwner(context.Background(), "alice")
	resp, err := s.CreateSpawn(ctx, connect.NewRequest(&cpv1.CreateSpawnRequest{
		AppId: "gh-app",
		Model: "m",
		Mounts: []*cpv1.MountBinding{{
			Name:               "main",
			BackendUri:         "github:owner/repo",
			CredentialSecretId: "gh-cred",
			RepositoryId:       "123",
		}},
	}))
	if err != nil {
		t.Fatalf("CreateSpawn: %v", err)
	}
	spawnID := resp.Msg.GetSpawnId()

	// provisionSpawn must fail closed before issuing StartSpawn.
	waitIntentSpawnStatus(t, s, spawnID, store.Errored)
	assertNoStartSpawn(t, sender, spawnID)
}
