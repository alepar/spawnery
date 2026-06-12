package main

// intent.go: spawnctl-side A4 two-phase sign-after-resolve loop [AC1][AM12].
// pollAndSign is the client half of the protocol: it generates an ephemeral ECDSA P-256 session
// key, polls GetPendingIntent until the CP registers the pending intent, builds and signs the
// IntentBody, then submits via SubmitIntent. It must be called concurrently with the lifecycle
// RPC (CreateSpawn, MigrateSpawn, etc.) that blocks until the envelope is submitted.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"fmt"
	"time"

	"connectrpc.com/connect"

	authv1 "spawnery/gen/auth/v1"
	cpv1 "spawnery/gen/cp/v1"
	"spawnery/internal/intent"
)

// intentClient is the minimal A4 client interface for polling and signing [AC1][AM12].
// cpv1connect.SpawnServiceClient satisfies this interface, enabling both the real implementation
// and narrow fakes for unit tests that don't exercise the intent path.
type intentClient interface {
	GetPendingIntent(context.Context, *connect.Request[cpv1.GetPendingIntentRequest]) (*connect.Response[cpv1.GetPendingIntentResponse], error)
	SubmitIntent(context.Context, *connect.Request[cpv1.SubmitIntentRequest]) (*connect.Response[cpv1.SubmitIntentResponse], error)
}

// intentParams holds the user-initiated parameters spawnctl knows before pollAndSign —
// used to validate the CP's PendingIntent against the originating request [AM1].
// A zero field is not validated (the caller did not specify or know that value).
type intentParams struct {
	AppRef       string // user's requested app_ref (create flow)
	Model        string // user's requested model (create flow)
	TargetNodeID string // user's explicit target node (migrate flow; "" = cloud/CP-assigned)
}

// pollAndSign polls GetPendingIntent until the CP registers the pending intent for spawnID, then
// validates the returned tuple against params [AM1], builds and submits a signed AuthEnvelope. An
// ephemeral ECDSA P-256 session key is generated per call; the caller need not manage key material.
// In dev mode the CP mints the cnf-bearing aud=node token from the SPKI DER embedded in the
// SignedIntent (NodeAccessToken left empty here).
//
// pollAndSign MUST be called concurrently with the lifecycle RPC that triggers the two-phase flow —
// that RPC blocks at the CP until the envelope is submitted. Cancel the context to abort early.
func pollAndSign(ctx context.Context, ic intentClient, spawnID string, params intentParams) error {
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
	si, err := intent.Build(op, body, sessionKey)
	if err != nil {
		return fmt.Errorf("pollAndSign %s: build intent: %w", spawnID, err)
	}

	// NodeAccessToken is intentionally empty in dev mode: the CP mints a cnf-bearing aud=node token
	// from si.SpkiDer in SubmitIntent when its dev AS key is configured [AM12]. In a production
	// deployment the client would obtain this token from the AS before calling SubmitIntent.
	_, err = ic.SubmitIntent(ctx, connect.NewRequest(&cpv1.SubmitIntentRequest{
		SpawnId: spawnID,
		Intent:  si,
	}))
	if err != nil {
		return fmt.Errorf("pollAndSign %s: SubmitIntent: %w", spawnID, err)
	}
	return nil
}
