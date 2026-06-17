package cp

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"testing"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/authsvc"
	"spawnery/internal/cp/auth"
	"spawnery/internal/cp/registry"
)

func TestAuthorizeGitHubMintRequiresIndexedHostedNode(t *testing.T) {
	s, _, _ := newTestServer(t)
	createActiveSpawn(t, s, "alice", "sp1", "node-1")
	s.githubLinks.note("sp1", "gh-main")

	err := s.authorizeGitHubMint(context.Background(), authsvc.GitHubMintAuthorization{
		NodeID: "node-1", SpawnID: "sp1", Generation: 1, SecretID: "gh-main",
	})
	if err != nil {
		t.Fatalf("authorize hosted indexed node: %v", err)
	}
	err = s.authorizeGitHubMint(context.Background(), authsvc.GitHubMintAuthorization{
		NodeID: "node-2", SpawnID: "sp1", Generation: 1, SecretID: "gh-main",
	})
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("authorize wrong node code=%v err=%v", connect.CodeOf(err), err)
	}
	err = s.authorizeGitHubMint(context.Background(), authsvc.GitHubMintAuthorization{
		NodeID: "node-1", SpawnID: "sp1", Generation: 1, SecretID: "not-indexed",
	})
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("authorize unindexed link code=%v err=%v", connect.CodeOf(err), err)
	}
}

func TestAuthorizeGitHubMintRPCConfirmsIndexedHostedNode(t *testing.T) {
	s, _, _ := newTestServer(t)
	createActiveSpawn(t, s, "alice", "sp1", "node-1")
	s.githubLinks.note("sp1", "gh-main")

	_, err := s.AuthorizeGitHubMint(context.Background(), connect.NewRequest(&cpv1.AuthorizeGitHubMintRequest{
		NodeId: "node-1", SpawnId: "sp1", Generation: 1, SecretId: "gh-main", Version: 12, DeliveryId: "d1",
	}))
	if err != nil {
		t.Fatalf("AuthorizeGitHubMint RPC: %v", err)
	}
}

func TestGitHubLinkTargetsReturnPublishedNodeKeys(t *testing.T) {
	s, reg, _ := newTestServer(t)
	reg.Add(&registry.Node{ID: "node-1", Sender: &capSender{}})
	createActiveSpawn(t, s, "alice", "sp1", "node-1")
	s.githubLinks.noteCPSecrets("sp1", []*cpv1.SealedSecret{{
		SecretId:   "gh-main",
		Sealed:     []byte("old-ciphertext"),
		Type:       cpv1.SecretType_SECRET_TYPE_GITHUB_TOKEN,
		Version:    11,
		DeliveryId: "delivery-old",
		Usages: []cpv1.SecretUsage{
			cpv1.SecretUsage_SECRET_USAGE_NODE_STORAGE,
			cpv1.SecretUsage_SECRET_USAGE_AGENT_RENDER,
		},
		MountNames: []string{"main"},
		Render: &cpv1.SecretRenderSpec{
			Profile: "gh-and-git-helper-v1",
		},
		GithubToken: &cpv1.GitHubTokenClearMetadata{
			Host:                "github.com",
			Login:               "alice",
			AppClientId:         "Iv1.app",
			AccessExpiresAtUnix: 1893420000,
		},
	}})
	s.nodeKeys.put("node-1", []byte("signed-subkey"), []byte("cert-chain"))

	targets, err := s.githubLinkTargets(context.Background(), "gh-main")
	if err != nil {
		t.Fatalf("targets: %v", err)
	}
	if len(targets) != 1 {
		t.Fatalf("targets len=%d want 1: %+v", len(targets), targets)
	}
	if targets[0].SpawnID != "sp1" || targets[0].NodeID != "node-1" || targets[0].Generation != 1 ||
		string(targets[0].SignedSubkey) != "signed-subkey" || string(targets[0].NodeCertChain) != "cert-chain" {
		t.Fatalf("target = %+v", targets[0])
	}
	if len(targets[0].SecretTemplates) != 1 {
		t.Fatalf("secret templates = %+v, want 1", targets[0].SecretTemplates)
	}
	tmpl := targets[0].SecretTemplates[0]
	if len(tmpl.GetSealed()) != 0 {
		t.Fatalf("secret template leaked sealed ciphertext: %q", tmpl.GetSealed())
	}
	if tmpl.GetSecretId() != "gh-main" || tmpl.GetVersion() != 11 || tmpl.GetDeliveryId() != "delivery-old" ||
		len(tmpl.GetUsages()) != 2 || tmpl.GetMountNames()[0] != "main" ||
		tmpl.GetGithubToken().GetHost() != "github.com" ||
		tmpl.GetGithubToken().GetAccessExpiresAtUnix() != 1893420000 {
		t.Fatalf("secret template lost routing metadata: %+v", tmpl)
	}
}

func TestGitHubLinkTargetsRPCReturnsPublishedNodeKeys(t *testing.T) {
	s, reg, _ := newTestServer(t)
	reg.Add(&registry.Node{ID: "node-1", Sender: &capSender{}})
	createActiveSpawn(t, s, "alice", "sp1", "node-1")
	s.githubLinks.note("sp1", "gh-main")
	s.nodeKeys.put("node-1", []byte("signed-subkey"), []byte("cert-chain"))

	resp, err := s.GetGitHubLinkTargets(context.Background(), connect.NewRequest(&cpv1.GetGitHubLinkTargetsRequest{SecretId: "gh-main"}))
	if err != nil {
		t.Fatalf("GetGitHubLinkTargets RPC: %v", err)
	}
	targets := resp.Msg.GetTargets()
	if len(targets) != 1 || targets[0].GetSpawnId() != "sp1" || targets[0].GetNodeId() != "node-1" ||
		string(targets[0].GetSignedSubkey()) != "signed-subkey" || string(targets[0].GetNodeCertChain()) != "cert-chain" {
		t.Fatalf("targets = %+v", targets)
	}
}

func TestGitHubLinkTargetsIndexesResumeAndRecreateStartupSecrets(t *testing.T) {
	for _, tc := range []struct {
		name    string
		spawnID string
		seed    func(*testing.T, *Server, string, string)
		call    func(context.Context, *Server, string) error
	}{
		{
			name:    "resume",
			spawnID: "sp-gh-resume",
			seed:    seedSuspendedSpawn,
			call: func(ctx context.Context, s *Server, spawnID string) error {
				_, err := s.ResumeSpawn(ctx, connect.NewRequest(&cpv1.ResumeSpawnRequest{
					SpawnId:           spawnID,
					AttachedSecretIds: []string{"gh-main"},
				}))
				return err
			},
		},
		{
			name:    "recreate",
			spawnID: "sp-gh-recreate",
			seed: func(t *testing.T, s *Server, spawnID, owner string) {
				seedErroredSpawn(t, s, spawnID, owner)
				addIntentStartupSecretArtifact(t, s, spawnID, "gh-main")
			},
			call: func(ctx context.Context, s *Server, spawnID string) error {
				_, err := s.RecreateSpawn(ctx, connect.NewRequest(&cpv1.RecreateSpawnRequest{SpawnId: spawnID}))
				return err
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, stopACK := intentTestServer(t)
			defer stopACK()
			createIntentCatalogSecret(t, s, "alice", "gh-main", cpv1.ArtifactTarget_ARTIFACT_TARGET_AGENT, "github/token", "GITHUB_TOKEN")
			s.nodeKeys.put("n-intent", []byte("signed-subkey"), []byte("cert-chain"))
			tc.seed(t, s, tc.spawnID, "alice")

			sessionKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			errCh := make(chan error, 1)
			goSubmitIntentWithSecrets(context.Background(), s, tc.spawnID, "alice", sessionKey, []*cpv1.SealedSecret{intentThreadingCPSecret()}, errCh)

			ownerCtx := auth.WithOwner(context.Background(), "alice")
			if err := tc.call(ownerCtx, s, tc.spawnID); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if submitErr := <-errCh; submitErr != nil {
				t.Fatalf("SubmitIntent error: %v", submitErr)
			}

			resp, err := s.GetGitHubLinkTargets(context.Background(), connect.NewRequest(&cpv1.GetGitHubLinkTargetsRequest{SecretId: "gh-main"}))
			if err != nil {
				t.Fatalf("GetGitHubLinkTargets: %v", err)
			}
			targets := resp.Msg.GetTargets()
			if len(targets) != 1 {
				t.Fatalf("targets len=%d want 1: %+v", len(targets), targets)
			}
			target := targets[0]
			if target.GetSpawnId() != tc.spawnID || target.GetNodeId() != "n-intent" || target.GetGeneration() != 2 ||
				string(target.GetSignedSubkey()) != "signed-subkey" || string(target.GetNodeCertChain()) != "cert-chain" {
				t.Fatalf("target = %+v", target)
			}
			templates := target.GetSecretTemplates()
			if len(templates) != 1 || templates[0].GetSecretId() != "gh-main" ||
				templates[0].GetType() != cpv1.SecretType_SECRET_TYPE_GITHUB_TOKEN || len(templates[0].GetSealed()) != 0 {
				t.Fatalf("secret templates = %+v", templates)
			}
		})
	}
}

func TestGitHubLinkTargetsSkipsDisconnectedAndSubkeylessSiblings(t *testing.T) {
	s, reg, _ := newTestServer(t)
	// node-1: connected + published subkey -> a valid target.
	reg.Add(&registry.Node{ID: "node-1", Sender: &capSender{}})
	createActiveSpawn(t, s, "alice", "sp1", "node-1")
	s.githubLinks.note("sp1", "gh-main")
	s.nodeKeys.put("node-1", []byte("signed-subkey"), []byte("cert-chain"))

	// node-2: hosts a spawn on the link but is NOT connected (absent from registry),
	// yet still has a stale cached subkey. Must be skipped, not abort the call.
	createActiveSpawn(t, s, "alice", "sp2", "node-2")
	s.githubLinks.note("sp2", "gh-main")
	s.nodeKeys.put("node-2", []byte("stale-subkey"), []byte("stale-chain"))

	// node-3: connected but never published a subkey. Must be skipped.
	reg.Add(&registry.Node{ID: "node-3", Sender: &capSender{}})
	createActiveSpawn(t, s, "alice", "sp3", "node-3")
	s.githubLinks.note("sp3", "gh-main")

	targets, err := s.githubLinkTargets(context.Background(), "gh-main")
	if err != nil {
		t.Fatalf("githubLinkTargets must not error on disconnected/subkeyless siblings: %v", err)
	}
	if len(targets) != 1 {
		t.Fatalf("targets len=%d want 1 (only node-1): %+v", len(targets), targets)
	}
	if targets[0].SpawnID != "sp1" || targets[0].NodeID != "node-1" {
		t.Fatalf("unexpected surviving target: %+v", targets[0])
	}
}

func TestFanoutGitHubSealedAccessTokenSkipsDisconnectedSiblingAndDeliversToConnected(t *testing.T) {
	s, reg, _ := newTestServer(t)
	// node-1 connected (the requesting node), node-2 disconnected (absent from registry).
	sender1 := &capSender{}
	reg.Add(&registry.Node{ID: "node-1", Sender: sender1})
	createActiveSpawn(t, s, "alice", "sp1", "node-1")
	createActiveSpawn(t, s, "alice", "sp2", "node-2")
	s.githubLinks.note("sp1", "gh-main")
	s.githubLinks.note("sp2", "gh-main")

	secret1 := &cpv1.SealedSecret{
		SecretId: "gh-main", Type: cpv1.SecretType_SECRET_TYPE_GITHUB_TOKEN,
		Version: 12, DeliveryId: "delivery-sp1-gh-main-v12", Sealed: []byte("sealed-for-node-1"),
		Usages: []cpv1.SecretUsage{cpv1.SecretUsage_SECRET_USAGE_AGENT_RENDER},
	}

	// Deliveries cover ONLY sp1 (node-2 was skipped during sealing because it is disconnected).
	err := s.fanoutGitHubSealedAccessToken(context.Background(), "gh-main", []GitHubSealedAccessTokenDelivery{
		{SpawnID: "sp1", Generation: 1, Secrets: []*cpv1.SealedSecret{secret1}},
	})
	if err != nil {
		t.Fatalf("fanout must succeed despite disconnected sibling sp2/node-2: %v", err)
	}
	got1 := sender1.secretDeliveries()
	if len(got1) != 1 || got1[0].GetSpawnId() != "sp1" ||
		string(got1[0].GetSecrets()[0].GetSealed()) != "sealed-for-node-1" {
		t.Fatalf("requesting node did not receive its sealed token: %+v", got1)
	}
}

func TestFanoutGitHubSealedAccessTokenRelaysOpaqueSecretsToIndexedLiveSpawns(t *testing.T) {
	s, reg, _ := newTestServer(t)
	sender1 := &capSender{}
	sender2 := &capSender{}
	reg.Add(&registry.Node{ID: "node-1", Sender: sender1})
	reg.Add(&registry.Node{ID: "node-2", Sender: sender2})
	createActiveSpawn(t, s, "alice", "sp1", "node-1")
	createActiveSpawn(t, s, "alice", "sp2", "node-2")
	s.githubLinks.note("sp1", "gh-main")
	s.githubLinks.note("sp2", "gh-main")

	secret1 := &cpv1.SealedSecret{
		SecretId:   "gh-main",
		Type:       cpv1.SecretType_SECRET_TYPE_GITHUB_TOKEN,
		Version:    12,
		DeliveryId: "delivery-sp1-gh-main-v12",
		Sealed:     []byte("sealed-for-node-1"),
		Usages:     []cpv1.SecretUsage{cpv1.SecretUsage_SECRET_USAGE_AGENT_RENDER},
	}
	secret2 := &cpv1.SealedSecret{
		SecretId:   "gh-main",
		Type:       cpv1.SecretType_SECRET_TYPE_GITHUB_TOKEN,
		Version:    12,
		DeliveryId: "delivery-sp2-gh-main-v12",
		Sealed:     []byte("sealed-for-node-2"),
		Usages:     []cpv1.SecretUsage{cpv1.SecretUsage_SECRET_USAGE_AGENT_RENDER},
	}

	err := s.fanoutGitHubSealedAccessToken(context.Background(), "gh-main", []GitHubSealedAccessTokenDelivery{
		{SpawnID: "sp1", Generation: 1, Secrets: []*cpv1.SealedSecret{secret1}},
		{SpawnID: "sp2", Generation: 1, Secrets: []*cpv1.SealedSecret{secret2}},
	})
	if err != nil {
		t.Fatalf("fanout: %v", err)
	}
	got1 := sender1.secretDeliveries()
	got2 := sender2.secretDeliveries()
	if len(got1) != 1 || got1[0].GetSpawnId() != "sp1" || len(got1[0].GetSecrets()) != 1 {
		t.Fatalf("sender1 deliveries = %+v", got1)
	}
	if len(got2) != 1 || got2[0].GetSpawnId() != "sp2" || len(got2[0].GetSecrets()) != 1 {
		t.Fatalf("sender2 deliveries = %+v", got2)
	}
	if string(got1[0].GetSecrets()[0].GetSealed()) != "sealed-for-node-1" ||
		string(got2[0].GetSecrets()[0].GetSealed()) != "sealed-for-node-2" {
		t.Fatalf("sealed payloads not relayed opaquely: %q %q",
			got1[0].GetSecrets()[0].GetSealed(), got2[0].GetSecrets()[0].GetSealed())
	}
	if got1[0].GetSecrets()[0].GetType() != nodev1.SecretType_SECRET_TYPE_GITHUB_TOKEN {
		t.Fatalf("secret type = %v", got1[0].GetSecrets()[0].GetType())
	}
}

func TestFanoutGitHubSealedAccessTokenRPCRelaysOpaqueSecrets(t *testing.T) {
	s, reg, _ := newTestServer(t)
	sender := &capSender{}
	reg.Add(&registry.Node{ID: "node-1", Sender: sender})
	createActiveSpawn(t, s, "alice", "sp1", "node-1")
	s.githubLinks.note("sp1", "gh-main")

	_, err := s.FanoutGitHubSealedAccessToken(context.Background(), connect.NewRequest(&cpv1.FanoutGitHubSealedAccessTokenRequest{
		SecretId: "gh-main",
		Deliveries: []*cpv1.GitHubSealedAccessTokenDelivery{{
			SpawnId: "sp1", Generation: 1,
			Secrets: []*cpv1.SealedSecret{{
				SecretId: "gh-main", Type: cpv1.SecretType_SECRET_TYPE_GITHUB_TOKEN,
				Version: 12, DeliveryId: "delivery-sp1-gh-main-v12", Sealed: []byte("sealed"),
			}},
		}},
	}))
	if err != nil {
		t.Fatalf("FanoutGitHubSealedAccessToken RPC: %v", err)
	}
	got := sender.secretDeliveries()
	if len(got) != 1 || string(got[0].GetSecrets()[0].GetSealed()) != "sealed" {
		t.Fatalf("deliveries = %+v", got)
	}
}
