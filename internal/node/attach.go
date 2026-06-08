// Package node implements the spawnlet's CP-attached mode: it dials the CP,
// registers, heartbeats, and services CPMessages by reusing the existing
// spawnlet Manager + per-spawn Pump. It never accepts inbound connections.
package node

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"sync"
	"time"

	"connectrpc.com/connect"

	nodev1 "spawnery/gen/node/v1"
	"spawnery/gen/node/v1/nodev1connect"
	"spawnery/internal/agentcaps"
	"spawnery/internal/spawnlet"
)

// readyTimeout bounds how long startSpawn waits for the agent to answer pump.start's ACP initialize
// handshake before declaring the spawn failed (the standalone readiness probe is folded into that
// handshake). Kept well under the CP scheduler's 60s Provision wait (cmd/cp/main.go) so the node
// reports ERROR (with a useful detail) rather than the scheduler timing out. goose boots to ACP-ready
// in ~5s; 30s is generous headroom for a slow node.
const readyTimeout = 30 * time.Second

type Config struct {
	NodeID        string
	CPURL         string // e.g. http://127.0.0.1:8080
	MaxSpawns     uint32
	AgentImage    string
	AgentBinaries []string // binaries this node's image ships (registry keys: goose, opencode, ...)
	NodeClass     string
	NodeOwner     string
}

// cpStream is the subset of the Connect bidi stream the attacher uses. *connect.BidiStreamForClient
// satisfies it; tests inject a fake to capture sent NodeMessages and drive received CPMessages.
type cpStream interface {
	Send(*nodev1.NodeMessage) error
	Receive() (*nodev1.CPMessage, error)
}

type attacher struct {
	cfg   Config
	mgr   *spawnlet.Manager
	httpc connect.HTTPClient
	sx    sessionExec // container-exec boundary for additional-session launch/reap (sp-npxq.3)

	mu         sync.Mutex
	pumps      map[sessionKey]*Pump
	tmuxRelays map[sessionKey]*tmuxRelay
	sessions   map[string]*sessionRegistry // spawn_id -> live session set (roster source of truth)
	active     uint32

	sendMu sync.Mutex
	stream cpStream
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
func registerMessage(cfg Config, running []*nodev1.RunningSpawn) *nodev1.Register {
	return &nodev1.Register{
		NodeId: cfg.NodeID, MaxSpawns: cfg.MaxSpawns, AgentImages: []string{cfg.AgentImage},
		NodeClass: cfg.NodeClass, NodeOwner: cfg.NodeOwner, Binaries: cfg.AgentBinaries,
		Running: running,
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
		sx:         &realSessionExec{mgr: mgr},
		pumps:      map[sessionKey]*Pump{},
		tmuxRelays: map[sessionKey]*tmuxRelay{},
		sessions:   map[string]*sessionRegistry{},
	}
	client := nodev1connect.NewNodeServiceClient(httpc, cfg.CPURL, connect.WithGRPC())
	a.stream = client.Attach(connCtx)

	if err := a.send(&nodev1.NodeMessage{Msg: &nodev1.NodeMessage_Register{
		Register: registerMessage(cfg, a.runningSpawns()),
	}}); err != nil {
		return err
	}
	log.Printf("node: connected to CP at %s (id=%s class=%s)", cfg.CPURL, cfg.NodeID, cfg.NodeClass)
	go a.heartbeatLoop(connCtx)
	go a.idleReapLoop(connCtx)

	for {
		msg, err := a.stream.Receive()
		if err != nil {
			return err
		}
		a.handle(connCtx, msg)
	}
}

// runningSpawns maps the Manager's live inventory to proto RunningSpawn (all ACTIVE — the Manager
// only holds running spawns), for the CP reconcile carried on Register/Heartbeat.
func (a *attacher) runningSpawns() []*nodev1.RunningSpawn {
	inv := a.mgr.RunningInventory()
	out := make([]*nodev1.RunningSpawn, 0, len(inv))
	for _, mp := range inv {
		out = append(out, &nodev1.RunningSpawn{SpawnId: mp.SpawnID, Generation: mp.Generation, Phase: nodev1.SpawnPhase_ACTIVE})
	}
	return out
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
				ActiveSpawns: active, FreeSlots: free, Running: a.runningSpawns(),
			}}})
		}
	}
}

// Two-stage idle budgets (sp-8hf item 3): a detached spawn (no clients) is reaped sooner than an
// attached one. NOTE: this is a LOSSY teardown (reports STOPPED) — the lossless suspend-on-idle path is
// sp-gd9, gated on E3 persistent storage.
const (
	idleDetachedTimeout = 15 * time.Minute
	idleAttachedTimeout = 60 * time.Minute
	idleReapInterval    = time.Minute
)

// idleReapLoop periodically reaps spawns idle past their stage budget, until ctx is cancelled.
func (a *attacher) idleReapLoop(ctx context.Context) {
	t := time.NewTicker(idleReapInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.reapIdle(ctx, time.Now(), idleDetachedTimeout, idleAttachedTimeout)
		}
	}
}

// reapIdle tears down every spawn whose last relay activity is older than its stage budget (detached
// vs attached), measured from now. Candidates are snapshotted under the lock, then stopped outside it
// (stopSpawn takes the lock itself). Both ACP pumps and tmux relays are reaped.
func (a *attacher) reapIdle(ctx context.Context, now time.Time, detachedTimeout, attachedTimeout time.Duration) {
	// Session #0 keys gate whole-spawn idle reaping; additional sessions are reaped only on explicit
	// close (sp-npxq.3, plan decision 8) — an idle session-N pump/relay must NOT stopSpawn the container.
	a.mu.Lock()
	type cand struct {
		key sessionKey
		p   *Pump
	}
	type relayCand struct {
		key sessionKey
		r   *tmuxRelay
	}
	cands := make([]cand, 0, len(a.pumps))
	for k, p := range a.pumps {
		if k.sessionID != SessionZeroID {
			continue // additional sessions are reaped by explicit close / stopSpawn, not idle (decision 8)
		}
		cands = append(cands, cand{k, p})
	}
	relayCands := make([]relayCand, 0, len(a.tmuxRelays))
	for k, r := range a.tmuxRelays {
		if k.sessionID != SessionZeroID {
			continue
		}
		relayCands = append(relayCands, relayCand{k, r})
	}
	a.mu.Unlock()

	for _, c := range cands {
		budget := detachedTimeout
		if c.p.attached() {
			budget = attachedTimeout
		}
		if now.Sub(c.p.lastActive()) >= budget {
			log.Printf("idle-reaping spawn=%s (idle past %s, attached=%v)", c.key.spawnID, budget, c.p.attached())
			a.stopSpawn(ctx, c.key.spawnID)
		}
	}
	for _, c := range relayCands {
		budget := detachedTimeout
		if c.r.attached() {
			budget = attachedTimeout
		}
		if now.Sub(c.r.lastActive()) >= budget {
			log.Printf("idle-reaping tmux spawn=%s (idle past %s, attached=%v)", c.key.spawnID, budget, c.r.attached())
			a.stopSpawn(ctx, c.key.spawnID)
		}
	}
}

func (a *attacher) status(spawnID string, ph nodev1.SpawnPhase, detail string) {
	_ = a.send(&nodev1.NodeMessage{Msg: &nodev1.NodeMessage_Status{Status: &nodev1.SpawnStatus{SpawnId: spawnID, Phase: ph, Detail: detail}}})
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
	default:
		// TODO(sp-gd9): handle *nodev1.CPMessage_Suspend (persist mounts + tear down, then emit
		// NodeMessage_SuspendComplete with per-mount markers). Inert until the suspend path lands.
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

func (a *attacher) startSpawn(ctx context.Context, st *nodev1.StartSpawn) {
	a.status(st.SpawnId, nodev1.SpawnPhase_STARTING, "")
	sp, err := a.mgr.CreateWithSelection(ctx, st.SpawnId, st.AppRef, st.Model, st.Name, st.AppId, st.Generation,
		spawnlet.AgentSelection{Image: st.Image, RunnableID: st.RunnableId, Mode: st.Mode})
	if err != nil {
		logErr("startSpawn "+st.SpawnId, err)
		a.status(st.SpawnId, nodev1.SpawnPhase_ERROR, err.Error())
		return
	}
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
		a.status(st.SpawnId, nodev1.SpawnPhase_ACTIVE, "")
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
	a.status(st.SpawnId, nodev1.SpawnPhase_ACTIVE, "")
}

func (a *attacher) stopSpawn(ctx context.Context, spawnID string) {
	// Reap every session of the spawn (sp-npxq.3 adds sessions 1..N; today only #0 exists).
	a.mu.Lock()
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
	delete(a.sessions, spawnID)
	a.mu.Unlock()
	for _, p := range ps {
		p.stop()
	}
	for _, r := range relays {
		r.stop()
	}
	if err := a.mgr.Stop(ctx, spawnID); err != nil {
		logErr("stopSpawn "+spawnID, err)
	}
	a.mu.Lock()
	// TODO(sp-npxq.3): account per-session capacity (one slot per spawn today, one primary session).
	if a.active > 0 {
		a.active--
	}
	a.mu.Unlock()
	a.status(spawnID, nodev1.SpawnPhase_STOPPED, "")
}

func (a *attacher) attachClient(spawnID, sessionID, clientID string, cursor int64) {
	k := sessionKey{spawnID, sessionID}
	a.mu.Lock()
	relay := a.tmuxRelays[k]
	p := a.pumps[k]
	a.mu.Unlock()
	if relay != nil {
		if err := relay.attach(context.Background(), clientID); err != nil {
			log.Printf("tmux attach %s/%s/%s: %v", spawnID, sessionID, clientID, err)
		}
		return
	}
	if p == nil {
		log.Printf("warn: attachClient: no pump for %s/%s", spawnID, sessionID)
		return
	}
	send := func(line []byte) error {
		return a.send(&nodev1.NodeMessage{Msg: &nodev1.NodeMessage_Frame{Frame: &nodev1.Frame{
			SpawnId: spawnID, SessionId: sessionID, ClientId: clientID, Data: append([]byte(nil), line...),
		}}})
	}
	p.attachClient(clientID, cursor, send)
}

func (a *attacher) detachClient(spawnID, sessionID, clientID string) {
	k := sessionKey{spawnID, sessionID}
	a.mu.Lock()
	relay := a.tmuxRelays[k]
	p := a.pumps[k]
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

	// opencode-tui attaches to a served opencode; reject if the spawn has none (plan decision 9, CONFIRM).
	if m.Runnable == "opencode-tui" && !hasServedOpencode(reg) {
		log.Printf("rejecting opencode-tui session for %s: no served opencode in spawn", m.SpawnId)
		a.sessionStatus(m.SpawnId, "", nodev1.SessionState_SESSION_STATE_ERROR, "opencode-tui needs a served opencode session")
		return
	}

	id := reg.allocID()
	e := &sessionEntry{id: id, transport: m.Transport, runnable: m.Runnable, state: nodev1.SessionState_SESSION_STATE_STARTING}

	if m.Transport == nodev1.SessionTransport_SESSION_TRANSPORT_ACP {
		port, ok := reg.allocPort()
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

// hasServedOpencode reports whether the spawn already runs an opencode-served session (the backend an
// additional opencode-tui session attaches to). See plan decision 9.
func hasServedOpencode(reg *sessionRegistry) bool {
	for _, s := range reg.snapshot() {
		if s.Runnable == "opencode-served" {
			return true
		}
	}
	return false
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
			a.failSession(spawnID, reg, e, "attach argv: "+err.Error())
			return
		}
		sessID := e.id
		relay := newTmuxRelay(argv, func(clientID string, data []byte) error {
			return a.send(&nodev1.NodeMessage{Msg: &nodev1.NodeMessage_Frame{Frame: &nodev1.Frame{
				SpawnId: spawnID, SessionId: sessID, ClientId: clientID, Data: data,
			}}})
		})
		a.mu.Lock()
		_, live := reg.get(e.id)
		if !live { // closed mid-launch: undo
			a.mu.Unlock()
			relay.stop()
			_ = a.sx.KillTmux(ctx, spawnID, e.endpoint)
			return
		}
		a.tmuxRelays[sessionKey{spawnID, e.id}] = relay
		a.mu.Unlock()
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
	a.mu.Lock()
	_, live := reg.get(e.id)
	if !live { // closed mid-launch: undo
		a.mu.Unlock()
		p.stop()
		_ = a.sx.KillTmux(ctx, spawnID, tmuxName)
		reg.freePort(port)
		return
	}
	a.pumps[sessionKey{spawnID, e.id}] = p
	a.mu.Unlock()
	reg.setState(e.id, nodev1.SessionState_SESSION_STATE_ACTIVE)
	a.emitRoster(spawnID)
	a.sessionStatus(spawnID, e.id, nodev1.SessionState_SESSION_STATE_ACTIVE, "")
}

// failSession reaps a session that failed to launch: free its acp port (if any), drop the entry, emit
// the updated roster + an ERROR SessionStatus.
func (a *attacher) failSession(spawnID string, reg *sessionRegistry, e *sessionEntry, detail string) {
	logErr("launchSession "+spawnID+"/"+e.id, fmt.Errorf("%s", detail))
	if e.transport == nodev1.SessionTransport_SESSION_TRANSPORT_ACP {
		if p, err := strconv.Atoi(e.endpoint); err == nil {
			reg.freePort(p)
		}
	}
	reg.remove(e.id)
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
	reg.setState(m.SessionId, nodev1.SessionState_SESSION_STATE_CLOSING)

	a.mu.Lock()
	p := a.pumps[sessionKey{m.SpawnId, m.SessionId}]
	relay := a.tmuxRelays[sessionKey{m.SpawnId, m.SessionId}]
	delete(a.pumps, sessionKey{m.SpawnId, m.SessionId})
	delete(a.tmuxRelays, sessionKey{m.SpawnId, m.SessionId})
	a.mu.Unlock()

	switch e.transport {
	case nodev1.SessionTransport_SESSION_TRANSPORT_ACP:
		if p != nil {
			p.stop()
		}
		_ = a.sx.KillTmux(ctx, m.SpawnId, acpTmuxName(m.SessionId))
		if port, err := strconv.Atoi(e.endpoint); err == nil {
			reg.freePort(port)
		}
	default: // mosh
		if relay != nil {
			relay.stop()
		}
		_ = a.sx.KillTmux(ctx, m.SpawnId, e.endpoint) // endpoint == mosh tmux name
	}

	reg.remove(m.SessionId)
	a.emitRoster(m.SpawnId)
	a.sessionStatus(m.SpawnId, m.SessionId, nodev1.SessionState_SESSION_STATE_CLOSED, "")
}
