package cp

import (
	"context"
	"log/slog"
	"sync"
	"time"

	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/cp/registry"
	"spawnery/internal/cp/store"
)

// sp-mwco.4.3: re-presign over the Attach stream. This is the FIRST node-initiated
// request/response pair on Attach (every other node->CP message is an unsolicited event or an ack
// of a CP push) — a node whose GET on the original presigned URL failed (TTL expiry) asks the CP
// to mint a fresh one for the same object.
//
// SECURITY (do not omit): the skills bucket is ONE GLOBAL CONTENT-ADDRESSED NAMESPACE with
// GUESSABLE KEYS (anyone who can fetch the same public repo reproduces the sha). Without
// key-scoping, the only check would be "does the spawn belong to this node" — and since a
// self-hosted node with one live spawn can request ANY object_key, that would let it have the CP
// mint 30-min bearer GETs for arbitrary skill objects. handleRepresign therefore INTERSECTS the
// requested object_keys with the spawn's PERSISTED artifact rows (step 5 below) and rejects the
// WHOLE request if any key falls outside that set.

const (
	// maxRepresignKeys bounds object_keys per request to the same per-spawn artifact cap CreateSpawn
	// enforces — a request can never need more keys than the spawn has artifacts.
	maxRepresignKeys = maxArtifactsPerSpawn
	// represignRateWindow / maxRepresignPerWindow bound how often one spawn can ask for re-presigns,
	// so a compromised/buggy node cannot use this as a presign-minting oracle.
	represignRateWindow   = time.Minute
	maxRepresignPerWindow = 8
	// represignHandlerTimeout bounds the async handler's own DB + presign work so a slow store/Garage
	// call cannot pin resources indefinitely; it does NOT block the runNode receive loop either way.
	represignHandlerTimeout = 10 * time.Second
)

// windowCounter is one spawn's fixed-window request count for represignLimiter.
type windowCounter struct {
	windowStart time.Time
	count       int
}

// represignLimiter is a per-spawn fixed-window rate limiter for RepresignArtifactsRequest.
// now is overridable in tests so the window can be advanced deterministically without a real sleep.
type represignLimiter struct {
	mu  sync.Mutex
	m   map[string]windowCounter
	now func() time.Time
}

func newRepresignLimiter() *represignLimiter {
	return &represignLimiter{m: map[string]windowCounter{}, now: time.Now}
}

// allow reports whether spawnID may issue another re-presign request in the current window,
// incrementing its counter if so. Sweeps every window-expired entry on each call so the map never
// grows unbounded with churned spawn ids.
func (l *represignLimiter) allow(spawnID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	for id, wc := range l.m {
		if now.Sub(wc.windowStart) >= represignRateWindow {
			delete(l.m, id)
		}
	}
	wc, ok := l.m[spawnID]
	if !ok {
		wc = windowCounter{windowStart: now}
	}
	if wc.count >= maxRepresignPerWindow {
		l.m[spawnID] = wc
		return false
	}
	wc.count++
	l.m[spawnID] = wc
	return true
}

// resolveRepresignOwner reports whether nodeID is currently authorized to be staging spawnID at
// generation gen. Artifact staging happens strictly BEFORE ACTIVE (inside startSpawn), i.e. before
// Spawns().SetActive ever sets the live container row's node_id — so the scheduler's in-flight
// placement (populated for the duration of Provision) is consulted FIRST, and the bound live
// container (the post-ACTIVE case: a spawn already running that re-fetches an artifact) is the
// fallback. Any other outcome (store error, no row, wrong node, generation mismatch) returns false;
// callers must not distinguish these cases in the response (don't leak whether the spawn exists).
func (s *Server) resolveRepresignOwner(ctx context.Context, nodeID, spawnID string, gen uint64) bool {
	if fs, ok := s.sched.InFlightStart(spawnID); ok {
		return fs.NodeID == nodeID && fs.Gen == gen
	}
	c, ok, err := s.st.Spawns().LiveContainer(ctx, spawnID)
	if err != nil || !ok {
		return false
	}
	return c.NodeID == nodeID && uint64(c.Generation) == gen
}

// handleRepresign is the CP side of sp-mwco.4.3. Dispatched from runNode on its own goroutine (via
// safego.Go) so a slow DB/Garage round trip never blocks the single per-connection receive loop.
// Refusal is ALWAYS a response with Error set — never a stream error, never a panic; the node turns
// any refusal into a Terminal fetch error for the affected artifact(s). The response is
// all-or-nothing: a partially-satisfiable request is refused in full, never partially granted.
func (s *Server) handleRepresign(ctx context.Context, nodeID string, sender registry.NodeSender, req *nodev1.RepresignArtifactsRequest) {
	ctx, cancel := context.WithTimeout(ctx, represignHandlerTimeout)
	defer cancel()

	refuse := func(reason string) {
		slog.Warn("represign: refused", "node", nodeID, "spawn", req.GetSpawnId(), "reason", reason)
		_ = sender.Send(&nodev1.CPMessage{Msg: &nodev1.CPMessage_RepresignArtifactsResponse{
			RepresignArtifactsResponse: &nodev1.RepresignArtifactsResponse{
				RequestId: req.GetRequestId(),
				SpawnId:   req.GetSpawnId(),
				Error:     reason,
			},
		}})
	}

	// 1. Cheap shape validation.
	if nodeID == "" || req.GetRequestId() == "" || req.GetSpawnId() == "" || req.GetGeneration() == 0 || len(req.GetObjectKeys()) == 0 {
		refuse("invalid re-presign request")
		return
	}

	// 2. Bound object_keys length.
	if len(req.GetObjectKeys()) > maxRepresignKeys {
		refuse("too many object keys")
		return
	}

	// 3. Per-spawn rate limit.
	if !s.represigns.allow(req.GetSpawnId()) {
		refuse("re-presign rate limit exceeded")
		return
	}

	// 4. Ownership + generation fence.
	if !s.resolveRepresignOwner(ctx, nodeID, req.GetSpawnId(), req.GetGeneration()) {
		refuse("spawn is not starting on this node")
		return
	}

	// 5. KEY-SCOPING — the security line, DO NOT OMIT: intersect the requested keys with the
	// spawn's own persisted artifact rows. Any requested key outside that set refuses the WHOLE
	// request.
	rows, err := s.st.Spawns().GetArtifacts(ctx, req.GetSpawnId())
	if err != nil {
		refuse("failed to load spawn artifacts")
		return
	}
	byKey := make(map[string]store.Artifact, len(rows))
	for _, a := range rows {
		if a.ObjectKey != "" {
			byKey[a.ObjectKey] = a
		}
	}
	matched := make([]store.Artifact, 0, len(req.GetObjectKeys()))
	seen := make(map[string]struct{}, len(req.GetObjectKeys()))
	for _, key := range req.GetObjectKeys() {
		a, ok := byKey[key]
		if !ok {
			slog.Warn("represign: requested key not bound to spawn", "node", nodeID, "spawn", req.GetSpawnId(), "key", key)
			refuse("object key not bound to this spawn: " + key)
			return
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		matched = append(matched, a)
	}

	// 6. Convert + presign. presignNodeArtifacts runs the sha denylist gate as its first statement
	// (sp-mwco.3.2), so a revoked sha is never re-presigned. statSkillObjects (the HEAD gate) does
	// NOT run here — it already ran at StartSpawn; a since-deleted object surfaces as a node-side
	// 404 Terminal, not a CP-side refusal.
	specs := storeToNodeArtifacts(matched, s.effectiveSkillPlainTarCap())
	if err := s.presignNodeArtifacts(ctx, specs); err != nil {
		refuse(err.Error())
		return
	}

	// 7. Success: one PresignedObject per requested (deduplicated) key.
	objects := make([]*nodev1.PresignedObject, 0, len(specs))
	for _, spec := range specs {
		if spec.Objectref == nil {
			continue
		}
		objects = append(objects, &nodev1.PresignedObject{
			ObjectKey:    spec.Objectref.ObjectKey,
			PresignedUrl: spec.Objectref.PresignedUrl,
		})
	}
	slog.Debug("represign: granted", "node", nodeID, "spawn", req.GetSpawnId(), "keys", len(objects))
	_ = sender.Send(&nodev1.CPMessage{Msg: &nodev1.CPMessage_RepresignArtifactsResponse{
		RepresignArtifactsResponse: &nodev1.RepresignArtifactsResponse{
			RequestId: req.GetRequestId(),
			SpawnId:   req.GetSpawnId(),
			Objects:   objects,
		},
	}})
}
