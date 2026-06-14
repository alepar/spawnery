// Package node implements the spawnlet's CP-attached mode: it dials the CP,
// registers, heartbeats, and services CPMessages by reusing the existing
// spawnlet Manager + per-spawn Pump. It never accepts inbound connections.
package node

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	"connectrpc.com/connect"

	authv1 "spawnery/gen/auth/v1"
	nodev1 "spawnery/gen/node/v1"
	"spawnery/gen/node/v1/nodev1connect"
	"spawnery/internal/agentcaps"
	"spawnery/internal/secrets/subkey"
	"spawnery/internal/spawnlet"
)

// readyTimeout bounds how long startSpawn waits for the agent to answer pump.start's ACP initialize
// handshake before declaring the spawn failed (the standalone readiness probe is folded into that
// handshake). Kept well under the CP scheduler's 60s Provision wait (cmd/spawnery_cp/main.go) so the node
// reports ERROR (with a useful detail) rather than the scheduler timing out. goose boots to ACP-ready
// in ~5s; 30s is generous headroom for a slow node.
const readyTimeout = 30 * time.Second

// controlPostTimeout bounds the node's POST to the per-pod sidecar control endpoint. The sidecar is
// reachable at the pod bridge IP (a short hop), so a few seconds is generous; bounding it keeps a wedged
// sidecar from stalling the SetModel handler.
const controlPostTimeout = 5 * time.Second

// httpDoer is the minimal HTTP surface the SetModel handler needs (satisfied by *http.Client). It is a
// seam so tests can stub the sidecar control endpoint without real network.
type httpDoer interface {
	Do(*http.Request) (*http.Response, error)
}

type Config struct {
	NodeID        string
	CPURL         string // e.g. http://127.0.0.1:8080
	MaxSpawns     uint32
	AgentImage    string
	AgentBinaries []string // binaries this node's image ships (registry keys: goose, opencode, ...)
	NodeClass     string
	NodeOwner     string

	// SubKeys is the node's HPKE sub-key holder (owner-sealed-secrets §1): it generates/re-signs sub-keys
	// with the node cert key, publishes the current SignedSubKey on Register/Heartbeat, and unseals
	// delivered ciphertext via OpenDelivered. nil in insecure/dev mode (no signing identity) — then the
	// node publishes no sub-key and rejects SecretDelivery. Shared across reconnects (it retains private
	// halves), so it lives in Config (one holder per node process), not per-connection.
	SubKeys *subkey.Node

	// Verifier is the A4 intent verifier for StartSpawn and SessionOpen [AC1][AM12].
	// nil = skip verification (dev/insecure default until the verifier is explicitly configured).
	// Set via NewIntentVerifier with AuthModeVerifyLog for NODE_AUTH_MODE=insecure (verify-and-log)
	// or AuthModeEnforced for NODE_AUTH_MODE=enforced.
	Verifier *IntentVerifier
}

// cpStream is the subset of the Connect bidi stream the attacher uses. *connect.BidiStreamForClient
// satisfies it; tests inject a fake to capture sent NodeMessages and drive received CPMessages.
type cpStream interface {
	Send(*nodev1.NodeMessage) error
	Receive() (*nodev1.CPMessage, error)
}

type attacher struct {
	cfg      Config
	mgr      *spawnlet.Manager
	httpc    connect.HTTPClient
	sx       sessionExec     // container-exec boundary for additional-session launch/reap (sp-npxq.3)
	verifier *IntentVerifier // A4 intent verifier; nil = skip (tests + insecure mode without explicit verifier)

	ctrlHTTP httpDoer // POSTs SetModel to the per-pod sidecar control endpoint (injectable for tests)

	mu         sync.Mutex
	pumps      map[sessionKey]*Pump
	tmuxRelays map[sessionKey]*tmuxRelay
	sessions   map[string]*sessionRegistry    // spawn_id -> live session set (roster source of truth)
	pending    map[sessionKey][]pendingClient // attaches that arrived before the pump/relay existed (session STARTING)
	active     uint32

	sendMu sync.Mutex
	stream cpStream

	// subkeysMu guards cfg.SubKeys + lastSubKeyID: Rotate/Current (publish, from the heartbeat loop +
	// Register) and OpenDelivered (from the receive loop) race otherwise — subkey.Node is not internally
	// synchronized.
	subkeysMu    sync.Mutex
	lastSubKeyID string // KeyID of the most recently published sub-key (heartbeat re-publishes only on change)
}

// pendingClient is a client attach that arrived before its session's pump/relay was registered (the
// session was still STARTING — an async launchSession is mid-flight). It is queued under attacher.pending
// and bound when the resource readies (mirrors the CP pending-at-Bind precedent).
type pendingClient struct {
	clientID string
	cursor   int64
}

// Run keeps the node connected to the CP: it (re)dials and serves one connection at a time, backing
// off on failure, until ctx is cancelled. It does NOT exit when the CP is down or drops — the node
// waits for the CP at startup and reconnects after a disconnect (re-registering each time; the CP
// reconciles a returning node). The Manager + its running spawns persist across reconnects.
func Run(ctx context.Context, mgr *spawnlet.Manager, httpc connect.HTTPClient, cfg Config) error {
	// Reap pods leaked by a previous node process before serving — the in-mem store is empty at
	// startup, so every spawnery-managed pod the runtime still has is an orphan.
	if err := mgr.ReapOrphans(ctx); err != nil {
		log.Printf("node: reap orphans at startup: %v", err)
	}
	const minBackoff, maxBackoff = time.Second, 30 * time.Second
	backoff := minBackoff
	for {
		start := time.Now()
		err := runOnce(ctx, mgr, httpc, cfg)
		if ctx.Err() != nil {
			return ctx.Err() // clean shutdown
		}
		if time.Since(start) > maxBackoff {
			backoff = minBackoff // the connection was healthy for a while; reset
		}
		log.Printf("node: CP connection ended (%v); reconnecting in %s", err, backoff)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < maxBackoff {
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}
}

// registerMessage builds the node's Register announcement. Extracted for testability and so the
// node advertises the binaries its image ships (the CP upserts these into the agent-image catalog).
func registerMessage(cfg Config, running []*nodev1.RunningSpawn, signedSubKey []byte) *nodev1.Register {
	return &nodev1.Register{
		NodeId: cfg.NodeID, MaxSpawns: cfg.MaxSpawns, AgentImages: []string{cfg.AgentImage},
		NodeClass: cfg.NodeClass, NodeOwner: cfg.NodeOwner, Binaries: cfg.AgentBinaries,
		Running: running, SignedSubkey: signedSubKey,
	}
}

// runOnce serves a single CP connection: dial + Register + heartbeat + receive loop. It returns when
// the connection ends (stream error) or ctx is cancelled. Everything connection-scoped (heartbeat,
// pump sessions) is tied to connCtx so it stops cleanly when the connection ends.
func runOnce(ctx context.Context, mgr *spawnlet.Manager, httpc connect.HTTPClient, cfg Config) error {
	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	a := &attacher{
		cfg: cfg, mgr: mgr, httpc: httpc,
		verifier:   cfg.Verifier,
		ctrlHTTP:   &http.Client{Timeout: controlPostTimeout},
		sx:         &realSessionExec{mgr: mgr},
		pumps:      map[sessionKey]*Pump{},
		tmuxRelays: map[sessionKey]*tmuxRelay{},
		sessions:   map[string]*sessionRegistry{},
		pending:    map[sessionKey][]pendingClient{},
	}
	client := nodev1connect.NewNodeServiceClient(httpc, cfg.CPURL, connect.WithGRPC())
	a.stream = client.Attach(connCtx)

	if err := a.send(&nodev1.NodeMessage{Msg: &nodev1.NodeMessage_Register{
		Register: registerMessage(cfg, a.runningSpawns(connCtx), a.publishSubKey(time.Now())),
	}}); err != nil {
		return err
	}
	log.Printf("node: connected to CP at %s (id=%s class=%s)", cfg.CPURL, cfg.NodeID, cfg.NodeClass)
	go a.heartbeatLoop(connCtx)

	for {
		msg, err := a.stream.Receive()
		if err != nil {
			return err
		}
		a.handle(connCtx, msg)
	}
}

// runningSpawns maps the Manager's live inventory to proto RunningSpawn (all ACTIVE — the Manager
// only holds running spawns), for the CP reconcile carried on Register/Heartbeat. It also
// populates per-spawn resource metrics for the CP-side evaluators (§6 node-local detectors →
// CP-side reporters):
//   - DeltaSizeBytes: committed delta image size (bytes), 0 when unavailable (no DeltaSize backend)
//   - LastActivityUnixMs: max lastActivity across all pumps/relays for the spawn, epoch-ms (UTC);
//     0 when the spawn has no pumps or relays (e.g. mode=tmux with no active relay)
func (a *attacher) runningSpawns(ctx context.Context) []*nodev1.RunningSpawn {
	inv := a.mgr.RunningInventory()
	out := make([]*nodev1.RunningSpawn, 0, len(inv))
	for _, mp := range inv {
		sz, _ := a.mgr.DeltaSize(ctx, mp.SpawnID)
		out = append(out, &nodev1.RunningSpawn{
			SpawnId:            mp.SpawnID,
			Generation:         mp.Generation,
			Phase:              nodev1.SpawnPhase_ACTIVE,
			DeltaSizeBytes:     sz,
			LastActivityUnixMs: a.lastActivityMs(mp.SpawnID),
		})
	}
	return out
}

// lastActivityMs returns the most recent lastActivity time across all pumps and tmux relays for
// spawnID, expressed as epoch-milliseconds (UTC). Returns 0 when no pump or relay is registered
// for the spawn (the spawn may be mid-launch or tmux-only with no relay yet).
func (a *attacher) lastActivityMs(spawnID string) int64 {
	var latest time.Time
	a.mu.Lock()
	for k, p := range a.pumps {
		if k.spawnID == spawnID {
			if t := p.lastActive(); t.After(latest) {
				latest = t
			}
		}
	}
	for k, r := range a.tmuxRelays {
		if k.spawnID == spawnID {
			if t := r.lastActive(); t.After(latest) {
				latest = t
			}
		}
	}
	a.mu.Unlock()
	if latest.IsZero() {
		return 0
	}
	return latest.UnixMilli()
}

func (a *attacher) send(m *nodev1.NodeMessage) error {
	a.sendMu.Lock()
	defer a.sendMu.Unlock()
	return a.stream.Send(m)
}

func (a *attacher) heartbeatLoop(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.mu.Lock()
			active := a.active
			a.mu.Unlock()
			free := a.cfg.MaxSpawns - active
			_ = a.send(&nodev1.NodeMessage{Msg: &nodev1.NodeMessage_Heartbeat{Heartbeat: &nodev1.Heartbeat{
				ActiveSpawns: active, FreeSlots: free, Running: a.runningSpawns(ctx),
				SignedSubkey: a.rotatedSubKey(time.Now()), // re-publish only when the sub-key just rotated
			}}})
		}
	}
}


func (a *attacher) status(spawnID string, ph nodev1.SpawnPhase, detail string) {
	_ = a.send(&nodev1.NodeMessage{Msg: &nodev1.NodeMessage_Status{Status: &nodev1.SpawnStatus{SpawnId: spawnID, Phase: ph, Detail: detail}}})
}

// statusActive reports the ACTIVE transition. It carries the spawn's resolved base-image
// digest so the CP can pin it on the spawn row (SetBaseImageDigest) — the digest's only
// report-back path; without it cross-node resume pinning is inert (spec §4).
func (a *attacher) statusActive(spawnID, baseImageDigest string) {
	_ = a.send(&nodev1.NodeMessage{Msg: &nodev1.NodeMessage_Status{Status: &nodev1.SpawnStatus{SpawnId: spawnID, Phase: nodev1.SpawnPhase_ACTIVE, BaseImageDigest: baseImageDigest}}})
}

// zeroKey is the map key for a spawn's session #0 (the primary).
func zeroKey(spawnID string) sessionKey {
	return sessionKey{spawnID: spawnID, sessionID: SessionZeroID}
}

// moshTmuxName / acpTmuxName are the deterministic tmux session names for an additional session, so
// reaping needs no extra registry field. Session #0 keeps its legacy name "spawn" (see startSpawn).
func moshTmuxName(sessionID string) string { return "spawn-" + sessionID }
func acpTmuxName(sessionID string) string  { return "acp-" + sessionID }

// sessionStatus reports a single session's lifecycle state to the CP (alongside the roster).
func (a *attacher) sessionStatus(spawnID, sessionID string, st nodev1.SessionState, detail string) {
	_ = a.send(&nodev1.NodeMessage{Msg: &nodev1.NodeMessage_SessionStatus{SessionStatus: &nodev1.SessionStatus{
		SpawnId: spawnID, SessionId: sessionID, State: st, Detail: detail,
	}}})
}

// sid defaults an empty wire session id to session #0 (backward compat with single-session CPs).
func sid(s string) string {
	if s == "" {
		return SessionZeroID
	}
	return s
}

// emitRoster sends spawnID's current session set to the CP. Call after any registry membership change.
func (a *attacher) emitRoster(spawnID string) {
	a.mu.Lock()
	reg := a.sessions[spawnID]
	a.mu.Unlock()
	if reg == nil {
		return
	}
	_ = a.send(&nodev1.NodeMessage{Msg: &nodev1.NodeMessage_Roster{Roster: &nodev1.SessionRoster{
		SpawnId: spawnID, Sessions: reg.snapshot(),
	}}})
}

func (a *attacher) handle(ctx context.Context, msg *nodev1.CPMessage) {
	switch m := msg.Msg.(type) {
	case *nodev1.CPMessage_Start:
		go a.startSpawn(ctx, m.Start)
	case *nodev1.CPMessage_Stop:
		if a.staleGen(m.Stop.SpawnId, m.Stop.Generation) {
			return
		}
		a.stopSpawn(ctx, m.Stop.SpawnId)
	case *nodev1.CPMessage_Open:
		// A4 SessionOpen verification [AC1][AM12]. Run BEFORE attaching the client so a forged/replayed
		// open is blocked in enforced mode. In verify-and-log mode (NODE_AUTH_MODE=insecure) failures are
		// logged but the attach proceeds. Generation is read from the live spawn (mgr.SpawnGeneration) so
		// the verifier can check correspondence even if the CP omits it from the SessionOpen wire message.
		if a.verifier != nil {
			spawnID := m.Open.GetSpawnId()
			gen, _ := a.mgr.SpawnGeneration(spawnID)
			fields := OpenFields{
				SpawnID:       spawnID,
				Generation:    gen,
				SessionID:     sid(m.Open.GetSessionId()),
				AssertedOwner: m.Open.GetAssertedOwner(),
			}
			if nack, detail := a.verifier.VerifyOpen(m.Open.GetAuth(), fields); nack != "" {
				log.Printf("SessionOpen %s/%s: intent NACK %s: %s (client not attached)",
					spawnID, sid(m.Open.GetSessionId()), nack, detail)
				return // enforced: drop the open; verify-and-log never returns a nack
			}
		}
		a.attachClient(m.Open.SpawnId, sid(m.Open.SessionId), m.Open.ClientId, m.Open.Cursor)
	case *nodev1.CPMessage_Close:
		a.detachClient(m.Close.SpawnId, sid(m.Close.SessionId), m.Close.ClientId)
	case *nodev1.CPMessage_Frame:
		a.fromClient(m.Frame.SpawnId, sid(m.Frame.SessionId), m.Frame.ClientId, m.Frame.Data)
	case *nodev1.CPMessage_CreateSession:
		if a.staleGen(m.CreateSession.SpawnId, m.CreateSession.Generation) {
			return
		}
		a.createSession(ctx, m.CreateSession)
	case *nodev1.CPMessage_CloseSession:
		if a.staleGen(m.CloseSession.SpawnId, m.CloseSession.Generation) {
			return
		}
		a.closeSession(ctx, m.CloseSession)
	case *nodev1.CPMessage_SetModel:
		if a.staleGen(m.SetModel.SpawnId, m.SetModel.Generation) {
			return // stale generation: a newer pod exists; the CP reconciler re-pushes. Drop (matches Stop/CreateSession).
		}
		// Async like startSpawn/launchSession: a slow/wedged sidecar POST (up to controlPostTimeout) must
		// not block the single per-connection Receive loop and stall all other inbound CPMessages. The
		// generation fence above stays SYNCHRONOUS (reads the live gen here). The reply goes via a.send,
		// which is sendMu-guarded — safe from this goroutine (matches the other async-dispatch handlers).
		go a.setModel(ctx, m.SetModel)
	case *nodev1.CPMessage_Suspend:
		if a.staleGen(m.Suspend.SpawnId, m.Suspend.Generation) {
			return // stale generation: a newer pod exists; drop (matches Stop/SetModel). Keep the fence SYNCHRONOUS.
		}
		// Async like setModel/startSpawn: the suspend persists mounts (a journal final snapshot can
		// block) + tears the pod down, so it must not stall the single per-connection Receive loop.
		go a.suspendSpawn(ctx, m.Suspend)
	case *nodev1.CPMessage_SecretDelivery:
		if a.staleGen(m.SecretDelivery.SpawnId, m.SecretDelivery.Generation) {
			return // stale generation: a newer pod exists; the owner re-seals to the current episode. Drop.
		}
		// Async: unsealing + writing the tmpfs files must not stall the single per-connection Receive loop.
		// The generation fence above stays SYNCHRONOUS (reads the live gen here), matching Stop/Suspend.
		go a.handleSecretDelivery(m.SecretDelivery)
	case *nodev1.CPMessage_SealJournalKey:
		if a.staleGen(m.SealJournalKey.SpawnId, m.SealJournalKey.Generation) {
			return // stale generation: drop (matches Stop/Suspend/SecretDelivery fence discipline)
		}
		// Async: crypto sealing must not stall the single per-connection Receive loop.
		go a.sealJournalKey(ctx, m.SealJournalKey)
	default:
	}
}

// staleGen reports whether a control message carrying generation gen targets a superseded container
// and must be ignored: the spawn is still tracked with a HIGHER generation (it was recreated since the
// message was sent). gen 0 (standalone / unset) is never fenced.
func (a *attacher) staleGen(spawnID string, gen uint64) bool {
	if gen == 0 {
		return false
	}
	live, ok := a.mgr.SpawnGeneration(spawnID)
	if ok && gen < live {
		log.Printf("fencing stale message for spawn=%s gen=%d (live gen=%d)", spawnID, gen, live)
		return true
	}
	return false
}

func rootfsArtifactsFromProto(in []*nodev1.RootfsArtifact) []spawnlet.RootfsArtifact {
	if len(in) == 0 {
		return nil
	}
	out := make([]spawnlet.RootfsArtifact, 0, len(in))
	for _, a := range in {
		if a == nil {
			continue
		}
		out = append(out, spawnlet.RootfsArtifact{
			ArtifactID:       a.GetArtifactId(),
			Generation:       a.GetGeneration(),
			Sequence:         int(a.GetSequence()),
			BaseImageDigest:  a.GetBaseImageDigest(),
			Format:           a.GetFormat(),
			ContentDigest:    a.GetContentDigest(),
			UncompressedSize: a.GetUncompressedSize(),
			ProducerNodeID:   a.GetProducerNodeId(),
			ProducerRuntime:  a.GetProducerRuntime(),
		})
	}
	return out
}

func rootfsArtifactsToProto(in []spawnlet.RootfsArtifact) []*nodev1.RootfsArtifact {
	if len(in) == 0 {
		return nil
	}
	out := make([]*nodev1.RootfsArtifact, 0, len(in))
	for _, a := range in {
		out = append(out, &nodev1.RootfsArtifact{
			ArtifactId:       a.ArtifactID,
			Generation:       a.Generation,
			Sequence:         int32(a.Sequence),
			BaseImageDigest:  a.BaseImageDigest,
			Format:           a.Format,
			ContentDigest:    a.ContentDigest,
			UncompressedSize: a.UncompressedSize,
			ProducerNodeId:   a.ProducerNodeID,
			ProducerRuntime:  a.ProducerRuntime,
		})
	}
	return out
}

func (a *attacher) startSpawn(ctx context.Context, st *nodev1.StartSpawn) {
	a.status(st.SpawnId, nodev1.SpawnPhase_STARTING, "")

	// A4 intent verification [AC1][AM12]. Verify BEFORE creating the container so a
	// forged/replayed StartSpawn is rejected at the gate. In verify-and-log mode (NODE_AUTH_MODE=insecure)
	// failures are logged but execution proceeds; in enforced mode a NACK returns ERROR status.
	if a.verifier != nil {
		mounts := make([]*authv1.MountRef, 0, len(st.GetMounts()))
		for _, m := range st.GetMounts() {
			mounts = append(mounts, &authv1.MountRef{Name: m.GetName(), BackendUri: m.GetBackendUri()})
		}
		fields := StartFields{
			SpawnID:       st.GetSpawnId(),
			Generation:    st.GetGeneration(),
			AppRef:        st.GetAppRef(),
			Image:         st.GetImage(),
			Model:         st.GetModel(),
			DataRef:       st.GetDataRef(),
			Mounts:        mounts,
			AssertedOwner: st.GetAssertedOwner(),
		}
		if nack, detail := a.verifier.VerifyStart(st.GetAuth(), fields); nack != "" {
			log.Printf("startSpawn %s: intent NACK %s: %s", st.SpawnId, nack, detail)
			a.status(st.SpawnId, nodev1.SpawnPhase_ERROR, string(nack)+": "+detail)
			return
		}
	}

	// Emit resume progress at key phase boundaries so the CP stall detector can reset (sp-u53.7.2).
	// The CP's resumeWaiters drops these if no waiter is registered (fresh creates have no waiter).
	a.resumeProgress(st.SpawnId, st.Generation, "starting", "creating containers")
	sp, err := a.mgr.CreateWithSelection(ctx, st.SpawnId, st.AppRef, st.Model, st.Name, st.AppId, st.Generation,
		spawnlet.AgentSelection{
			Image:                  st.Image,
			RunnableID:             st.RunnableId,
			Mode:                   st.Mode,
			BaseImageDigest:        st.GetBaseImageDigest(),
			RootfsSourceGeneration: st.GetRootfsSourceGeneration(),
			RootfsArtifacts:        rootfsArtifactsFromProto(st.GetRootfsArtifacts()),
		})
	if err != nil {
		logErr("startSpawn "+st.SpawnId, err)
		a.status(st.SpawnId, nodev1.SpawnPhase_ERROR, err.Error())
		return
	}
	a.resumeProgress(st.SpawnId, st.Generation, "containers_ready", "containers created")
	// tmux-mode spawns: register a raw-PTY relay per client (no ACP handshake, no Pump).
	// Goes ACTIVE immediately after the relay is registered.
	if st.Mode == string(agentcaps.ModeTmux) {
		relay := newTmuxRelay(a.mgr.TmuxAttachArgv(sp.AgentID, "spawn"), func(clientID string, data []byte) error {
			return a.send(&nodev1.NodeMessage{Msg: &nodev1.NodeMessage_Frame{Frame: &nodev1.Frame{
				SpawnId: st.SpawnId, SessionId: SessionZeroID, ClientId: clientID, Data: data,
			}}})
		})
		a.mu.Lock()
		a.tmuxRelays[zeroKey(st.SpawnId)] = relay
		reg := newSessionRegistry(st.SpawnId)
		reg.register(&sessionEntry{
			id: SessionZeroID, transport: transportForMode(st.Mode), runnable: st.RunnableId,
			state: nodev1.SessionState_SESSION_STATE_ACTIVE, endpoint: "spawn", pinned: true,
		})
		a.sessions[st.SpawnId] = reg
		a.active++
		a.mu.Unlock()
		a.emitRoster(st.SpawnId)
		a.statusActive(st.SpawnId, sp.BaseImageDigest)
		return
	}
	att, err := a.mgr.Attach(ctx, sp)
	if err != nil {
		logErr("startSpawn attach "+st.SpawnId, err)
		_ = a.mgr.Stop(ctx, st.SpawnId)
		a.status(st.SpawnId, nodev1.SpawnPhase_ERROR, err.Error())
		return
	}
	p := newPump(att.Stdin, att.Stdout)
	p.closeFn = att.Close
	p.exitFn = func() { // goose died after going active -> ERROR + reclaim (so capacity accounting stays honest)
		a.status(st.SpawnId, nodev1.SpawnPhase_ERROR, "agent exited")
		a.mu.Lock()
		mine := a.pumps[zeroKey(st.SpawnId)] == p // only clean up if we're still the registered pump (not replaced/stopped)
		if mine {
			delete(a.pumps, zeroKey(st.SpawnId))
			delete(a.sessions, st.SpawnId)
			if a.active > 0 {
				a.active--
			}
		}
		a.mu.Unlock()
		if mine {
			_ = a.mgr.Stop(context.WithoutCancel(ctx), st.SpawnId) // reclaim the crashed container
		}
	}
	a.mu.Lock()
	a.pumps[zeroKey(st.SpawnId)] = p
	a.mu.Unlock()
	a.resumeProgress(st.SpawnId, st.Generation, "attaching", "awaiting agent ACP readiness")
	if err := p.start(ctx, readyTimeout); err != nil {
		logErr("startSpawn "+st.SpawnId+": agent not ready", err)
		p.stop()
		a.mu.Lock()
		delete(a.pumps, zeroKey(st.SpawnId))
		a.mu.Unlock()
		_ = a.mgr.Stop(ctx, st.SpawnId)
		a.status(st.SpawnId, nodev1.SpawnPhase_ERROR, err.Error())
		return
	}
	a.mu.Lock()
	reg := newSessionRegistry(st.SpawnId)
	reg.register(&sessionEntry{
		id: SessionZeroID, transport: transportForMode(st.Mode), runnable: st.RunnableId,
		state: nodev1.SessionState_SESSION_STATE_ACTIVE, endpoint: "7000", pinned: true,
	})
	a.sessions[st.SpawnId] = reg
	a.active++
	a.mu.Unlock()
	a.emitRoster(st.SpawnId)
	a.statusActive(st.SpawnId, sp.BaseImageDigest)
}

// reapSessions removes every session of spawnID — session #0 plus any additional sessions 1..N
// (sp-npxq.3) — from the attacher's registries under the lock, and returns the pumps + tmux relays
// to stop OUTSIDE the lock. The container itself is torn down by mgr.Stop/Suspend, so additional acp
// tmux wrappers + their pool ports die with it (the whole registry — including its ports map — is
// dropped here); no per-session KillTmux is needed. Shared by stopSpawn and suspendSpawn.
func (a *attacher) reapSessions(spawnID string) ([]*Pump, []*tmuxRelay) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var ps []*Pump
	for k, p := range a.pumps {
		if k.spawnID == spawnID {
			ps = append(ps, p)
			delete(a.pumps, k)
		}
	}
	var relays []*tmuxRelay
	for k, r := range a.tmuxRelays {
		if k.spawnID == spawnID {
			relays = append(relays, r)
			delete(a.tmuxRelays, k)
		}
	}
	for k := range a.pending {
		if k.spawnID == spawnID {
			delete(a.pending, k) // spawn gone: drop any pended attaches (their WS will error)
		}
	}
	delete(a.sessions, spawnID)
	return ps, relays
}

// releaseSlot frees the spawn's single capacity slot. Capacity is one slot per SPAWN, not per
// session: additional sessions never incremented a.active (plan decision 7), so this releases exactly
// the spawn's single slot. Shared by stopSpawn and suspendSpawn.
func (a *attacher) releaseSlot() {
	a.mu.Lock()
	if a.active > 0 {
		a.active--
	}
	a.mu.Unlock()
}

func (a *attacher) stopSpawn(ctx context.Context, spawnID string) {
	ps, relays := a.reapSessions(spawnID)
	for _, p := range ps {
		p.stop()
	}
	for _, r := range relays {
		r.stop()
	}
	if err := a.mgr.Stop(ctx, spawnID); err != nil {
		logErr("stopSpawn "+spawnID, err)
	}
	a.releaseSlot()
	a.status(spawnID, nodev1.SpawnPhase_STOPPED, "")
}

// suspendProgress sends a SuspendProgress message to the CP on the node's Attach stream.
// Called at each phase boundary of SnapshotForSuspend/FinishSuspend so the CP stall detector
// can reset its timer (sp-u53.7.2). markers is nil except when partial mount markers are available.
func (a *attacher) suspendProgress(spawnID string, gen uint64, phase, detail string, markers map[string]string) {
	mm := make([]*nodev1.MountMarker, 0, len(markers))
	for k, v := range markers {
		mm = append(mm, &nodev1.MountMarker{Name: k, Marker: v})
	}
	_ = a.send(&nodev1.NodeMessage{Msg: &nodev1.NodeMessage_SuspendProgress{
		SuspendProgress: &nodev1.SuspendProgress{
			SpawnId: spawnID, Generation: gen, Phase: phase, Detail: detail, Markers: mm,
		},
	}})
}

// resumeProgress sends a ResumeProgress message to the CP on the node's Attach stream.
// Called at key phase boundaries during startSpawn so the CP resume stall detector can reset
// its timer (sp-u53.7.2). Only meaningful for resumes (rootfs restore path); ignored for fresh creates.
func (a *attacher) resumeProgress(spawnID string, gen uint64, phase, detail string) {
	_ = a.send(&nodev1.NodeMessage{Msg: &nodev1.NodeMessage_ResumeProgress{
		ResumeProgress: &nodev1.ResumeProgress{
			SpawnId: spawnID, Generation: gen, Phase: phase, Detail: detail,
		},
	}})
}

// suspendSpawn persists the spawn's mounts and tears the pod down, using a fail-closed gate->reap->finish
// sequence per spec §5:
//
//  1. GATE (sessions/pumps still live): SnapshotForSuspend takes the final journal snapshot while the
//     agent is still reachable. On FAILURE: emit SuspendComplete{Error} and status ACTIVE, then return
//     without touching sessions or releasing the capacity slot — the spawn keeps running.
//  2. On SUCCESS: reap sessions (reapSessions + p.stop/r.stop), then FinishSuspend to tear down the pod.
//  3. releaseSlot, emit SuspendComplete{Markers, RootfsArtifacts} (Markers from gate result, RootfsArtifacts
//     from finish result), status SUSPENDED.
//
// Mount markers MUST come from the gate (SnapshotForSuspend); FinishSuspend intentionally returns empty
// MountMarkers. Generation fencing is done by the caller (handle). Runs on its own goroutine.
func (a *attacher) suspendSpawn(ctx context.Context, m *nodev1.Suspend) {
	spawnID := m.SpawnId
	gen := m.Generation

	// progressFn relays each phase boundary from SnapshotForSuspend/FinishSuspend to the CP's
	// SuspendProgress wire message so the CP stall detector can reset its timer (sp-u53.7.2).
	progressFn := func(phase, detail string) {
		a.suspendProgress(spawnID, gen, phase, detail, nil)
	}

	// Step 1: gate — snapshot while sessions/pumps are still live.
	gate, err := a.mgr.SnapshotForSuspend(ctx, spawnID, progressFn)
	if err != nil {
		logErr("suspendSpawn gate "+spawnID, err)
		_ = a.send(&nodev1.NodeMessage{Msg: &nodev1.NodeMessage_SuspendComplete{SuspendComplete: &nodev1.SuspendComplete{
			SpawnId: spawnID, Generation: m.Generation, Error: err.Error(),
		}}})
		a.status(spawnID, nodev1.SpawnPhase_ACTIVE, err.Error())
		return
	}

	// Step 2: gate succeeded — reap sessions then finish suspend teardown.
	ps, relays := a.reapSessions(spawnID)
	for _, p := range ps {
		p.stop()
	}
	for _, r := range relays {
		r.stop()
	}
	res, err := a.mgr.FinishSuspend(ctx, spawnID, m.GetCaptureRootfsArtifact(), progressFn)
	if err != nil {
		logErr("suspendSpawn finish "+spawnID, err)
		// Sessions were already reaped above; release the capacity slot before returning so
		// the node does not permanently hold a slot for a spawn that is no longer tracked.
		a.releaseSlot()
		a.status(spawnID, nodev1.SpawnPhase_ERROR, err.Error())
		return
	}

	// Step 3: success — markers from gate, rootfs artifacts from finish.
	a.releaseSlot()
	mm := make([]*nodev1.MountMarker, 0, len(gate.MountMarkers))
	for name, marker := range gate.MountMarkers {
		mm = append(mm, &nodev1.MountMarker{Name: name, Marker: marker})
	}
	_ = a.send(&nodev1.NodeMessage{Msg: &nodev1.NodeMessage_SuspendComplete{SuspendComplete: &nodev1.SuspendComplete{
		SpawnId: spawnID, Generation: m.Generation, Markers: mm, RootfsArtifacts: rootfsArtifactsToProto(res.RootfsArtifacts),
	}}})
	a.status(spawnID, nodev1.SpawnPhase_SUSPENDED, "")
}

// setModel applies a CP SetModel to the running pod by POSTing the new model to the per-pod sidecar
// control endpoint (bearer-token authed). It always replies SetModelResult on the Attach stream, echoing
// the request_id so the CP can correlate the ack. Generation fencing is done by the caller (handle).
func (a *attacher) setModel(ctx context.Context, sm *nodev1.SetModel) {
	reply := func(ok bool, detail string) {
		_ = a.send(&nodev1.NodeMessage{Msg: &nodev1.NodeMessage_SetModelResult{SetModelResult: &nodev1.SetModelResult{
			SpawnId: sm.SpawnId, Ok: ok, Detail: detail, RequestId: sm.RequestId,
		}}})
	}

	sp, ok := a.mgr.Store().Get(sm.SpawnId)
	if !ok {
		reply(false, "unknown spawn")
		return
	}
	if sp.ControlURL == "" {
		reply(false, "no sidecar control endpoint (pod has no IP)")
		return
	}

	body, err := json.Marshal(struct {
		Model string `json:"model"`
	}{Model: sm.Model})
	if err != nil {
		reply(false, "marshal control body: "+err.Error())
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sp.ControlURL, bytes.NewReader(body))
	if err != nil {
		reply(false, "build control request: "+err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+sp.ControlToken)

	resp, err := a.ctrlHTTP.Do(req)
	if err != nil {
		reply(false, "sidecar control POST: "+err.Error())
		return
	}
	defer func() { _, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10)); _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		reply(false, fmt.Sprintf("sidecar control returned %d", resp.StatusCode))
		return
	}
	reply(true, "")
}

// frameSenderFor builds the per-client send closure that relays a pump frame line to the CP. Shared by
// attachClient and the pending-attach drain so a client bound late (after its pump readied) gets an
// identical sender.
func (a *attacher) frameSenderFor(spawnID, sessionID, clientID string) frameSender {
	return func(line []byte) error {
		return a.send(&nodev1.NodeMessage{Msg: &nodev1.NodeMessage_Frame{Frame: &nodev1.Frame{
			SpawnId: spawnID, SessionId: sessionID, ClientId: clientID, Data: append([]byte(nil), line...),
		}}})
	}
}

func (a *attacher) attachClient(spawnID, sessionID, clientID string, cursor int64) {
	k := sessionKey{spawnID, sessionID}
	a.mu.Lock()
	relay := a.tmuxRelays[k]
	p := a.pumps[k]
	if relay == nil && p == nil {
		// Neither resource is registered yet. If the session EXISTS in the registry it is still STARTING (an
		// async launchSession is mid-flight): PEND this attach so it binds when the pump/relay readies
		// (launchACPSession / the mosh branch drain pending under a.mu right after registering). Dropping it
		// here is the multi-session attach-race bug. A genuinely unknown session (not in the registry) is NOT
		// pended — that would queue forever; keep the existing warn/drop.
		if reg := a.sessions[spawnID]; reg != nil {
			if _, ok := reg.get(sessionID); ok {
				if a.pending == nil {
					a.pending = map[sessionKey][]pendingClient{}
				}
				a.pending[k] = append(a.pending[k], pendingClient{clientID: clientID, cursor: cursor})
				a.mu.Unlock()
				return
			}
		}
		a.mu.Unlock()
		log.Printf("warn: attachClient: no pump for %s/%s", spawnID, sessionID)
		return
	}
	a.mu.Unlock()
	if relay != nil {
		if err := relay.attach(context.Background(), clientID); err != nil {
			log.Printf("tmux attach %s/%s/%s: %v", spawnID, sessionID, clientID, err)
		}
		return
	}
	p.attachClient(clientID, cursor, a.frameSenderFor(spawnID, sessionID, clientID))
}

// takePending removes and returns the queued attaches for key k. Caller MUST hold a.mu. Callers bind the
// returned attaches AFTER releasing a.mu (never call p.attachClient/relay.attach under a.mu).
func (a *attacher) takePending(k sessionKey) []pendingClient {
	pend := a.pending[k]
	delete(a.pending, k)
	return pend
}

// removePending drops any queued attach for clientID under key k (a client that disconnected before its
// resource readied must not be bound to a now-phantom pump/relay). Caller MUST hold a.mu.
func (a *attacher) removePending(k sessionKey, clientID string) {
	pend := a.pending[k]
	if len(pend) == 0 {
		return
	}
	out := pend[:0]
	for _, pc := range pend {
		if pc.clientID != clientID {
			out = append(out, pc)
		}
	}
	if len(out) == 0 {
		delete(a.pending, k)
		return
	}
	a.pending[k] = out
}

func (a *attacher) detachClient(spawnID, sessionID, clientID string) {
	k := sessionKey{spawnID, sessionID}
	a.mu.Lock()
	relay := a.tmuxRelays[k]
	p := a.pumps[k]
	a.removePending(k, clientID) // a client that detaches while still STARTING must not be bound at ready
	a.mu.Unlock()
	if relay != nil {
		relay.detach(clientID)
		return
	}
	if p != nil {
		p.detachClient(clientID)
	}
}

func (a *attacher) fromClient(spawnID, sessionID, clientID string, data []byte) {
	k := sessionKey{spawnID, sessionID}
	a.mu.Lock()
	relay := a.tmuxRelays[k]
	p := a.pumps[k]
	a.mu.Unlock()
	if relay != nil {
		relay.fromClient(clientID, data)
		return
	}
	if p != nil {
		p.fromClient(clientID, data)
	}
}

// createSession reserves a session id (and, for acp, a pool port) synchronously on the receive
// goroutine, emits a STARTING roster, then launches the session asynchronously (so a slow docker exec
// + ACP handshake never blocks the control channel). Reservation-before-async keeps id/port unique
// without a separate lock (plan decision 5).
func (a *attacher) createSession(ctx context.Context, m *nodev1.CreateSession) {
	a.mu.Lock()
	reg := a.sessions[m.SpawnId]
	a.mu.Unlock()
	if reg == nil {
		log.Printf("warn: CreateSession for unknown spawn %s", m.SpawnId)
		return
	}

	id := reg.allocID()
	e := &sessionEntry{id: id, transport: m.Transport, runnable: m.Runnable, state: nodev1.SessionState_SESSION_STATE_STARTING}

	if m.Transport == nodev1.SessionTransport_SESSION_TRANSPORT_ACP {
		port, ok := reg.allocPort(id)
		if !ok {
			log.Printf("rejecting acp session for %s: port pool exhausted", m.SpawnId)
			a.sessionStatus(m.SpawnId, "", nodev1.SessionState_SESSION_STATE_ERROR, "acp port pool exhausted")
			return
		}
		e.endpoint = strconv.Itoa(port)
	} else {
		e.endpoint = moshTmuxName(id)
	}
	reg.register(e)
	a.emitRoster(m.SpawnId)
	go a.launchSession(ctx, m.SpawnId, e)
}

// launchSession performs the async launch of a reserved session and flips it ACTIVE, or reaps the
// reservation on failure. Runs on its own goroutine; re-checks the entry to cope with a CloseSession
// racing in mid-launch.
func (a *attacher) launchSession(ctx context.Context, spawnID string, e *sessionEntry) {
	a.mu.Lock()
	reg := a.sessions[spawnID]
	a.mu.Unlock()
	if reg == nil {
		return // spawn stopped under us
	}

	switch e.transport {
	case nodev1.SessionTransport_SESSION_TRANSPORT_MOSH:
		if err := a.sx.LaunchMosh(ctx, spawnID, e.runnable, e.endpoint); err != nil {
			a.failSession(spawnID, reg, e, "launch tmux: "+err.Error())
			return
		}
		argv, err := a.sx.MoshAttachArgv(spawnID, e.endpoint)
		if err != nil {
			// LaunchMosh already created the spawn-<id> tmux; tear it down so a failed attach doesn't
			// leave an orphaned tmux session (endpoint == mosh tmux name).
			_ = a.sx.KillTmux(ctx, spawnID, e.endpoint)
			a.failSession(spawnID, reg, e, "attach argv: "+err.Error())
			return
		}
		sessID := e.id
		relay := newTmuxRelay(argv, func(clientID string, data []byte) error {
			return a.send(&nodev1.NodeMessage{Msg: &nodev1.NodeMessage_Frame{Frame: &nodev1.Frame{
				SpawnId: spawnID, SessionId: sessID, ClientId: clientID, Data: data,
			}}})
		})
		k := sessionKey{spawnID, e.id}
		a.mu.Lock()
		_, live := reg.get(e.id)
		if !live { // closed mid-launch: undo
			a.takePending(k) // session closed: drop any pended attaches (their WS will error)
			a.mu.Unlock()
			relay.stop()
			_ = a.sx.KillTmux(ctx, spawnID, e.endpoint)
			return
		}
		a.tmuxRelays[k] = relay
		pend := a.takePending(k) // bind attaches that arrived while this session was STARTING (after unlock)
		a.mu.Unlock()
		for _, pc := range pend {
			if err := relay.attach(context.Background(), pc.clientID); err != nil {
				log.Printf("tmux attach %s/%s/%s: %v", spawnID, e.id, pc.clientID, err)
			}
		}
		reg.setState(e.id, nodev1.SessionState_SESSION_STATE_ACTIVE)
		a.emitRoster(spawnID)
		a.sessionStatus(spawnID, e.id, nodev1.SessionState_SESSION_STATE_ACTIVE, "")

	case nodev1.SessionTransport_SESSION_TRANSPORT_ACP:
		a.launchACPSession(ctx, spawnID, reg, e)
	}
}

// launchACPSession launches an additional acp session: start the tmux-wrapped acp launcher on the
// reserved port, dial it, run an Nth Pump, then flip ACTIVE. The session-N Pump has NO exitFn (server
// death does not reclaim the container — reap is on explicit close only, plan decision 7).
func (a *attacher) launchACPSession(ctx context.Context, spawnID string, reg *sessionRegistry, e *sessionEntry) {
	port, _ := strconv.Atoi(e.endpoint)
	tmuxName := acpTmuxName(e.id)
	if err := a.sx.LaunchACP(ctx, spawnID, e.runnable, tmuxName, port); err != nil {
		a.failSession(spawnID, reg, e, "launch acp: "+err.Error())
		return
	}
	att, err := a.sx.DialACP(ctx, spawnID, port)
	if err != nil {
		_ = a.sx.KillTmux(ctx, spawnID, tmuxName)
		a.failSession(spawnID, reg, e, "dial acp: "+err.Error())
		return
	}
	p := newPump(att.Stdin, att.Stdout)
	p.closeFn = att.Close
	if err := p.start(ctx, readyTimeout); err != nil {
		p.stop()
		_ = a.sx.KillTmux(ctx, spawnID, tmuxName)
		a.failSession(spawnID, reg, e, "acp not ready: "+err.Error())
		return
	}
	k := sessionKey{spawnID, e.id}
	a.mu.Lock()
	_, live := reg.get(e.id)
	if !live { // closed mid-launch: undo
		a.takePending(k) // session closed: drop any pended attaches (their WS will error)
		a.mu.Unlock()
		p.stop()
		_ = a.sx.KillTmux(ctx, spawnID, tmuxName)
		reg.freePort(port, e.id) // ownership-checked: no-op if CloseSession already freed/realloced it
		return
	}
	a.pumps[k] = p
	pend := a.takePending(k) // bind attaches that arrived while this session was STARTING (after unlock)
	a.mu.Unlock()
	for _, pc := range pend {
		p.attachClient(pc.clientID, pc.cursor, a.frameSenderFor(spawnID, e.id, pc.clientID))
	}
	reg.setState(e.id, nodev1.SessionState_SESSION_STATE_ACTIVE)
	a.emitRoster(spawnID)
	a.sessionStatus(spawnID, e.id, nodev1.SessionState_SESSION_STATE_ACTIVE, "")
}

// failSession reaps a session that failed to launch: free its acp port (if any), drop the entry, emit
// the updated roster + an ERROR SessionStatus.
func (a *attacher) failSession(spawnID string, reg *sessionRegistry, e *sessionEntry, detail string) {
	logErr("launchSession "+spawnID+"/"+e.id, fmt.Errorf("%s", detail))
	// Remove the registry entry and drain pended attaches in ONE a.mu section, so the "stop pending"
	// signal (reg.get -> !ok for a concurrent attachClient) is observable atomically with the drain.
	// Otherwise a racing attachClient could reg.get -> ok and pend AFTER the drain, stranding the entry.
	// Lock order a.mu -> reg.mu matches attachClient and the launch good-path (no inversion); reg.remove
	// only touches reg.mu and never calls back into the attacher. freePort stays OUTSIDE a.mu (its
	// ownership-checked free is idempotent, and its test-only onFreePort hook must not run under a.mu).
	a.mu.Lock()
	reg.remove(e.id)
	a.takePending(sessionKey{spawnID, e.id}) // launch failed: drop any pended attaches (their WS will error)
	a.mu.Unlock()
	if e.transport == nodev1.SessionTransport_SESSION_TRANSPORT_ACP {
		if p, err := strconv.Atoi(e.endpoint); err == nil {
			reg.freePort(p, e.id)
		}
	}
	a.emitRoster(spawnID)
	a.sessionStatus(spawnID, e.id, nodev1.SessionState_SESSION_STATE_ERROR, detail)
}

// closeSession reaps exactly one session, leaving the container (and other sessions) running. acp:
// stop its Pump, kill its tmux wrapper, free its pool port. mosh: tear down its relay, kill its tmux
// session. Session #0 is pinned (no-op). A transient client disconnect does NOT route here, so reap is
// only on an explicit CloseSession (spec §4/§6).
func (a *attacher) closeSession(ctx context.Context, m *nodev1.CloseSession) {
	a.mu.Lock()
	reg := a.sessions[m.SpawnId]
	a.mu.Unlock()
	if reg == nil {
		return
	}
	e, ok := reg.get(m.SessionId)
	if !ok {
		return
	}
	if e.pinned {
		log.Printf("ignoring CloseSession for pinned session %s/%s", m.SpawnId, m.SessionId)
		return
	}

	// Remove the registry entry FIRST — before the pump/relay teardown — but inside the SAME a.mu section
	// as that teardown (and drain any pended attaches for the key), so the remove is atomic with the
	// teardown and matches failSession's discipline. This closes the close-races-launch window: an async
	// launchSession re-checks reg liveness under a.mu before registering its pump, so with
	// remove-before-teardown every interleaving has exactly ONE owner tearing the pump down — never an
	// orphan. Both sides now serialize on a.mu: if the launch's register section runs first it registers
	// the pump and this teardown reads + stops it; if this section runs first the launch's re-check sees
	// !live and undoes its OWN pump (this teardown read a.pumps[key] == nil). The two sides can't both
	// stop the same pump. reg.remove only touches reg.mu and never calls back into the attacher; lock
	// order a.mu -> reg.mu matches attachClient and the launch good-path (no inversion). The slow KillTmux
	// exec stays OUTSIDE a.mu (in the switch below).
	// (CLOSING was previously set here but never emitted before CLOSED, so it was unobservable — dropped.)
	k := sessionKey{m.SpawnId, m.SessionId}
	a.mu.Lock()
	reg.remove(m.SessionId)
	a.takePending(k) // a STARTING session being closed: drop any pended attaches (their WS will error)
	p := a.pumps[k]
	relay := a.tmuxRelays[k]
	delete(a.pumps, k)
	delete(a.tmuxRelays, k)
	a.mu.Unlock()

	switch e.transport {
	case nodev1.SessionTransport_SESSION_TRANSPORT_ACP:
		if p != nil {
			p.stop()
		}
		_ = a.sx.KillTmux(ctx, m.SpawnId, acpTmuxName(m.SessionId))
		if port, err := strconv.Atoi(e.endpoint); err == nil {
			reg.freePort(port, m.SessionId)
		}
	default: // mosh
		if relay != nil {
			relay.stop()
		}
		_ = a.sx.KillTmux(ctx, m.SpawnId, e.endpoint) // endpoint == mosh tmux name
	}

	a.emitRoster(m.SpawnId)
	a.sessionStatus(m.SpawnId, m.SessionId, nodev1.SessionState_SESSION_STATE_CLOSED, "")
}
