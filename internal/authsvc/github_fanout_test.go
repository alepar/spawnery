package authsvc

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/internal/pki"
	"spawnery/internal/secrets/seal"
	"spawnery/internal/secrets/subkey"
)

type fakeGitHubFanoutCP struct {
	targets []*cpv1.GitHubLinkTarget
	got     *cpv1.FanoutGitHubSealedAccessTokenRequest
}

func (f *fakeGitHubFanoutCP) GetGitHubLinkTargets(_ context.Context, req *connect.Request[cpv1.GetGitHubLinkTargetsRequest]) (*connect.Response[cpv1.GetGitHubLinkTargetsResponse], error) {
	return connect.NewResponse(&cpv1.GetGitHubLinkTargetsResponse{Targets: f.targets}), nil
}

func (f *fakeGitHubFanoutCP) FanoutGitHubSealedAccessToken(_ context.Context, req *connect.Request[cpv1.FanoutGitHubSealedAccessTokenRequest]) (*connect.Response[cpv1.FanoutGitHubSealedAccessTokenResponse], error) {
	f.got = req.Msg
	return connect.NewResponse(&cpv1.FanoutGitHubSealedAccessTokenResponse{}), nil
}

type fanoutNodeFixture struct {
	rootPEM    []byte
	chainPEM   []byte
	subkeyJSON []byte
	holder     *subkey.Node
}

func newFanoutNodeFixture(t *testing.T, nodeID, accountID string, now time.Time) fanoutNodeFixture {
	t.Helper()
	root, err := pki.NewRootCA("fanout-root")
	if err != nil {
		t.Fatalf("root CA: %v", err)
	}
	inter, err := root.NewIntermediate(pki.ClassSelfHosted)
	if err != nil {
		t.Fatalf("intermediate: %v", err)
	}
	node, err := inter.IssueNode(nodeID, accountID, pki.ClassSelfHosted, now.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("issue node: %v", err)
	}
	holder := subkey.NewNode(node.Key, nodeID, time.Hour)
	published, err := holder.Rotate(now)
	if err != nil {
		t.Fatalf("rotate subkey: %v", err)
	}
	subkeyJSON, err := json.Marshal(published)
	if err != nil {
		t.Fatalf("marshal subkey: %v", err)
	}
	return fanoutNodeFixture{
		rootPEM:    pki.MarshalCertPEM(root.Cert),
		chainPEM:   append(pki.MarshalCertPEM(node.Cert), pki.MarshalCertPEM(inter.Cert)...),
		subkeyJSON: subkeyJSON,
		holder:     holder,
	}
}

func TestCPGitHubFanoutSealsAccessTokenUsingTargetTemplates(t *testing.T) {
	now := time.Now()
	fx := newFanoutNodeFixture(t, "node-1", "alice", now)
	cp := &fakeGitHubFanoutCP{targets: []*cpv1.GitHubLinkTarget{{
		SpawnId:       "sp1",
		NodeId:        "node-1",
		Generation:    3,
		SignedSubkey:  fx.subkeyJSON,
		NodeCertChain: fx.chainPEM,
		SecretTemplates: []*cpv1.SealedSecret{{
			SecretId:   "gh-main",
			Sealed:     []byte("old-ciphertext"),
			Type:       cpv1.SecretType_SECRET_TYPE_GITHUB_TOKEN,
			Version:    11,
			DeliveryId: "old-delivery",
			Usages: []cpv1.SecretUsage{
				cpv1.SecretUsage_SECRET_USAGE_NODE_STORAGE,
				cpv1.SecretUsage_SECRET_USAGE_AGENT_RENDER,
			},
			MountNames: []string{"main"},
			Render: &cpv1.SecretRenderSpec{
				Profile:     "gh-and-git-helper-v1",
				TargetPath:  "github",
				GhConfigDir: "github/gh",
			},
			GithubToken: &cpv1.GitHubTokenClearMetadata{
				Host:        "github.com",
				Login:       "alice",
				AppClientId: "Iv1.app",
			},
		}},
	}}}

	fanout := NewCPGitHubAccessTokenFanout(cp, fx.rootPEM, func() time.Time { return now })
	err := fanout.FanoutGitHubAccessToken(context.Background(), GitHubAccessTokenFanout{
		SecretID:            "gh-main",
		AccountID:           "alice",
		Version:             12,
		DeliveryID:          "github-access-gh-main-v12",
		AccessToken:         "ghu_rotated",
		AccessExpiresAtUnix: now.Add(8 * time.Hour).Unix(),
		TokenType:           "bearer",
	})
	if err != nil {
		t.Fatalf("fanout: %v", err)
	}
	if cp.got == nil || cp.got.GetSecretId() != "gh-main" || len(cp.got.GetDeliveries()) != 1 {
		t.Fatalf("fanout request = %+v", cp.got)
	}
	delivery := cp.got.GetDeliveries()[0]
	if delivery.GetSpawnId() != "sp1" || delivery.GetGeneration() != 3 || len(delivery.GetSecrets()) != 1 {
		t.Fatalf("delivery = %+v", delivery)
	}
	secret := delivery.GetSecrets()[0]
	if secret.GetVersion() != 12 || secret.GetDeliveryId() != "github-access-gh-main-v12" ||
		secret.GetSecretId() != "gh-main" || secret.GetMountNames()[0] != "main" ||
		secret.GetGithubToken().GetHost() != "github.com" {
		t.Fatalf("secret metadata = %+v", secret)
	}
	if string(secret.GetSealed()) == "old-ciphertext" || len(secret.GetSealed()) == 0 {
		t.Fatalf("secret sealed payload was not replaced: %q", secret.GetSealed())
	}
	var sealed seal.NodeSealed
	if err := json.Unmarshal(secret.GetSealed(), &sealed); err != nil {
		t.Fatalf("decode sealed payload: %v", err)
	}
	opened, err := fx.holder.OpenDelivered(&sealed, seal.InFlightAAD{
		SpawnID:    "sp1",
		Generation: 3,
		Version:    12,
		DeliveryID: "github-access-gh-main-v12",
	}, now)
	if err != nil {
		t.Fatalf("open sealed payload: %v", err)
	}
	if string(opened) != "ghu_rotated" {
		t.Fatalf("opened payload = %q", opened)
	}
}

func TestCPGitHubFanoutRejectsWrongSelfHostedAccount(t *testing.T) {
	now := time.Now()
	fx := newFanoutNodeFixture(t, "node-1", "mallory", now)
	cp := &fakeGitHubFanoutCP{targets: []*cpv1.GitHubLinkTarget{{
		SpawnId:       "sp1",
		NodeId:        "node-1",
		Generation:    3,
		SignedSubkey:  fx.subkeyJSON,
		NodeCertChain: fx.chainPEM,
		SecretTemplates: []*cpv1.SealedSecret{{
			SecretId: "gh-main",
			Type:     cpv1.SecretType_SECRET_TYPE_GITHUB_TOKEN,
		}},
	}}}

	fanout := NewCPGitHubAccessTokenFanout(cp, fx.rootPEM, func() time.Time { return now })
	err := fanout.FanoutGitHubAccessToken(context.Background(), GitHubAccessTokenFanout{
		SecretID:    "gh-main",
		AccountID:   "alice",
		Version:     12,
		DeliveryID:  "github-access-gh-main-v12",
		AccessToken: "ghu_rotated",
	})
	if err == nil {
		t.Fatal("fanout accepted a self-hosted node for the wrong account")
	}
	if cp.got != nil {
		t.Fatalf("CP fanout must not be called after verification failure: %+v", cp.got)
	}
}
