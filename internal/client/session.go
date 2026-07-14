package client

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"time"

	"connectrpc.com/connect"

	authv1 "spawnery/gen/auth/v1"
	cpv1 "spawnery/gen/cp/v1"
	"spawnery/internal/intent"
)

type sessionTargetClient interface {
	GetSpawnNodeKey(context.Context, *connect.Request[cpv1.GetSpawnNodeKeyRequest]) (*connect.Response[cpv1.GetSpawnNodeKeyResponse], error)
}

// BuildSessionOpenIntent resolves and verifies the active node before authorizing a pinned session.
func (c *Client) BuildSessionOpenIntent(ctx context.Context, spawnID string, generation uint64, sessionID string) (*authv1.AuthEnvelope, error) {
	return buildSessionOpenIntent(ctx, c.rpc, c.nodeCredentials, c.targetTrust, spawnID, generation, sessionID)
}

func buildSessionOpenIntent(ctx context.Context, rpc sessionTargetClient, source NodeCredentialSource, trust TargetTrust, spawnID string, generation uint64, sessionID string) (*authv1.AuthEnvelope, error) {
	if spawnID == "" || sessionID == "" {
		return nil, errors.New("session open requires spawn and session ids")
	}
	prepared, err := prepareNodeAuthorization(ctx, source, trust)
	if err != nil {
		return nil, err
	}
	response, err := rpc.GetSpawnNodeKey(ctx, connect.NewRequest(&cpv1.GetSpawnNodeKeyRequest{SpawnId: spawnID}))
	if err != nil {
		return nil, fmt.Errorf("session open %s: GetSpawnNodeKey: %w", spawnID, err)
	}
	if response.Msg.GetGeneration() != generation {
		return nil, fmt.Errorf("session open %s: resolved generation %d does not match active generation %d", spawnID, response.Msg.GetGeneration(), generation)
	}
	if _, err := verifyResolvedTarget(response.Msg.GetNodeCertChain(), response.Msg.GetTargetNodeId(), response.Msg.GetTargetNodeClass(), response.Msg.GetTargetNodeAccountId(), trust); err != nil {
		return nil, fmt.Errorf("session open %s: verify target: %w", spawnID, err)
	}
	credentials, err := prepared.NodeCredentials(ctx)
	if err != nil {
		return nil, fmt.Errorf("session open %s: node credentials: %w", spawnID, err)
	}
	if credentials.AccessToken == "" || credentials.Signer == nil {
		return nil, errors.New("session open received incomplete node credentials")
	}
	var jti [16]byte
	if _, err := rand.Read(jti[:]); err != nil {
		return nil, fmt.Errorf("session open %s: generate jti: %w", spawnID, err)
	}
	body := &authv1.IntentBody{
		Jti: fmt.Sprintf("%x", jti), IssuedAt: time.Now().Unix(), Op: string(intent.OpSessionOpen),
		SpawnId: spawnID, Generation: generation, TargetNodeId: response.Msg.GetTargetNodeId(), SessionId: sessionID,
	}
	signed, err := buildSignedIntent(intent.OpSessionOpen, body, credentials.Signer)
	if err != nil {
		return nil, fmt.Errorf("session open %s: sign: %w", spawnID, err)
	}
	return &authv1.AuthEnvelope{AccessToken: credentials.AccessToken, Intent: signed}, nil
}
