package client

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	authv1 "spawnery/gen/auth/v1"
	cpv1 "spawnery/gen/cp/v1"
	"spawnery/internal/intent"
)

type intentClient interface {
	GetPendingIntent(context.Context, *connect.Request[cpv1.GetPendingIntentRequest]) (*connect.Response[cpv1.GetPendingIntentResponse], error)
	SubmitIntent(context.Context, *connect.Request[cpv1.SubmitIntentRequest]) (*connect.Response[cpv1.SubmitIntentResponse], error)
}

type IntentParams struct {
	Op           intent.Op
	AppRef       string
	Model        string
	TargetNodeID string
	TargetClass  string
	Mounts       []*cpv1.MountBinding
}

func pollAndSign(ctx context.Context, ic intentClient, credentials NodeCredentialSource, trust TargetTrust, spawnID string, params IntentParams) error {
	const pollInterval = 200 * time.Millisecond
	const pollDeadline = 120 * time.Second
	deadline := time.Now().Add(pollDeadline)
	var response *cpv1.GetPendingIntentResponse
	for {
		resp, err := ic.GetPendingIntent(ctx, connect.NewRequest(&cpv1.GetPendingIntentRequest{SpawnId: spawnID}))
		if err != nil {
			return fmt.Errorf("pollAndSign %s: GetPendingIntent: %w", spawnID, err)
		}
		if resp.Msg.GetReady() {
			response = resp.Msg
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("pollAndSign %s: GetPendingIntent did not become ready within %s", spawnID, pollDeadline)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}

	pi, op, err := validatePendingIntent(response, spawnID, params)
	if err != nil {
		return fmt.Errorf("pollAndSign %s: %w", spawnID, err)
	}
	if _, err := verifyResolvedTarget(response.GetNodeCertChain(), response.GetTargetNodeId(), response.GetTargetNodeClass(), response.GetTargetNodeAccountId(), trust); err != nil {
		return fmt.Errorf("pollAndSign %s: verify target: %w", spawnID, err)
	}
	if credentials == nil {
		return fmt.Errorf("pollAndSign %s: node authorization requires login credentials", spawnID)
	}
	creds, err := credentials.NodeCredentials(ctx)
	if err != nil {
		return fmt.Errorf("pollAndSign %s: node credentials: %w", spawnID, err)
	}
	if creds.AccessToken == "" || creds.Signer == nil {
		return fmt.Errorf("pollAndSign %s: incomplete node credentials", spawnID)
	}

	body, err := intentBodyFromPending(pi, op)
	if err != nil {
		return fmt.Errorf("pollAndSign %s: %w", spawnID, err)
	}
	signed, err := buildSignedIntent(op, body, creds.Signer)
	if err != nil {
		return fmt.Errorf("pollAndSign %s: build intent: %w", spawnID, err)
	}
	_, err = ic.SubmitIntent(ctx, connect.NewRequest(&cpv1.SubmitIntentRequest{
		SpawnId: spawnID, Intent: signed, NodeAccessToken: creds.AccessToken,
	}))
	if err != nil {
		return fmt.Errorf("pollAndSign %s: SubmitIntent: %w", spawnID, err)
	}
	return nil
}

func validatePendingIntent(response *cpv1.GetPendingIntentResponse, spawnID string, params IntentParams) (*cpv1.PendingIntent, intent.Op, error) {
	if response == nil || response.GetPending() == nil {
		return nil, "", errors.New("ready response has no pending intent")
	}
	pi := response.GetPending()
	op := intent.Op(pi.GetOp())
	switch op {
	case intent.OpCreateSpawn, intent.OpResumeSpawn, intent.OpRecreateSpawn, intent.OpMigrateSpawn, intent.OpForkSpawn:
	default:
		return nil, "", fmt.Errorf("unsupported operation %q", pi.GetOp())
	}
	if params.Op == "" || op != params.Op {
		return nil, "", fmt.Errorf("operation %q does not match requested %q", op, params.Op)
	}
	if pi.GetSpawnId() != spawnID {
		return nil, "", fmt.Errorf("pending spawn_id %q does not match requested %q", pi.GetSpawnId(), spawnID)
	}
	if params.AppRef != "" && pi.GetAppRef() != params.AppRef {
		return nil, "", fmt.Errorf("AM1: app_ref %q does not match requested %q", pi.GetAppRef(), params.AppRef)
	}
	if params.Model != "" && pi.GetModel() != params.Model {
		return nil, "", fmt.Errorf("AM1: model %q does not match requested %q", pi.GetModel(), params.Model)
	}
	if params.TargetNodeID != "" && pi.GetTargetNodeId() != params.TargetNodeID {
		return nil, "", fmt.Errorf("AM1: target_node_id %q does not match requested %q", pi.GetTargetNodeId(), params.TargetNodeID)
	}
	if response.GetGeneration() != pi.GetGeneration() {
		return nil, "", fmt.Errorf("response generation %d does not match pending %d", response.GetGeneration(), pi.GetGeneration())
	}
	if response.GetTargetNodeId() == "" || response.GetTargetNodeId() != pi.GetTargetNodeId() {
		return nil, "", fmt.Errorf("response target_node_id %q does not match pending %q", response.GetTargetNodeId(), pi.GetTargetNodeId())
	}
	if params.TargetClass != "" && response.GetTargetNodeClass() != params.TargetClass {
		return nil, "", fmt.Errorf("target_node_class %q does not match requested %q", response.GetTargetNodeClass(), params.TargetClass)
	}
	if params.Mounts != nil && !mountBindingsEqual(pi.GetMounts(), params.Mounts) {
		return nil, "", errors.New("mounts do not match requested mounts")
	}
	return pi, op, nil
}

func mountBindingsEqual(a, b []*cpv1.MountBinding) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !proto.Equal(a[i], b[i]) {
			return false
		}
	}
	return true
}

func intentBodyFromPending(pi *cpv1.PendingIntent, op intent.Op) (*authv1.IntentBody, error) {
	var jti [16]byte
	if _, err := rand.Read(jti[:]); err != nil {
		return nil, fmt.Errorf("generate jti: %w", err)
	}
	body := &authv1.IntentBody{
		Jti: fmt.Sprintf("%x", jti), IssuedAt: time.Now().Unix(), SpawnId: pi.GetSpawnId(),
		Generation: pi.GetGeneration(), TargetNodeId: pi.GetTargetNodeId(), Op: string(op),
		AppRef: pi.GetAppRef(), Image: pi.GetImage(), Model: pi.GetModel(), DataRef: pi.GetDataRef(),
	}
	for _, mount := range pi.GetMounts() {
		if mount != nil {
			body.Mounts = append(body.Mounts, &authv1.MountRef{Name: mount.GetName(), BackendUri: mount.GetBackendUri(), CredentialSecretId: mount.GetCredentialSecretId(), CreateIfMissing: mount.GetCreateIfMissing(), RepositoryId: mount.GetRepositoryId()})
		}
	}
	return body, nil
}

func provisionWithIntent(ctx context.Context, ic intentClient, credentials NodeCredentialSource, trust TargetTrust, spawnID string, params IntentParams, doRPC func(context.Context) error, warn func(error)) error {
	if warn == nil {
		warn = func(error) {}
	}
	attempt := func() error {
		attemptCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		signCh := make(chan error, 1)
		rpcCh := make(chan error, 1)
		go func() { signCh <- pollAndSign(attemptCtx, ic, credentials, trust, spawnID, params) }()
		go func() { rpcCh <- doRPC(attemptCtx) }()
		for {
			select {
			case err := <-signCh:
				if err != nil && !errors.Is(err, context.Canceled) {
					return fmt.Errorf("provisionWithIntent %s: pollAndSign: %w", spawnID, err)
				}
				signCh = nil
			case err := <-rpcCh:
				return err
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	err := attempt()
	if err == nil {
		return nil
	}
	var connErr *connect.Error
	if errors.As(err, &connErr) && connErr.Code() == connect.CodeFailedPrecondition && intent.RetryableNACK(connErr.Message()) {
		warn(fmt.Errorf("provisionWithIntent %s: retryable NACK (%s); retrying once", spawnID, connErr.Message()))
		return attempt()
	}
	return err
}

func (c *Client) SignProvision(ctx context.Context, spawnID string, p IntentParams) error {
	return pollAndSign(ctx, c.rpc, c.nodeCredentials, c.targetTrust, spawnID, p)
}
