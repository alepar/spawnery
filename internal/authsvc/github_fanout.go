package authsvc

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/internal/clientverify"
	"spawnery/internal/pki"
	"spawnery/internal/secrets/seal"
	"spawnery/internal/secrets/subkey"
)

type GitHubFanoutCP interface {
	GetGitHubLinkTargets(context.Context, *connect.Request[cpv1.GetGitHubLinkTargetsRequest]) (*connect.Response[cpv1.GetGitHubLinkTargetsResponse], error)
	FanoutGitHubSealedAccessToken(context.Context, *connect.Request[cpv1.FanoutGitHubSealedAccessTokenRequest]) (*connect.Response[cpv1.FanoutGitHubSealedAccessTokenResponse], error)
}

type cpGitHubAccessTokenFanout struct {
	cp      GitHubFanoutCP
	rootPEM []byte
	now     func() time.Time
}

func NewCPGitHubAccessTokenFanout(cp GitHubFanoutCP, rootPEM []byte, now func() time.Time) GitHubAccessTokenFanoutNotifier {
	if now == nil {
		now = time.Now
	}
	return &cpGitHubAccessTokenFanout{
		cp:      cp,
		rootPEM: append([]byte(nil), rootPEM...),
		now:     now,
	}
}

func (f *cpGitHubAccessTokenFanout) FanoutGitHubAccessToken(ctx context.Context, req GitHubAccessTokenFanout) error {
	if f.cp == nil {
		return fmt.Errorf("github fanout cp client is not configured")
	}
	if strings.TrimSpace(req.SecretID) == "" || strings.TrimSpace(req.AccountID) == "" ||
		req.Version == 0 || strings.TrimSpace(req.DeliveryID) == "" || req.AccessToken == "" {
		return fmt.Errorf("github fanout requires secret_id, account_id, version, delivery_id, and access_token")
	}
	if len(f.rootPEM) == 0 {
		return fmt.Errorf("github fanout requires pinned root CA PEM")
	}

	targetsResp, err := f.cp.GetGitHubLinkTargets(ctx, connect.NewRequest(&cpv1.GetGitHubLinkTargetsRequest{
		SecretId: req.SecretID,
	}))
	if err != nil {
		return err
	}
	deliveries := make([]*cpv1.GitHubSealedAccessTokenDelivery, 0, len(targetsResp.Msg.GetTargets()))
	for _, target := range targetsResp.Msg.GetTargets() {
		delivery, err := f.sealForTarget(target, req)
		if err != nil {
			return err
		}
		deliveries = append(deliveries, delivery)
	}
	_, err = f.cp.FanoutGitHubSealedAccessToken(ctx, connect.NewRequest(&cpv1.FanoutGitHubSealedAccessTokenRequest{
		SecretId:   req.SecretID,
		Deliveries: deliveries,
	}))
	return err
}

func (f *cpGitHubAccessTokenFanout) sealForTarget(target *cpv1.GitHubLinkTarget, req GitHubAccessTokenFanout) (*cpv1.GitHubSealedAccessTokenDelivery, error) {
	if target == nil || target.GetSpawnId() == "" || target.GetNodeId() == "" || target.GetGeneration() == 0 {
		return nil, fmt.Errorf("github fanout target is missing spawn_id, node_id, or generation")
	}
	if len(target.GetSecretTemplates()) == 0 {
		return nil, fmt.Errorf("github fanout target %s has no secret templates", target.GetSpawnId())
	}
	var signed subkey.SignedSubKey
	if err := json.Unmarshal(target.GetSignedSubkey(), &signed); err != nil {
		return nil, fmt.Errorf("github fanout target %s: decode SignedSubKey: %w", target.GetSpawnId(), err)
	}
	leafPEM, chainPEM, err := splitGitHubFanoutCertChain(target.GetNodeCertChain())
	if err != nil {
		return nil, fmt.Errorf("github fanout target %s: split node cert chain: %w", target.GetSpawnId(), err)
	}
	hpkePub, id, err := f.verifyTargetNode(leafPEM, chainPEM, signed, req.AccountID)
	if err != nil {
		return nil, fmt.Errorf("github fanout target %s: verify node: %w", target.GetSpawnId(), err)
	}
	if id.NodeID != target.GetNodeId() {
		return nil, fmt.Errorf("github fanout target %s: verified node %q does not match CP target %q", target.GetSpawnId(), id.NodeID, target.GetNodeId())
	}

	secrets := make([]*cpv1.SealedSecret, 0, len(target.GetSecretTemplates()))
	for _, tmpl := range target.GetSecretTemplates() {
		if tmpl.GetSecretId() != req.SecretID {
			return nil, fmt.Errorf("github fanout target %s: template secret_id %q does not match %q", target.GetSpawnId(), tmpl.GetSecretId(), req.SecretID)
		}
		secret := cloneGitHubFanoutTemplate(tmpl)
		secret.Version = req.Version
		secret.DeliveryId = req.DeliveryID
		aad := seal.InFlightAAD{
			SpawnID:    target.GetSpawnId(),
			Generation: target.GetGeneration(),
			NodeID:     id.NodeID,
			NotAfter:   signed.NotAfter,
			Version:    req.Version,
			DeliveryID: req.DeliveryID,
		}
		sealed, err := seal.SealPlaintextToNode([]byte(req.AccessToken), hpkePub, aad)
		if err != nil {
			return nil, fmt.Errorf("github fanout target %s: seal secret %q: %w", target.GetSpawnId(), req.SecretID, err)
		}
		sealedJSON, err := json.Marshal(sealed)
		if err != nil {
			return nil, fmt.Errorf("github fanout target %s: encode sealed secret %q: %w", target.GetSpawnId(), req.SecretID, err)
		}
		secret.Sealed = sealedJSON
		secrets = append(secrets, secret)
	}
	return &cpv1.GitHubSealedAccessTokenDelivery{
		SpawnId:    target.GetSpawnId(),
		Generation: target.GetGeneration(),
		Secrets:    secrets,
	}, nil
}

func (f *cpGitHubAccessTokenFanout) verifyTargetNode(leafPEM, chainPEM []byte, signed subkey.SignedSubKey, accountID string) ([]byte, pki.Identity, error) {
	now := f.now()
	hpkePub, id, selfHostedErr := subkey.VerifyNodeForSealing(
		leafPEM,
		chainPEM,
		f.rootPEM,
		signed,
		clientverify.Expectation{Tenancy: pki.ClassSelfHosted, AccountID: accountID},
		subkey.AllowAll{},
		now,
	)
	if selfHostedErr == nil {
		return hpkePub, id, nil
	}
	hpkePub, id, cloudErr := subkey.VerifyNodeForSealing(
		leafPEM,
		chainPEM,
		f.rootPEM,
		signed,
		clientverify.Expectation{Tenancy: pki.ClassCloud},
		subkey.AllowAll{},
		now,
	)
	if cloudErr == nil {
		return hpkePub, id, nil
	}
	return nil, pki.Identity{}, selfHostedErr
}

func splitGitHubFanoutCertChain(chain []byte) (leafPEM, intermediatesPEM []byte, err error) {
	var certs [][]byte
	rest := chain
	for len(rest) > 0 {
		block, rem := pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type == "CERTIFICATE" {
			certs = append(certs, pem.EncodeToMemory(block))
		}
		rest = rem
	}
	if len(certs) == 0 {
		return nil, nil, fmt.Errorf("missing PEM certificates")
	}
	leafPEM = certs[0]
	for _, cert := range certs[1:] {
		intermediatesPEM = append(intermediatesPEM, cert...)
	}
	return leafPEM, intermediatesPEM, nil
}

func cloneGitHubFanoutTemplate(tmpl *cpv1.SealedSecret) *cpv1.SealedSecret {
	out := &cpv1.SealedSecret{
		TargetPath: tmpl.GetTargetPath(),
		SecretId:   tmpl.GetSecretId(),
		Type:       tmpl.GetType(),
		Usages:     append([]cpv1.SecretUsage(nil), tmpl.GetUsages()...),
		MountNames: append([]string(nil), tmpl.GetMountNames()...),
	}
	if tmpl.GetRender() != nil {
		out.Render = &cpv1.SecretRenderSpec{
			Profile:              tmpl.GetRender().GetProfile(),
			TargetPath:           tmpl.GetRender().GetTargetPath(),
			GhConfigDir:          tmpl.GetRender().GetGhConfigDir(),
			HostsPath:            tmpl.GetRender().GetHostsPath(),
			GitConfigPath:        tmpl.GetRender().GetGitConfigPath(),
			CredentialHelperPath: tmpl.GetRender().GetCredentialHelperPath(),
		}
	}
	if tmpl.GetGithubToken() != nil {
		out.GithubToken = &cpv1.GitHubTokenClearMetadata{
			Host:                 tmpl.GetGithubToken().GetHost(),
			Login:                tmpl.GetGithubToken().GetLogin(),
			GithubUserId:         tmpl.GetGithubToken().GetGithubUserId(),
			RefreshExpiresAtUnix: tmpl.GetGithubToken().GetRefreshExpiresAtUnix(),
			AppClientId:          tmpl.GetGithubToken().GetAppClientId(),
		}
	}
	return out
}
