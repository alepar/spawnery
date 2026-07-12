package client

// intent.go contains the staged A4 two-phase client flow [AC1]. Until paired node-credential
// custody is wired into this client, it validates pending tuples but refuses to submit an
// authorization envelope without the required AS-issued node token.
//
// provisionWithIntent retains the existing blocking lifecycle orchestration. Its concurrent
// pollAndSign reports the staged credential error through warn without making SubmitIntent.
//
// Both functions take the narrow intentClient interface (rather than being *Client methods) so
// they stay unit-testable with narrow fakes exactly as they were in spawnctl.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"fmt"
	"time"

	"connectrpc.com/connect"

	authv1 "spawnery/gen/auth/v1"
	cpv1 "spawnery/gen/cp/v1"
	"spawnery/internal/intent"
)

// intentClient is the minimal A4 client interface for polling and signing [AC1].
// cpv1connect.SpawnServiceClient satisfies this interface, enabling both the real implementation
// and narrow fakes for unit tests that don't exercise the intent path.
type intentClient interface {
	GetPendingIntent(context.Context, *connect.Request[cpv1.GetPendingIntentRequest]) (*connect.Response[cpv1.GetPendingIntentResponse], error)
	SubmitIntent(context.Context, *connect.Request[cpv1.SubmitIntentRequest]) (*connect.Response[cpv1.SubmitIntentResponse], error)
}

// ErrNodeCredentialUnavailable is returned locally until the paired AS-issued node credential is
// threaded into this client. Callers must not retry the same unsupported request against the CP.
var ErrNodeCredentialUnavailable = errors.New("node access credential unavailable: paired node credential custody is not wired into this client")

// IntentParams holds the user-initiated parameters the caller knows before pollAndSign — used to
// validate the CP's PendingIntent against the originating request [AM1]. A zero field is not
// validated (the caller did not specify or know that value).
type IntentParams struct {
	AppRef       string // user's requested app_ref (create flow)
	Model        string // user's requested model (create flow)
	TargetNodeID string // user's explicit target node (migrate flow; "" = cloud/CP-assigned)
}

// pollAndSign polls GetPendingIntent until the CP registers the pending intent for spawnID, then
// validates the returned tuple against params [AM1]. It currently fails locally after constructing
// the intent because paired node-credential custody is implemented by sp-dvke.3.3.
//
// pollAndSign MUST be called concurrently with the lifecycle RPC that triggers the two-phase flow —
// that RPC blocks at the CP until the envelope is submitted. Cancel the context to abort early.
func pollAndSign(ctx context.Context, ic intentClient, spawnID string, params IntentParams) error {
	sessionKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("pollAndSign %s: generate session key: %w", spawnID, err)
	}

	// Poll until the CP registers the pending intent, with a generous deadline (> CP's defaultIntentTTL).
	const pollInterval = 200 * time.Millisecond
	const pollDeadline = 120 * time.Second
	deadline := time.Now().Add(pollDeadline)
	var pi *cpv1.PendingIntent
	for {
		resp, err := ic.GetPendingIntent(ctx, connect.NewRequest(&cpv1.GetPendingIntentRequest{SpawnId: spawnID}))
		if err != nil {
			return fmt.Errorf("pollAndSign %s: GetPendingIntent: %w", spawnID, err)
		}
		if resp.Msg.Ready {
			pi = resp.Msg.Pending
			// Validate the CP-supplied tuple against the user's known parameters [AM1]:
			// a compromised CP could substitute a different workload; reject on mismatch.
			if params.AppRef != "" && pi.GetAppRef() != params.AppRef {
				return fmt.Errorf("pollAndSign %s: AM1: CP offered app_ref %q but client requested %q", spawnID, pi.GetAppRef(), params.AppRef)
			}
			if params.Model != "" && pi.GetModel() != params.Model {
				return fmt.Errorf("pollAndSign %s: AM1: CP offered model %q but client requested %q", spawnID, pi.GetModel(), params.Model)
			}
			if params.TargetNodeID != "" && pi.GetTargetNodeId() != params.TargetNodeID {
				return fmt.Errorf("pollAndSign %s: AM1: CP offered target_node_id %q but client requested %q", spawnID, pi.GetTargetNodeId(), params.TargetNodeID)
			}
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

	op := intent.Op(pi.GetOp())

	// Unique JTI: 16 random bytes hex-encoded. A replay within the node's JTI cache window
	// (defaulted to FreshnessWindow + SkewBudget) would be rejected regardless.
	var jtiBytes [16]byte
	if _, err := rand.Read(jtiBytes[:]); err != nil {
		return fmt.Errorf("pollAndSign %s: generate jti: %w", spawnID, err)
	}
	body := &authv1.IntentBody{
		Jti:          fmt.Sprintf("%x", jtiBytes),
		IssuedAt:     time.Now().Unix(),
		SpawnId:      pi.GetSpawnId(),
		Generation:   pi.GetGeneration(),
		TargetNodeId: pi.GetTargetNodeId(),
		Op:           string(op),
		AppRef:       pi.GetAppRef(),
		Image:        pi.GetImage(),
		Model:        pi.GetModel(),
		DataRef:      pi.GetDataRef(),
	}
	if len(pi.GetMounts()) > 0 {
		body.Mounts = make([]*authv1.MountRef, 0, len(pi.GetMounts()))
		for _, mount := range pi.GetMounts() {
			if mount == nil {
				continue
			}
			body.Mounts = append(body.Mounts, &authv1.MountRef{
				Name:               mount.GetName(),
				BackendUri:         mount.GetBackendUri(),
				CredentialSecretId: mount.GetCredentialSecretId(),
				CreateIfMissing:    mount.GetCreateIfMissing(),
				RepositoryId:       mount.GetRepositoryId(),
			})
		}
	}
	_, err = intent.Build(op, body, sessionKey)
	if err != nil {
		return fmt.Errorf("pollAndSign %s: build intent: %w", spawnID, err)
	}
	return fmt.Errorf("pollAndSign %s: %w", spawnID, ErrNodeCredentialUnavailable)
}

// provisionWithIntent orchestrates a blocking lifecycle RPC concurrently with pollAndSign. The
// retry path remains for node NACK handling once credential threading lands in sp-dvke.3.3.
func provisionWithIntent(ctx context.Context, ic intentClient, spawnID string, params IntentParams, doRPC func(context.Context) error, warn func(error)) error {
	if warn == nil {
		warn = func(error) {}
	}
	attempt := func() error {
		pollCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		go func() {
			if err := pollAndSign(pollCtx, ic, spawnID, params); err != nil && !errors.Is(err, context.Canceled) {
				warn(fmt.Errorf("provisionWithIntent %s: pollAndSign: %w", spawnID, err))
			}
		}()
		return doRPC(ctx)
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

// SignProvision validates an async create/fork pending tuple, then returns the staged node-credential
// error without calling SubmitIntent.
func (c *Client) SignProvision(ctx context.Context, spawnID string, p IntentParams) error {
	return pollAndSign(ctx, c.rpc, spawnID, p)
}

// BuildSessionOpenIntent builds a signed session-open AuthEnvelope for binding a new Session
// stream to spawnID at the given episode generation: a fresh ephemeral ECDSA-P256 key, a random
// 16-byte hex jti, an IntentBody, and its A4 signature. Unlike the inline best-effort version this
// replaces (main.go's bindFrame construction, which silently drops errors), it returns the error.
func BuildSessionOpenIntent(spawnID string, generation uint64) (*authv1.AuthEnvelope, error) {
	sessionKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("BuildSessionOpenIntent %s: generate session key: %w", spawnID, err)
	}
	var jtiBytes [16]byte
	if _, err := rand.Read(jtiBytes[:]); err != nil {
		return nil, fmt.Errorf("BuildSessionOpenIntent %s: generate jti: %w", spawnID, err)
	}
	body := &authv1.IntentBody{
		Jti:        fmt.Sprintf("%x", jtiBytes),
		IssuedAt:   time.Now().Unix(),
		SpawnId:    spawnID,
		Generation: generation,
		SessionId:  "0",
		Op:         string(intent.OpSessionOpen),
	}
	si, err := intent.Build(intent.OpSessionOpen, body, sessionKey)
	if err != nil {
		return nil, fmt.Errorf("BuildSessionOpenIntent %s: build intent: %w", spawnID, err)
	}
	return &authv1.AuthEnvelope{Intent: si}, nil
}
