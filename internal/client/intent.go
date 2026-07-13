package client

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"slices"
	"time"

	"connectrpc.com/connect"

	authv1 "spawnery/gen/auth/v1"
	cpv1 "spawnery/gen/cp/v1"
	"spawnery/internal/intent"
)

type intentClient interface {
	GetPendingIntent(context.Context, *connect.Request[cpv1.GetPendingIntentRequest]) (*connect.Response[cpv1.GetPendingIntentResponse], error)
	SubmitIntent(context.Context, *connect.Request[cpv1.SubmitIntentRequest]) (*connect.Response[cpv1.SubmitIntentResponse], error)
}

type IntentParams struct {
	Op                intent.Op
	AppRef            string
	Model             string
	Image             string
	TargetNodeID      string
	TargetClass       string
	Mounts            []*cpv1.MountBinding
	AttachedSecretIDs []string
}

func pollAndSign(ctx context.Context, ic intentClient, credentials NodeCredentialSource, trust TargetTrust, spawnID string, params IntentParams) error {
	_, err := pollAndSignResolved(ctx, ic, credentials, trust, spawnID, params, func(*cpv1.GetPendingIntentResponse) (string, error) {
		return spawnID, nil
	})
	return err
}

func pollAndSignFork(ctx context.Context, ic intentClient, credentials NodeCredentialSource, trust TargetTrust, sourceSpawnID string, params IntentParams) (string, error) {
	return pollAndSignResolved(ctx, ic, credentials, trust, sourceSpawnID, params, func(response *cpv1.GetPendingIntentResponse) (string, error) {
		forkSpawnID := response.GetPending().GetSpawnId()
		if forkSpawnID == "" {
			return "", errors.New("pending fork spawn_id is empty")
		}
		if forkSpawnID == sourceSpawnID {
			return "", errors.New("pending fork spawn_id must differ from source")
		}
		return forkSpawnID, nil
	})
}

func pollAndSignResolved(ctx context.Context, ic intentClient, credentials NodeCredentialSource, trust TargetTrust, lookupSpawnID string, params IntentParams, resolveAuthorizedSpawnID func(*cpv1.GetPendingIntentResponse) (string, error)) (string, error) {
	const pollInterval = 200 * time.Millisecond
	const pollDeadline = 120 * time.Second
	deadline := time.Now().Add(pollDeadline)
	var response *cpv1.GetPendingIntentResponse
	for {
		resp, err := ic.GetPendingIntent(ctx, connect.NewRequest(&cpv1.GetPendingIntentRequest{SpawnId: lookupSpawnID}))
		if err != nil {
			return "", fmt.Errorf("pollAndSign %s: GetPendingIntent: %w", lookupSpawnID, err)
		}
		if resp.Msg.GetReady() {
			response = resp.Msg
			break
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("pollAndSign %s: GetPendingIntent did not become ready within %s", lookupSpawnID, pollDeadline)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(pollInterval):
		}
	}

	authorizedSpawnID, err := resolveAuthorizedSpawnID(response)
	if err != nil {
		return "", fmt.Errorf("pollAndSign %s: %w", lookupSpawnID, err)
	}
	pi, op, err := validatePendingIntent(response, authorizedSpawnID, params)
	if err != nil {
		return "", fmt.Errorf("pollAndSign %s: %w", lookupSpawnID, err)
	}
	if _, err := verifyResolvedTarget(response.GetNodeCertChain(), response.GetTargetNodeId(), response.GetTargetNodeClass(), response.GetTargetNodeAccountId(), trust); err != nil {
		return "", fmt.Errorf("pollAndSign %s: verify target: %w", lookupSpawnID, err)
	}
	if credentials == nil {
		return "", fmt.Errorf("pollAndSign %s: node authorization requires login credentials", lookupSpawnID)
	}
	creds, err := credentials.NodeCredentials(ctx)
	if err != nil {
		return "", fmt.Errorf("pollAndSign %s: node credentials: %w", lookupSpawnID, err)
	}
	if creds.AccessToken == "" || creds.Signer == nil {
		return "", fmt.Errorf("pollAndSign %s: incomplete node credentials", lookupSpawnID)
	}

	body, err := intentBodyFromPending(pi, op)
	if err != nil {
		return "", fmt.Errorf("pollAndSign %s: %w", lookupSpawnID, err)
	}
	signed, err := buildSignedIntent(op, body, creds.Signer)
	if err != nil {
		return "", fmt.Errorf("pollAndSign %s: build intent: %w", lookupSpawnID, err)
	}
	_, err = ic.SubmitIntent(ctx, connect.NewRequest(&cpv1.SubmitIntentRequest{
		SpawnId: lookupSpawnID, Intent: signed, NodeAccessToken: creds.AccessToken,
	}))
	if err != nil {
		return "", fmt.Errorf("pollAndSign %s: SubmitIntent: %w", lookupSpawnID, err)
	}
	return authorizedSpawnID, nil
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
	if params.Image != "" && pi.GetImage() != params.Image {
		return nil, "", fmt.Errorf("AM1: image %q does not match requested %q", pi.GetImage(), params.Image)
	}
	if params.AttachedSecretIDs != nil && !containsAllStrings(pi.GetAttachedSecretIds(), params.AttachedSecretIDs) {
		return nil, "", fmt.Errorf("AM1: resolved attached_secret_ids %v omit caller-selected %v", pi.GetAttachedSecretIds(), params.AttachedSecretIDs)
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
	if params.Mounts != nil && !mountBindingsContain(pi.GetMounts(), params.Mounts) {
		return nil, "", errors.New("mounts do not match requested mounts")
	}
	return pi, op, nil
}

func containsAllStrings(resolved, expected []string) bool {
	set := make(map[string]struct{}, len(resolved))
	for _, value := range resolved {
		set[value] = struct{}{}
	}
	for _, value := range expected {
		if _, ok := set[value]; !ok {
			return false
		}
	}
	return true
}

func mountBindingsContain(resolved, selected []*cpv1.MountBinding) bool {
	byName := make(map[string]*cpv1.MountBinding, len(resolved))
	for _, mount := range resolved {
		if mount == nil || mount.GetName() == "" {
			return false
		}
		if _, duplicate := byName[mount.GetName()]; duplicate {
			return false
		}
		byName[mount.GetName()] = mount
	}
	for _, want := range selected {
		if want == nil || want.GetName() == "" {
			return false
		}
		got := byName[want.GetName()]
		if got == nil || got.GetBackendUri() != want.GetBackendUri() ||
			got.GetCreateIfMissing() != want.GetCreateIfMissing() || got.GetRepositoryId() != want.GetRepositoryId() ||
			(want.GetCredentialSecretId() != "" && got.GetCredentialSecretId() != want.GetCredentialSecretId()) {
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
	body.AttachedSecretIds = canonicalStringSet(pi.GetAttachedSecretIds())
	for _, mount := range pi.GetMounts() {
		if mount != nil {
			body.Mounts = append(body.Mounts, &authv1.MountRef{Name: mount.GetName(), BackendUri: mount.GetBackendUri(), CredentialSecretId: mount.GetCredentialSecretId(), CreateIfMissing: mount.GetCreateIfMissing(), RepositoryId: mount.GetRepositoryId()})
		}
	}
	return body, nil
}

func canonicalStringSet(values []string) []string {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value != "" {
			set[value] = struct{}{}
		}
	}
	result := make([]string, 0, len(set))
	for value := range set {
		result = append(result, value)
	}
	slices.Sort(result)
	return result
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
		signDone, rpcDone := false, false
		var firstErr error
		ctxDone := ctx.Done()
		for !signDone || !rpcDone {
			select {
			case err := <-signCh:
				if err != nil && firstErr == nil {
					firstErr = fmt.Errorf("provisionWithIntent %s: pollAndSign: %w", spawnID, err)
					cancel()
				}
				signDone = true
				signCh = nil
			case err := <-rpcCh:
				if err != nil && firstErr == nil {
					firstErr = err
					cancel()
				}
				rpcDone = true
				rpcCh = nil
			case <-ctxDone:
				if firstErr == nil {
					firstErr = ctx.Err()
				}
				cancel()
				ctxDone = nil
			}
		}
		return firstErr
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
	prepared, err := prepareNodeAuthorization(ctx, c.nodeCredentials, c.targetTrust)
	if err != nil {
		return err
	}
	return pollAndSign(ctx, c.rpc, prepared, c.targetTrust, spawnID, p)
}
