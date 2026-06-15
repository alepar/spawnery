package wirecheck

import (
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	authv1 "spawnery/gen/auth/v1"
	"spawnery/gen/auth/v1/authv1connect"
	cpv1 "spawnery/gen/cp/v1"
	nodev1 "spawnery/gen/node/v1"
)

func requireFieldNumber(t *testing.T, msg proto.Message, name protoreflect.Name, want protoreflect.FieldNumber) {
	t.Helper()
	fd := msg.ProtoReflect().Descriptor().Fields().ByName(name)
	if fd == nil {
		t.Fatalf("%T missing field %q", msg, name)
	}
	if got := fd.Number(); got != want {
		t.Fatalf("%T.%s field number=%d want %d", msg, name, got, want)
	}
}

func TestNodeGitHubSecretRoutingProtoSurface(t *testing.T) {
	if got := nodev1.SecretType_SECRET_TYPE_UNSPECIFIED.Number(); got != 0 {
		t.Fatalf("SECRET_TYPE_UNSPECIFIED=%d want 0", got)
	}
	if got := nodev1.SecretType_SECRET_TYPE_GITHUB_TOKEN.Number(); got != 1 {
		t.Fatalf("SECRET_TYPE_GITHUB_TOKEN=%d want 1", got)
	}
	if got := nodev1.SecretUsage_SECRET_USAGE_UNSPECIFIED.Number(); got != 0 {
		t.Fatalf("SECRET_USAGE_UNSPECIFIED=%d want 0", got)
	}
	if got := nodev1.SecretUsage_SECRET_USAGE_NODE_STORAGE.Number(); got != 1 {
		t.Fatalf("SECRET_USAGE_NODE_STORAGE=%d want 1", got)
	}
	if got := nodev1.SecretUsage_SECRET_USAGE_AGENT_RENDER.Number(); got != 2 {
		t.Fatalf("SECRET_USAGE_AGENT_RENDER=%d want 2", got)
	}

	requireFieldNumber(t, &nodev1.SealedSecret{}, "target_path", 1)
	requireFieldNumber(t, &nodev1.SealedSecret{}, "sealed", 2)
	requireFieldNumber(t, &nodev1.SealedSecret{}, "secret_id", 3)
	requireFieldNumber(t, &nodev1.SealedSecret{}, "type", 4)
	requireFieldNumber(t, &nodev1.SealedSecret{}, "version", 5)
	requireFieldNumber(t, &nodev1.SealedSecret{}, "delivery_id", 6)
	requireFieldNumber(t, &nodev1.SealedSecret{}, "usages", 7)
	requireFieldNumber(t, &nodev1.SealedSecret{}, "mount_names", 8)
	requireFieldNumber(t, &nodev1.SealedSecret{}, "render", 9)
	requireFieldNumber(t, &nodev1.SealedSecret{}, "github_token", 10)
	requireFieldNumber(t, &nodev1.StartSpawn{}, "secrets", 25)
	requireFieldNumber(t, &nodev1.SecretRenderSpec{}, "profile", 1)
	requireFieldNumber(t, &nodev1.SecretRenderSpec{}, "target_path", 2)
	requireFieldNumber(t, &nodev1.SecretRenderSpec{}, "gh_config_dir", 3)
	requireFieldNumber(t, &nodev1.SecretRenderSpec{}, "hosts_path", 4)
	requireFieldNumber(t, &nodev1.SecretRenderSpec{}, "git_config_path", 5)
	requireFieldNumber(t, &nodev1.SecretRenderSpec{}, "credential_helper_path", 6)
	requireFieldNumber(t, &nodev1.GitHubTokenClearMetadata{}, "host", 1)
	requireFieldNumber(t, &nodev1.GitHubTokenClearMetadata{}, "login", 2)
	requireFieldNumber(t, &nodev1.GitHubTokenClearMetadata{}, "github_user_id", 3)
	requireFieldNumber(t, &nodev1.GitHubTokenClearMetadata{}, "refresh_expires_at_unix", 4)
	requireFieldNumber(t, &nodev1.GitHubTokenClearMetadata{}, "app_client_id", 5)

	secret := &nodev1.SealedSecret{
		TargetPath: "github/workspace/legacy-target",
		Sealed:     []byte("node-sealed-refresh-tuple"),
		SecretId:   "gh-main",
		Type:       nodev1.SecretType_SECRET_TYPE_GITHUB_TOKEN,
		Version:    11,
		DeliveryId: "delivery-sp1-gen3-gh-main-v11",
		Usages: []nodev1.SecretUsage{
			nodev1.SecretUsage_SECRET_USAGE_NODE_STORAGE,
			nodev1.SecretUsage_SECRET_USAGE_AGENT_RENDER,
		},
		MountNames: []string{"workspace"},
		Render: &nodev1.SecretRenderSpec{
			Profile:              "gh-cli-v1",
			TargetPath:           "github/workspace",
			GhConfigDir:          "github/workspace/gh",
			HostsPath:            "github/workspace/gh/hosts.yml",
			GitConfigPath:        "github/workspace/gitconfig",
			CredentialHelperPath: "github/workspace/git-credential-spawnery",
		},
		GithubToken: &nodev1.GitHubTokenClearMetadata{
			Host:                 "github.com",
			Login:                "alice",
			GithubUserId:         "123456",
			RefreshExpiresAtUnix: 1893456000,
			AppClientId:          "Iv1.spawnerytest",
		},
	}
	start := &nodev1.StartSpawn{SpawnId: "sp1", Generation: 3, Secrets: []*nodev1.SealedSecret{secret}}

	b, err := proto.Marshal(start)
	if err != nil {
		t.Fatalf("marshal StartSpawn: %v", err)
	}
	var got nodev1.StartSpawn
	if err := proto.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal StartSpawn: %v", err)
	}
	if len(got.GetSecrets()) != 1 {
		t.Fatalf("StartSpawn.Secrets len=%d want 1", len(got.GetSecrets()))
	}
	gotSecret := got.GetSecrets()[0]
	if gotSecret.GetSecretId() != "gh-main" || gotSecret.GetVersion() != 11 || gotSecret.GetDeliveryId() != "delivery-sp1-gen3-gh-main-v11" {
		t.Fatalf("secret identity lost on round-trip: %+v", gotSecret)
	}
	if gotSecret.GetType() != nodev1.SecretType_SECRET_TYPE_GITHUB_TOKEN {
		t.Fatalf("secret type=%v want github-token", gotSecret.GetType())
	}
	if len(gotSecret.GetUsages()) != 2 || gotSecret.GetUsages()[0] != nodev1.SecretUsage_SECRET_USAGE_NODE_STORAGE || gotSecret.GetUsages()[1] != nodev1.SecretUsage_SECRET_USAGE_AGENT_RENDER {
		t.Fatalf("secret usages lost on round-trip: %+v", gotSecret.GetUsages())
	}
	if len(gotSecret.GetMountNames()) != 1 || gotSecret.GetMountNames()[0] != "workspace" {
		t.Fatalf("mount names lost on round-trip: %+v", gotSecret.GetMountNames())
	}
	if gotSecret.GetRender().GetGhConfigDir() != "github/workspace/gh" || gotSecret.GetRender().GetHostsPath() != "github/workspace/gh/hosts.yml" {
		t.Fatalf("render routing lost on round-trip: %+v", gotSecret.GetRender())
	}
	if gotSecret.GetGithubToken().GetHost() != "github.com" || gotSecret.GetGithubToken().GetGithubUserId() != "123456" {
		t.Fatalf("github clear metadata lost on round-trip: %+v", gotSecret.GetGithubToken())
	}
}

func TestCPGitHubSecretRoutingProtoSurface(t *testing.T) {
	if got := cpv1.SecretType_SECRET_TYPE_UNSPECIFIED.Number(); got != 0 {
		t.Fatalf("cp SECRET_TYPE_UNSPECIFIED=%d want 0", got)
	}
	if got := cpv1.SecretType_SECRET_TYPE_GITHUB_TOKEN.Number(); got != 1 {
		t.Fatalf("cp SECRET_TYPE_GITHUB_TOKEN=%d want 1", got)
	}
	if got := cpv1.SecretUsage_SECRET_USAGE_UNSPECIFIED.Number(); got != 0 {
		t.Fatalf("cp SECRET_USAGE_UNSPECIFIED=%d want 0", got)
	}
	if got := cpv1.SecretUsage_SECRET_USAGE_NODE_STORAGE.Number(); got != 1 {
		t.Fatalf("cp SECRET_USAGE_NODE_STORAGE=%d want 1", got)
	}
	if got := cpv1.SecretUsage_SECRET_USAGE_AGENT_RENDER.Number(); got != 2 {
		t.Fatalf("cp SECRET_USAGE_AGENT_RENDER=%d want 2", got)
	}

	requireFieldNumber(t, &cpv1.SealedSecret{}, "target_path", 1)
	requireFieldNumber(t, &cpv1.SealedSecret{}, "sealed", 2)
	requireFieldNumber(t, &cpv1.SealedSecret{}, "secret_id", 3)
	requireFieldNumber(t, &cpv1.SealedSecret{}, "type", 4)
	requireFieldNumber(t, &cpv1.SealedSecret{}, "version", 5)
	requireFieldNumber(t, &cpv1.SealedSecret{}, "delivery_id", 6)
	requireFieldNumber(t, &cpv1.SealedSecret{}, "usages", 7)
	requireFieldNumber(t, &cpv1.SealedSecret{}, "mount_names", 8)
	requireFieldNumber(t, &cpv1.SealedSecret{}, "render", 9)
	requireFieldNumber(t, &cpv1.SealedSecret{}, "github_token", 10)
	requireFieldNumber(t, &cpv1.SecretRenderSpec{}, "profile", 1)
	requireFieldNumber(t, &cpv1.SecretRenderSpec{}, "target_path", 2)
	requireFieldNumber(t, &cpv1.SecretRenderSpec{}, "gh_config_dir", 3)
	requireFieldNumber(t, &cpv1.SecretRenderSpec{}, "hosts_path", 4)
	requireFieldNumber(t, &cpv1.SecretRenderSpec{}, "git_config_path", 5)
	requireFieldNumber(t, &cpv1.SecretRenderSpec{}, "credential_helper_path", 6)
	requireFieldNumber(t, &cpv1.GitHubTokenClearMetadata{}, "host", 1)
	requireFieldNumber(t, &cpv1.GitHubTokenClearMetadata{}, "login", 2)
	requireFieldNumber(t, &cpv1.GitHubTokenClearMetadata{}, "github_user_id", 3)
	requireFieldNumber(t, &cpv1.GitHubTokenClearMetadata{}, "refresh_expires_at_unix", 4)
	requireFieldNumber(t, &cpv1.GitHubTokenClearMetadata{}, "app_client_id", 5)
	requireFieldNumber(t, &cpv1.SubmitIntentRequest{}, "secrets", 4)

	secret := &cpv1.SealedSecret{
		TargetPath: "github/workspace/legacy-target",
		Sealed:     []byte("cp-sealed-refresh-tuple"),
		SecretId:   "gh-main",
		Type:       cpv1.SecretType_SECRET_TYPE_GITHUB_TOKEN,
		Version:    11,
		DeliveryId: "delivery-sp1-gen3-gh-main-v11",
		Usages: []cpv1.SecretUsage{
			cpv1.SecretUsage_SECRET_USAGE_NODE_STORAGE,
			cpv1.SecretUsage_SECRET_USAGE_AGENT_RENDER,
		},
		MountNames: []string{"workspace"},
		Render: &cpv1.SecretRenderSpec{
			Profile:              "gh-cli-v1",
			TargetPath:           "github/workspace",
			GhConfigDir:          "github/workspace/gh",
			HostsPath:            "github/workspace/gh/hosts.yml",
			GitConfigPath:        "github/workspace/gitconfig",
			CredentialHelperPath: "github/workspace/git-credential-spawnery",
		},
		GithubToken: &cpv1.GitHubTokenClearMetadata{
			Host:                 "github.com",
			Login:                "alice",
			GithubUserId:         "123456",
			RefreshExpiresAtUnix: 1893456000,
			AppClientId:          "Iv1.spawnerytest",
		},
	}

	in := &cpv1.DeliverSecretsRequest{
		SpawnId: "sp1",
		Secrets: []*cpv1.SealedSecret{secret},
	}

	b, err := proto.Marshal(in)
	if err != nil {
		t.Fatalf("marshal DeliverSecretsRequest: %v", err)
	}
	var got cpv1.DeliverSecretsRequest
	if err := proto.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal DeliverSecretsRequest: %v", err)
	}
	if len(got.GetSecrets()) != 1 || got.GetSecrets()[0].GetGithubToken().GetHost() != "github.com" {
		t.Fatalf("cp secret routing lost on round-trip: %+v", got.GetSecrets())
	}

	submit := &cpv1.SubmitIntentRequest{
		SpawnId:         "sp1",
		NodeAccessToken: "node-token",
		Secrets:         []*cpv1.SealedSecret{secret},
	}
	submitBytes, err := proto.Marshal(submit)
	if err != nil {
		t.Fatalf("marshal SubmitIntentRequest: %v", err)
	}
	var gotSubmit cpv1.SubmitIntentRequest
	if err := proto.Unmarshal(submitBytes, &gotSubmit); err != nil {
		t.Fatalf("unmarshal SubmitIntentRequest: %v", err)
	}
	if len(gotSubmit.GetSecrets()) != 1 {
		t.Fatalf("SubmitIntentRequest.Secrets len=%d want 1", len(gotSubmit.GetSecrets()))
	}
	gotSecret := gotSubmit.GetSecrets()[0]
	if gotSecret.GetSecretId() != "gh-main" || gotSecret.GetVersion() != 11 || gotSecret.GetDeliveryId() != "delivery-sp1-gen3-gh-main-v11" {
		t.Fatalf("SubmitIntent secret identity lost on round-trip: %+v", gotSecret)
	}
	if gotSecret.GetType() != cpv1.SecretType_SECRET_TYPE_GITHUB_TOKEN {
		t.Fatalf("SubmitIntent secret type=%v want github-token", gotSecret.GetType())
	}
	if len(gotSecret.GetUsages()) != 2 || gotSecret.GetUsages()[0] != cpv1.SecretUsage_SECRET_USAGE_NODE_STORAGE || gotSecret.GetUsages()[1] != cpv1.SecretUsage_SECRET_USAGE_AGENT_RENDER {
		t.Fatalf("SubmitIntent secret usages lost on round-trip: %+v", gotSecret.GetUsages())
	}
	if len(gotSecret.GetMountNames()) != 1 || gotSecret.GetMountNames()[0] != "workspace" {
		t.Fatalf("SubmitIntent mount names lost on round-trip: %+v", gotSecret.GetMountNames())
	}
	if gotSecret.GetRender().GetCredentialHelperPath() != "github/workspace/git-credential-spawnery" {
		t.Fatalf("SubmitIntent render routing lost on round-trip: %+v", gotSecret.GetRender())
	}
	if gotSecret.GetGithubToken().GetLogin() != "alice" || gotSecret.GetGithubToken().GetAppClientId() != "Iv1.spawnerytest" {
		t.Fatalf("SubmitIntent github clear metadata lost on round-trip: %+v", gotSecret.GetGithubToken())
	}
}

func TestAuthGitHubMintProtoSurface(t *testing.T) {
	if got, want := authv1connect.AuthServiceMintGitHubAccessTokenProcedure, "/auth.v1.AuthService/MintGitHubAccessToken"; got != want {
		t.Fatalf("mint procedure=%q want %q", got, want)
	}

	requireFieldNumber(t, &authv1.GitHubRefreshTokenRef{}, "secret_id", 1)
	requireFieldNumber(t, &authv1.GitHubRefreshTokenRef{}, "version", 2)
	requireFieldNumber(t, &authv1.GitHubRefreshTokenRef{}, "delivery_id", 3)
	requireFieldNumber(t, &authv1.GitHubRefreshTokenRef{}, "refresh_token", 4)
	requireFieldNumber(t, &authv1.MintGitHubAccessTokenRequest{}, "request_id", 1)
	requireFieldNumber(t, &authv1.MintGitHubAccessTokenRequest{}, "spawn_id", 2)
	requireFieldNumber(t, &authv1.MintGitHubAccessTokenRequest{}, "generation", 3)
	requireFieldNumber(t, &authv1.MintGitHubAccessTokenRequest{}, "refresh_token_ref", 4)
	requireFieldNumber(t, &authv1.MintGitHubAccessTokenRequest{}, "repository_id", 5)
	requireFieldNumber(t, &authv1.MintGitHubAccessTokenResponse{}, "request_id", 1)
	requireFieldNumber(t, &authv1.MintGitHubAccessTokenResponse{}, "access_token", 2)
	requireFieldNumber(t, &authv1.MintGitHubAccessTokenResponse{}, "access_expires_at_unix", 3)
	requireFieldNumber(t, &authv1.MintGitHubAccessTokenResponse{}, "refresh_token", 4)
	requireFieldNumber(t, &authv1.MintGitHubAccessTokenResponse{}, "refresh_expires_at_unix", 5)
	requireFieldNumber(t, &authv1.MintGitHubAccessTokenResponse{}, "token_type", 6)
	requireFieldNumber(t, &authv1.MintGitHubAccessTokenResponse{}, "repository_id", 7)

	req := &authv1.MintGitHubAccessTokenRequest{
		RequestId:    "mint-sp1-gen3-gh-main-delivery-sp1-gen3-gh-main-v11-repo987654321",
		SpawnId:      "sp1",
		Generation:   3,
		RepositoryId: "987654321",
		RefreshTokenRef: &authv1.GitHubRefreshTokenRef{
			SecretId:     "gh-main",
			Version:      11,
			DeliveryId:   "delivery-sp1-gen3-gh-main-v11",
			RefreshToken: "github-refresh-token-before-rotation",
		},
	}
	resp := &authv1.MintGitHubAccessTokenResponse{
		RequestId:            req.GetRequestId(),
		AccessToken:          "github-access-token-after-rotation",
		AccessExpiresAtUnix:  1890000000,
		RefreshToken:         "github-refresh-token-after-rotation",
		RefreshExpiresAtUnix: 1900000000,
		TokenType:            "bearer",
		RepositoryId:         req.GetRepositoryId(),
	}

	reqBytes, err := proto.Marshal(req)
	if err != nil {
		t.Fatalf("marshal mint request: %v", err)
	}
	var gotReq authv1.MintGitHubAccessTokenRequest
	if err := proto.Unmarshal(reqBytes, &gotReq); err != nil {
		t.Fatalf("unmarshal mint request: %v", err)
	}
	if gotReq.GetRefreshTokenRef().GetSecretId() != "gh-main" || gotReq.GetRepositoryId() != "987654321" || gotReq.GetRequestId() == "" {
		t.Fatalf("mint request lost idempotency/routing fields: %+v", &gotReq)
	}

	respBytes, err := proto.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal mint response: %v", err)
	}
	var gotResp authv1.MintGitHubAccessTokenResponse
	if err := proto.Unmarshal(respBytes, &gotResp); err != nil {
		t.Fatalf("unmarshal mint response: %v", err)
	}
	if gotResp.GetRefreshToken() != "github-refresh-token-after-rotation" || gotResp.GetRepositoryId() != "987654321" || gotResp.GetRequestId() != req.GetRequestId() {
		t.Fatalf("mint response lost rotated tuple/idempotency fields: %+v", &gotResp)
	}
}
