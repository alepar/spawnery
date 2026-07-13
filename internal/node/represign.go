package node

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/spawnlet"
)

// represignTimeout bounds how long represignFunc waits for a RepresignArtifactsResponse (sp-mwco.4.3).
// Well inside spawnlet's 5m stagingBudget; materializeByRef's heartbeat keeps the CP stall timer
// resetting while we wait. Overridable per-attacher via represignRequestTimeout (see that field).
const represignTimeout = 30 * time.Second

// represignPending correlates in-flight RepresignArtifactsRequests (by request_id) with their
// CP response. Scoped to ONE attacher/CP connection: created in runOnce, drained by runOnce's
// defer on disconnect (failAll) so a waiter is unblocked immediately instead of burning the full
// represignTimeout.
type represignPending struct {
	mu sync.Mutex
	m  map[string]chan *nodev1.RepresignArtifactsResponse
}

func newRepresignPending() *represignPending {
	return &represignPending{m: map[string]chan *nodev1.RepresignArtifactsResponse{}}
}

// register allocates a buffered-1 waiter channel for requestID. Call BEFORE sending the request so
// a fast response can never race ahead of the registration.
func (p *represignPending) register(requestID string) chan *nodev1.RepresignArtifactsResponse {
	ch := make(chan *nodev1.RepresignArtifactsResponse, 1)
	p.mu.Lock()
	p.m[requestID] = ch
	p.mu.Unlock()
	return ch
}

// release removes requestID's waiter. Always call this once the caller is done waiting (success,
// timeout, or ctx cancellation) so the map doesn't accumulate stale entries.
func (p *represignPending) release(requestID string) {
	p.mu.Lock()
	delete(p.m, requestID)
	p.mu.Unlock()
}

// resolve routes an inbound RepresignArtifactsResponse to its waiter, if any. A response whose
// request_id has no live waiter here — a stale/duplicate reply, or one that arrived on a
// connection other than the one that sent the request — is dropped, not panicked on.
func (p *represignPending) resolve(resp *nodev1.RepresignArtifactsResponse) {
	p.mu.Lock()
	ch, ok := p.m[resp.GetRequestId()]
	p.mu.Unlock()
	if !ok {
		slog.Debug("represign: response for unknown request_id dropped", "request_id", resp.GetRequestId())
		return
	}
	select {
	case ch <- resp:
	default:
	}
}

// failAll closes every currently-registered waiter's channel, unblocking every in-flight
// represignFunc call with an error immediately rather than making it wait out the full
// represignTimeout. Called exactly once per connection, from runOnce's defer, on disconnect.
func (p *represignPending) failAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, ch := range p.m {
		close(ch)
		delete(p.m, id)
	}
}

// represignFunc returns a spawnlet.RepresignFunc bound to spawnID/gen: it asks the CP to mint
// fresh presigned GET URLs for objectKeys over the Attach stream (sp-mwco.4.3 — the node's GET on
// the original presigned URL failed, e.g. TTL expiry) and waits for the response.
//
// RECONNECT MID-FETCH (the behavior this seam commits to): the attacher — and its represignPending
// map — is per-connection; runOnce builds a fresh one on every reconnect. A startSpawn goroutine
// begun on a since-dropped connection keeps referencing the OLD attacher, whose stream is dead.
// So: a re-presign issued after the drop fails immediately on send (dead stream); a re-presign
// already in flight is unblocked by failAll() in runOnce's defer. Either way this closure returns
// an error, spawnlet's fetchWithRetry converts it to a Terminal fetch error for that artifact, and
// startSpawn fails the spawn. No cross-connection delivery is possible — the CP replies on the
// stream it received the request on, and this node's pending map lives on the (now dead) attacher
// that sent it; a response with an unrecognized request_id arriving on any connection is simply
// dropped (see resolve). The CP side needs no reconnect logic of its own: a send failure to a dead
// stream is a no-op there, and the scheduler's in-flight placement is cleaned up by Provision's own
// defer regardless of how Provision itself ends.
func (a *attacher) represignFunc(spawnID string, gen uint64) spawnlet.RepresignFunc {
	return func(ctx context.Context, objectKeys []string) (map[string]string, error) {
		requestID := newRepresignRequestID()
		ch := a.represigns.register(requestID)
		defer a.represigns.release(requestID)

		if err := a.send(&nodev1.NodeMessage{Msg: &nodev1.NodeMessage_RepresignArtifactsRequest{
			RepresignArtifactsRequest: &nodev1.RepresignArtifactsRequest{
				RequestId:  requestID,
				SpawnId:    spawnID,
				Generation: gen,
				ObjectKeys: objectKeys,
			},
		}}); err != nil {
			return nil, fmt.Errorf("represign: send request: %w", err)
		}

		timeout := a.represignRequestTimeout
		if timeout <= 0 {
			timeout = represignTimeout
		}
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		select {
		case resp, ok := <-ch:
			if !ok {
				return nil, errors.New("represign: CP connection lost")
			}
			if resp.GetSpawnId() != spawnID {
				return nil, fmt.Errorf("represign: response spawn_id %q != requested %q", resp.GetSpawnId(), spawnID)
			}
			if resp.GetError() != "" {
				return nil, fmt.Errorf("represign: CP refused: %s", resp.GetError())
			}
			requested := make(map[string]struct{}, len(objectKeys))
			for _, k := range objectKeys {
				requested[k] = struct{}{}
			}
			out := make(map[string]string, len(resp.GetObjects()))
			for _, o := range resp.GetObjects() {
				// Keep only keys that were actually requested — a defensive filter against a CP
				// response that (by bug) carries extra objects. A requested key missing from the
				// response is simply absent from out; spawnlet already treats that as Terminal.
				if _, wanted := requested[o.GetObjectKey()]; wanted {
					out[o.GetObjectKey()] = o.GetPresignedUrl()
				}
			}
			return out, nil
		case <-timer.C:
			return nil, errors.New("represign: timed out waiting for CP response")
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// newRepresignRequestID mints a node-connection-scoped correlation id for a RepresignArtifactsRequest.
func newRepresignRequestID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return "represign-" + hex.EncodeToString(b)
}
