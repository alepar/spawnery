// Package cp is the control plane: it accepts node Attach streams and client
// cp.v1 calls, routing between them. Both the node and the CP are transparent
// byte relays — ACP smarts live in the client and agent only.
package cp

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	authv1 "spawnery/gen/auth/v1"
	cpv1 "spawnery/gen/cp/v1"
	"spawnery/gen/cp/v1/cpv1connect"
	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/agentcaps"
	"spawnery/internal/authsvc/token"
	"spawnery/internal/cp/auth"
	"spawnery/internal/cp/journalkeys"
	"spawnery/internal/cp/lock"
	"spawnery/internal/cp/nodeauth"
	"spawnery/internal/cp/registry"
	"spawnery/internal/cp/router"
	"spawnery/internal/cp/scheduler"
	"spawnery/internal/cp/store"
	"spawnery/internal/cp/telemetry"
	"spawnery/internal/intent"
)

// reconcileAttempt tracks, for one spawn, when the reconciler first started trying to apply the
// CURRENT model. Keyed by spawn id; the model string lets a fresh SetSpawnModel reset the clock.
type reconcileAttempt struct {
	model string
	first time.Time
}

type Server struct {
	cpv1connect.UnimplementedSpawnServiceHandler // new RPCs default to CodeUnimplemented until sp-pc4
	reg                                          *registry.Registry
	rt                                           *router.Router
	sched                                        *scheduler.Scheduler
	st                                           store.Store
	tel                                          telemetry.Sink
	locks                                        *lock.Keyed

	models          *modelWaiters // correlates inline SetSpawnModel pushes with node SetModelResult acks
	setModelTimeout time.Duration // bound for the inline SetModel push; overridable in tests

	suspends       *suspendWaiters // correlates a SuspendSpawn with the node's SuspendComplete (markers)
	suspendTimeout time.Duration   // bound for awaiting SuspendComplete; overridable in tests

	// upgradeWaiters correlates UpgradeToOwnerSealed requests with SealJournalKeyToOwnerResponse
	// messages from the node (sp-8dkp §4). Keyed by per-request request_id.
	upgradeWaiters *upgradeWaiters
	upgradeTimeout time.Duration // bound for awaiting node seal; overridable in tests

	// Reconciler: a background loop drives model_applied=false spawns to convergence (sp-bp9w.7).
	reconcileInterval time.Duration               // tick period; overridable in tests
	reconcileGiveUp   time.Duration               // per-spawn bounded retry window before giving up
	now               func() time.Time            // clock, injectable in tests
	giveUp            map[string]reconcileAttempt // spawn id -> first-attempt time for current model; reconciler-goroutine-only (no lock)

	maxSpawnsPerOwner int

	// nodeKeys caches each node's published HPKE sub-key + relayed cert chain (sp-2ckv.4), refreshed on
	// Register/Heartbeat. GetSpawnNodeKey serves it to owner clients; the CP relays — never unseals.
	nodeKeys *nodeKeyCache

	// journalKeys custodies owner-sealed journal-password CIPHERTEXT per (spawn, mount) — opaque to the
	// CP. Get/PutJournalKeyCiphertext serve/store it; MigrateSpawn relies on the owner client fetching
	// it to reseal to the target node. ownerDevices resolves an owner's enrolled device pubkeys (the
	// recipient set), wired to a populatable MemDeviceRegistry — not the fail-closed UnwiredRegistry.
	journalKeys  journalkeys.Store
	ownerDevices journalkeys.OwnerDeviceRegistry

	// Auth: session registry for revocation fan-out + in-band reauth; verify for the reauth path.
	// verify is a func to avoid an import cycle: main wires it; nil = no reauth enforcement.
	sessions       *auth.SessionRegistry
	verify         func(string) (auth.Identity, error)
	devMode        bool
	reauthInterval time.Duration // reauth deadline; 0 uses defaultReauthInterval

	// intentEnabled gates the A4 two-phase sign-after-resolve flow. Decoupled from devMode so that
	// dev instances can run the full A4 flow (verify-and-log at the node) when a dev AS key is
	// available. Default false keeps existing tests passing — tests that need the intent flow call
	// SetIntentEnabled(true). Production callers also call SetIntentEnabled(true) after auth mode
	// is confirmed [AM12].
	intentEnabled bool
	// devASKey is the dev-only AS Ed25519 signing key used to mint aud=node tokens in SubmitIntent
	// when the client does not supply NodeAccessToken. Set via SetDevASKey; nil = no dev minting.
	devASKey   ed25519.PrivateKey
	devASKeyID string

	// pendingIntents is the A4 two-phase sign-after-resolve registry [AC1]. Lifecycle handlers
	// (Create/Resume/Recreate/Migrate) register a pending intent BEFORE calling Provision; the
	// client polls GetPendingIntent and submits a SignedIntent via SubmitIntent, unblocking provision.
	pendingIntents *pendingIntentRegistry

	// deliveryPending tracks spawns that are active on a target node but whose owner-sealed journal
	// key has not yet been delivered by the browser (the post-migration delivery step, sp-8dkp §5).
	// Set by MigrateSpawn when upgrade_to_owner_sealed=true; cleared by DeliverSecrets on journal-key
	// delivery. Surfaced as journal_key_delivery_pending in ListSpawns to drive the web-UI step.
	deliveryPending *deliveryPendingTracker
}

const (
	defaultReconcileInterval = 5 * time.Second  // reconciler tick period
	defaultReconcileGiveUp   = 2 * time.Minute  // bounded per-spawn retry window
	defaultReauthInterval    = 15 * time.Minute // in-band reauth deadline
	reauthGrace              = 30 * time.Second // grace period beyond the reauth deadline
)

// Server must satisfy the (now larger) connect handler interface; the 5 new lifecycle RPCs are
// served by the embedded Unimplemented handler until sp-pc4 overrides them.
var _ cpv1connect.SpawnServiceHandler = (*Server)(nil)

func NewServer(reg *registry.Registry, rt *router.Router, sched *scheduler.Scheduler, st store.Store, tel telemetry.Sink) *Server {
	return &Server{reg: reg, rt: rt, sched: sched, st: st, tel: tel, locks: lock.New(),
		models: newModelWaiters(), setModelTimeout: defaultSetModelPushTimeout,
		suspends: newSuspendWaiters(), suspendTimeout: defaultSuspendTimeout,
		upgradeWaiters:    newUpgradeWaiters(),
		reconcileInterval: defaultReconcileInterval, reconcileGiveUp: defaultReconcileGiveUp,
		now: time.Now, giveUp: map[string]reconcileAttempt{}, nodeKeys: newNodeKeyCache(),
		journalKeys: journalkeys.NewMemStore(), ownerDevices: journalkeys.NewMemDeviceRegistry(),
		pendingIntents:  newPendingIntentRegistry(),
		deliveryPending: newDeliveryPendingTracker(),
		// devMode=true is the safe default: production explicitly calls SetDevMode(false) after
		// confirming auth mode. Tests that don't call SetDevMode get dev mode (no intent enforcement).
		devMode: true}
}

// --- node side: NodeService/Attach ----------------------------------------

// nodeStream is the concurrency-safe CP->node sender (one writer per stream).
type nodeStream struct {
	mu     sync.Mutex
	stream *connect.BidiStream[nodev1.NodeMessage, nodev1.CPMessage]
}

func (n *nodeStream) Send(m *nodev1.CPMessage) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.stream.Send(m)
}

func (s *Server) Attach(ctx context.Context, stream *connect.BidiStream[nodev1.NodeMessage, nodev1.CPMessage]) error {
	sender := &nodeStream{stream: stream}
	return s.runNode(ctx, sender, stream.Receive)
}

// runNode is the receive loop, split out so it is unit-testable without gRPC.
func (s *Server) runNode(ctx context.Context, sender registry.NodeSender, recv func() (*nodev1.NodeMessage, error)) error {
	var nodeID string
	var nodeClass string
	var token uint64 // this connection's registry token (0 until accepted); guards teardown
	defer func() {
		// Only tear down routes if THIS connection is still the registered owner. A rejected
		// duplicate (token 0) or a displaced old stream must not drop the live node's routes.
		if nodeID == "" || !s.reg.RemoveIfCurrent(nodeID, token) {
			return
		}
		dropped := s.rt.DropNode(nodeID)
		if len(dropped) > 0 {
			_, _ = s.st.Spawns().MarkUnreachable(context.Background(), dropped)
		}
		for _, id := range dropped {
			_ = s.tel.Emit(telemetry.Event{Kind: "session_end", NodeID: nodeID, SpawnID: id, Timestamp: time.Now().UTC()})
		}
	}()
	for {
		msg, err := recv()
		if err != nil {
			return nil // stream closed
		}
		// If a newer connection has taken over this node id (we were displaced), stop serving.
		if token != 0 && !s.reg.IsCurrent(nodeID, token) {
			return nil
		}
		switch m := msg.Msg.(type) {
		case *nodev1.NodeMessage_Register:
			nodeID = m.Register.NodeId
			nodeClass = m.Register.NodeClass
			nodeOwner := m.Register.NodeOwner
			// enforced mode: the verified mTLS identity is authoritative; the self-asserted Register
			// fields are ignored. insecure mode (no identity on ctx) falls back to them (dev/test).
			if id, ok := nodeauth.IdentityFromContext(ctx); ok {
				if m.Register.NodeId != "" && m.Register.NodeId != id.NodeID {
					log.Printf("node %s: self-asserted node_id %q != verified identity; using verified", id.NodeID, m.Register.NodeId)
				}
				nodeID, nodeClass, nodeOwner = id.NodeID, id.Class, id.AccountID
			}
			if nodeClass == "" {
				nodeClass = "cloud" // safe default: an unidentified node is assumed restricted
			}
			// Master's token-based Register (duplicate rejection) fed with sp-ova's VERIFIED
			// identity values (nodeClass/nodeOwner from mTLS in enforced mode), not the
			// self-asserted Register fields — using m.Register.NodeOwner here would defeat sp-ova.
			tok, accepted := s.reg.Register(&registry.Node{ID: nodeID, Sender: sender, Max: m.Register.MaxSpawns, Free: m.Register.MaxSpawns, Images: m.Register.AgentImages, Class: nodeClass, Owner: nodeOwner})
			if !accepted {
				// A live node already holds this id: reject the duplicate rather than corrupt routing.
				log.Printf("rejecting registration for node id=%s: another node with that id is still alive", nodeID)
				return connect.NewError(connect.CodeAlreadyExists, fmt.Errorf("node id %q is already registered and alive", nodeID))
			}
			token = tok
			log.Printf("node connected: id=%s class=%s owner=%q max_spawns=%d images=%v", nodeID, nodeClass, nodeOwner, m.Register.MaxSpawns, m.Register.AgentImages)
			// Cache the node's published sub-key + relayed cert chain (sp-2ckv.4). The chain is the
			// mTLS-verified peer chain (empty in insecure mode); the sub-key is the node's published JSON.
			certChain, _ := nodeauth.CertChainFromContext(ctx)
			s.nodeKeys.put(nodeID, m.Register.SignedSubkey, certChain)
			s.reconcileInventory(ctx, nodeID, sender, m.Register.Running) // a returning node reports what it still runs
			s.upsertAgentCatalog(ctx, m.Register.AgentImages, m.Register.Binaries)
		case *nodev1.NodeMessage_Heartbeat:
			s.reg.Heartbeat(nodeID, token, m.Heartbeat.ActiveSpawns, m.Heartbeat.FreeSlots)
			if len(m.Heartbeat.SignedSubkey) > 0 {
				certChain, _ := nodeauth.CertChainFromContext(ctx)
				s.nodeKeys.put(nodeID, m.Heartbeat.SignedSubkey, certChain) // sub-key rotated -> refresh cache
			}
			s.reconcileInventory(ctx, nodeID, sender, m.Heartbeat.Running)
		case *nodev1.NodeMessage_Status:
			s.sched.OnStatus(m.Status.SpawnId, m.Status.Phase, m.Status.Detail)
			if m.Status.Phase == nodev1.SpawnPhase_ACTIVE {
				var owner string
				if sp, err := s.st.Spawns().Get(ctx, m.Status.SpawnId); err == nil {
					owner = sp.OwnerID
				}
				_ = s.tel.Emit(telemetry.Event{Kind: "spawn_create", Owner: owner, NodeID: nodeID, NodeClass: nodeClass, SpawnID: m.Status.SpawnId, Tier: "reviewed", Storage: "managed", Timestamp: time.Now().UTC()})
				// Spec §4 report-back: node resolves the base-image digest at create time and
				// sends it with the ACTIVE status so the CP can persist it for cross-node resume.
				// Best-effort: non-fatal if the digest is empty (resolution failed on the node)
				// or if the store call fails (e.g. spawn deleted before ACTIVE lands).
				if dg := m.Status.BaseImageDigest; dg != "" {
					if err := s.st.Spawns().SetBaseImageDigest(ctx, m.Status.SpawnId, dg); err != nil {
						log.Printf("spawn %s: persist base_image_digest %q: %v (non-fatal)", m.Status.SpawnId, dg, err)
					}
				}
			}
		case *nodev1.NodeMessage_Frame:
			s.rt.FromNode(m.Frame.SpawnId, m.Frame.SessionId, m.Frame.ClientId, m.Frame.Data) // opaque bytes; never inspected
		case *nodev1.NodeMessage_Roster:
			s.rt.UpdateRoster(m.Roster.SpawnId, nodeID, m.Roster.Sessions) // node-authoritative session set; CP mirrors
		case *nodev1.NodeMessage_SessionStatus:
			s.rt.ApplySessionStatus(m.SessionStatus.SpawnId, m.SessionStatus.SessionId, m.SessionStatus.State)
		case *nodev1.NodeMessage_SetModelResult:
			s.models.deliver(m.SetModelResult)
		case *nodev1.NodeMessage_SealJournalKeyResult:
			s.upgradeWaiters.deliver(m.SealJournalKeyResult)
		case *nodev1.NodeMessage_SuspendComplete:
			// Route to the SuspendSpawn awaiting this spawn's persist markers. deliver drops a
			// stale-episode reply (generation != the awaiting episode's) — mirrors the generation fence
			// the node applies to the inbound Suspend. A reply with no live waiter (timed-out suspend) is
			// likewise dropped, never blocking this Receive loop.
			s.suspends.deliver(m.SuspendComplete)
		}
	}
}

// reconcileInventory diffs the node's reported running inventory against the store (state/DAO
// design §6.2) in three idempotent arms, run on Register and on every Heartbeat:
//
//  1. adopt: a reported (spawn_id, gen) matching the spawn's live container row is (re)bound to the
//     reporting node — see adoptOrStop. Steady state is a cheap no-op (one DB point read plus one
//     route-map lookup).
//  2. orphan: a reported (spawn_id, gen) with NO matching live row (suspended/deleted/errored spawn,
//     or a superseded generation after recreate) -> StopSpawn(spawn_id, gen) to the reporting node.
//  3. unreachable: an ACTIVE live row on this node the node does NOT report (it restarted and lost
//     the pod, or the pod died) -> drop the route, mark the spawn unreachable. The live row is KEPT
//     so a later report can re-adopt it. Starting spawns are skipped (may not be in the inventory
//     yet). User-driven recovery (RecreateSpawn) also remains available.
func (s *Server) reconcileInventory(ctx context.Context, nodeID string, sender registry.NodeSender, running []*nodev1.RunningSpawn) {
	if nodeID == "" {
		return
	}
	reported := make(map[string]bool, len(running))
	for _, rs := range running {
		reported[rs.GetSpawnId()] = true
		s.adoptOrStop(ctx, nodeID, sender, rs)
	}
	live, err := s.st.Spawns().LiveContainersByNode(ctx, nodeID)
	if err != nil {
		return
	}
	var lost []string
	for _, c := range live {
		// A suspend in flight INTENTIONALLY makes the node stop reporting the torn-down container
		// while the CP row is still Active (SetSuspending is deferred to the node's reply). Such a
		// container is not "lost" — exempt it so the reconcile doesn't flip it Unreachable and make
		// the suspend's SetSuspending conflict.
		if c.Phase == store.PhaseActive && !reported[c.SpawnID] && !s.suspends.inFlight(c.SpawnID) {
			lost = append(lost, c.SpawnID)
		}
	}
	if len(lost) == 0 {
		return
	}
	for _, id := range lost {
		s.rt.Drop(id)
	}
	if n, err := s.st.Spawns().MarkUnreachable(ctx, lost); err == nil && n > 0 {
		log.Printf("node %s inventory: %d active spawn(s) not reported -> unreachable", nodeID, n)
	}
}

// adoptOrStop handles ONE reported running spawn: the adopt + orphan arms of reconcileInventory.
// Idempotent per heartbeat: when the live row already points at this node and the route is bound,
// it is a single point read and a map lookup — no writes.
func (s *Server) adoptOrStop(ctx context.Context, nodeID string, sender registry.NodeSender, rs *nodev1.RunningSpawn) {
	id, gen := rs.GetSpawnId(), int64(rs.GetGeneration())
	c, ok, err := s.st.Spawns().LiveContainer(ctx, id)
	if err != nil {
		return // transient store error; the next heartbeat retries
	}
	matched := ok && c.Generation == gen
	if matched && c.NodeID != nodeID {
		// The live row is bound elsewhere (node came back under a new id, or the CP recorded a
		// since-stale binding): rebind it to the reporter — Adopt's documented contract. ErrConflict
		// means the gen was fenced out concurrently (recreate/stop won the race), so the reported
		// pod is an orphan after all. Any OTHER error (DB I/O) says nothing about orphanhood:
		// stopping a healthy pod over a transient store error would be harmful, so log and let the
		// next heartbeat retry.
		switch aerr := s.st.Spawns().Adopt(ctx, id, nodeID, gen); {
		case errors.Is(aerr, store.ErrConflict):
			matched = false
		case aerr != nil:
			log.Printf("node %s inventory: Adopt spawn %s gen %d: %v", nodeID, id, gen, aerr)
			return
		default:
			log.Printf("node %s inventory: adopted spawn %s gen %d", nodeID, id, gen)
		}
	}
	if !matched {
		// Orphaned pod: suspended/deleted/errored spawn, or a superseded generation. Tell the
		// reporting node to destroy it — a direct send, since an orphan has no route. The node-side
		// gen fence guarantees this can only kill the stale pod, never a current episode.
		_ = sender.Send(&nodev1.CPMessage{Msg: &nodev1.CPMessage_Stop{Stop: &nodev1.StopSpawn{SpawnId: id, Generation: rs.GetGeneration()}}})
		return
	}
	if s.rt.Bound(id) {
		return // steady state: route bound + row already adopted -> per-heartbeat no-op
	}
	s.rt.Bind(id, nodeID, sender)
	// Wait->adopt: a spawn marked unreachable (boot sweep or node loss) turned out to be alive.
	// Flip it back to active; ErrConflict is a benign race (a concurrent recreate/stop moved the
	// spawn first) and any other status is none of the adopt arm's business. A non-conflict error
	// (DB I/O) must not vanish — log it (behavior otherwise unchanged).
	if sp, gerr := s.st.Spawns().Get(ctx, id); gerr == nil && sp.Status == store.Unreachable {
		switch rerr := s.st.Spawns().MarkReachable(ctx, id, gen); {
		case rerr == nil:
			log.Printf("node %s inventory: spawn %s reachable again -> active", nodeID, id)
		case !errors.Is(rerr, store.ErrConflict):
			log.Printf("node %s inventory: MarkReachable spawn %s gen %d: %v", nodeID, id, gen, rerr)
		}
	}
}

// upsertAgentCatalog records each advertised image and the binaries it ships so the durable catalog
// (ListAgentImages + runnable validation) reflects what connected nodes can run. Idempotent across
// reconnects: created_at is preserved and the binary set is replaced. Errors are logged, not fatal —
// a catalog write must never break node registration.
func (s *Server) upsertAgentCatalog(ctx context.Context, images, binaries []string) {
	now := time.Now().Unix()
	for _, img := range images {
		if img == "" {
			continue
		}
		if err := s.st.WithTx(ctx, func(tx store.Store) error {
			return tx.AgentImages().Upsert(ctx, store.AgentImage{Image: img, CreatedAt: now}, binaries)
		}); err != nil {
			log.Printf("register: upsert agent image %q: %v", img, err)
		}
	}
}

// lookupRunnable finds the first runnable matching id across an image's binaries.
func lookupRunnable(bins []string, id string) (agentcaps.Runnable, bool) {
	for _, b := range bins {
		if r, ok := agentcaps.Lookup(b, id); ok {
			return r, true
		}
	}
	return agentcaps.Runnable{}, false
}

// --- client side: cp.v1 SpawnService --------------------------------------

// SetMaxSpawnsPerOwner sets the per-owner concurrent-spawn cap (0 = unlimited).
func (s *Server) SetMaxSpawnsPerOwner(n int) { s.maxSpawnsPerOwner = n }

// SetSessionRegistry wires the session registry for revocation fan-out.
func (s *Server) SetSessionRegistry(sr *auth.SessionRegistry) { s.sessions = sr }

// SetVerify wires the token verifier function for in-band reauth.
// v is called with the raw token wire; returns an Identity or an error.
func (s *Server) SetVerify(v func(string) (auth.Identity, error)) { s.verify = v }

// SetDevMode sets whether the server is in dev mode (reauth enforced in prod only).
func (s *Server) SetDevMode(dev bool) { s.devMode = dev }

// SetIntentEnabled enables or disables the A4 two-phase sign-after-resolve flow [AC1][AM12].
// Defaults to false so existing tests are unaffected. Production main and dev instances with a
// dev AS key call SetIntentEnabled(true).
func (s *Server) SetIntentEnabled(v bool) { s.intentEnabled = v }

// SetDevASKey configures the dev-only AS Ed25519 signing key used to mint aud=node tokens in
// SubmitIntent when the client omits NodeAccessToken. priv must be non-nil; keyID is the derived
// token.KeyID. In prod mode this MUST NOT be set — prod clients obtain node tokens from the real AS.
func (s *Server) SetDevASKey(priv ed25519.PrivateKey, keyID string) {
	s.devASKey = priv
	s.devASKeyID = keyID
}

// SetReauthInterval overrides the in-band reauth deadline (default 15 min).
func (s *Server) SetReauthInterval(d time.Duration) { s.reauthInterval = d }

// checkSpawnQuota returns ResourceExhausted if the owner is at/over the per-owner spawn cap.
func (s *Server) checkSpawnQuota(ctx context.Context, owner string) error {
	if s.maxSpawnsPerOwner <= 0 {
		return nil
	}
	existing, err := s.st.Spawns().ListByOwner(ctx, owner)
	if err != nil {
		return connect.NewError(connect.CodeInternal, err)
	}
	if len(existing) >= s.maxSpawnsPerOwner {
		return connect.NewError(connect.CodeResourceExhausted, fmt.Errorf("spawn limit reached (%d)", s.maxSpawnsPerOwner))
	}
	return nil
}

func (s *Server) CreateSpawn(ctx context.Context, req *connect.Request[cpv1.CreateSpawnRequest]) (*connect.Response[cpv1.CreateSpawnResponse], error) {
	owner, ok := auth.OwnerFromContext(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("no owner"))
	}
	if err := s.checkSpawnQuota(ctx, owner); err != nil {
		return nil, err
	}
	appID := req.Msg.AppId
	var err error
	var ver store.AppVersion
	if v := req.Msg.Version; v != "" {
		ver, err = s.st.Apps().GetVersion(ctx, appID, v)
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("unknown app version: %s@%s", appID, v))
		}
	} else {
		ver, err = s.st.Apps().LatestReviewed(ctx, appID)
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("unknown app: %s", appID))
		}
	}
	// Resolve the optional agent selection: validate the runnable is offered by the chosen
	// image's binaries, resolve the run mode, and reject modes we can't launch yet.
	var selImage, selRunnable, selMode string
	if req.Msg.Image == "" && req.Msg.RunnableId != "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("image is required when runnable_id is set"))
	}
	if req.Msg.Image != "" {
		selImage = req.Msg.Image
		if req.Msg.RunnableId == "" {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("runnable_id is required when image is set"))
		}
		bins, berr := s.st.AgentImages().Binaries(ctx, selImage)
		if berr != nil {
			return nil, connect.NewError(connect.CodeInternal, berr)
		}
		if len(bins) == 0 {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("unknown or empty agent image: %s", selImage))
		}
		run, found := lookupRunnable(bins, req.Msg.RunnableId)
		if !found {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("runnable %q is not offered by image %s", req.Msg.RunnableId, selImage))
		}
		selRunnable = run.ID
		selMode = string(run.Mode)
	}
	decls, err := s.st.Apps().DeclaredMounts(ctx, appID, ver.Version)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	mounts := make([]store.Mount, len(decls))
	for i, d := range decls {
		mounts[i] = store.Mount{Name: d.Name, BackendURI: "scratch"}
	}
	placement, err := s.placementFor(ctx, owner, appID, ver)
	if err != nil {
		return nil, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	spawnID := id.String()

	unlock := s.locks.Lock(spawnID)
	defer unlock()

	// Name is best-effort dedup: the lock above is keyed by spawn id (not owner), so concurrent
	// creates for one owner may still produce duplicate names — harmless, since the spawn id is the key.
	name := strings.TrimSpace(req.Msg.Name)
	if name == "" {
		base := appID
		if app, aerr := s.st.Apps().Get(ctx, appID); aerr == nil && app.DisplayName != "" {
			base = app.DisplayName
		}
		existing, lerr := s.st.Spawns().ListByOwner(ctx, owner)
		if lerr != nil {
			return nil, connect.NewError(connect.CodeInternal, lerr)
		}
		taken := make(map[string]bool, len(existing))
		for _, e := range existing {
			taken[e.Name] = true
		}
		name = nextSpawnName(base, taken)
	}

	now := time.Now().Unix()
	sp := store.Spawn{
		ID: spawnID, OwnerID: owner, Name: name, AppID: appID, AppVersion: ver.Version, AppRef: ver.Ref, Pinned: req.Msg.Pin,
		Model: req.Msg.Model, Image: selImage, RunnableID: selRunnable, Mode: selMode,
		Status: store.Starting, CreatedAt: now, LastUsedAt: now,
	}
	if err := s.st.WithTx(ctx, func(tx store.Store) error { return tx.Spawns().Create(ctx, sp, mounts) }); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	// Provision asynchronously: return the spawn in 'starting' immediately; the background goroutine
	// drives it to active/error on the node's signal, so the UI can show a 'starting' period. The
	// request ctx is done once we return, so the goroutine uses a detached ctx.
	go s.provisionSpawn(context.WithoutCancel(ctx), spawnID, owner, ver.Ref, req.Msg.Model, placement)
	return connect.NewResponse(&cpv1.CreateSpawnResponse{SpawnId: spawnID}), nil
}

// provisionSpawn runs the async provision for a spawn that CreateSpawn left in 'starting'. It takes
// the per-spawn lock (serializing a Stop/Suspend during starting AFTER it) and bails if the spawn was
// already stopped in the lock gap; then Provision -> SetActive, or SetError on failure, with the same
// teardown compensation as the old inline path on a post-provision SetActive failure.
// When intentEnabled=true the two-phase A4 flow runs: PickNodeID → register pending intent →
// await SignedIntent from client → Provision. When intentEnabled=false the flow is skipped (nil env).
func (s *Server) provisionSpawn(ctx context.Context, spawnID, ownerID, appRef, model string, placement registry.Placement) {
	unlock := s.locks.Lock(spawnID)
	defer unlock()
	sp, err := s.st.Spawns().Get(ctx, spawnID)
	if err != nil || sp.Status != store.Starting {
		return // stopped/deleted in the lock gap, or already advanced
	}
	placement.Image = sp.Image

	// Gen 1: store.Create inserted the live container row at generation 1 (SetActive below matches).
	var env *authv1.AuthEnvelope
	if s.intentEnabled {
		// Two-phase A4 sign-after-resolve [AC1]: pick node, register pending intent, await client.
		targetNodeID, pickErr := s.sched.PickNodeID(placement)
		if pickErr != nil {
			log.Printf("provisionSpawn %s: PickNodeID failed: %v", spawnID, pickErr)
			if serr := s.st.Spawns().SetError(ctx, spawnID); serr != nil {
				log.Printf("provisionSpawn %s: SetError after PickNodeID failure also failed: %v", spawnID, serr)
			}
			return
		}
		mounts, _ := s.st.Spawns().GetMounts(ctx, spawnID)
		pi := buildPendingIntent(intent.OpCreateSpawn, spawnID, 1, targetNodeID, sp.Image, appRef, model, "", mounts)
		ch := s.pendingIntents.register(spawnID, ownerID, pi)
		defer s.pendingIntents.cleanup(spawnID)
		env, err = s.pendingIntents.await(ctx, ch)
		if err != nil {
			log.Printf("provisionSpawn %s: await SignedIntent: %v", spawnID, err)
			if serr := s.st.Spawns().SetError(ctx, spawnID); serr != nil {
				log.Printf("provisionSpawn %s: SetError after await failure also failed: %v", spawnID, serr)
			}
			return
		}
		// Pin the same node the client signed for.
		placement.TargetNodeID = targetNodeID
	}

	// Fresh create: base_image_digest is unknown until the node resolves it at create time.
	// Pass "" so the node resolves and records the digest on first startup (spec §4).
	nodeID, err := s.sched.Provision(ctx, spawnID, appRef, model, sp.Name, sp.AppID, sp.RunnableID, sp.Mode, 1, placement, env, "", nil)
	if err != nil {
		log.Printf("provisionSpawn %s: provision failed: %v", spawnID, err)
		if serr := s.st.Spawns().SetError(ctx, spawnID); serr != nil {
			log.Printf("provisionSpawn %s: SetError after provision failure also failed: %v", spawnID, serr)
		}
		return
	}
	if err := s.st.Spawns().SetActive(ctx, spawnID, nodeID, 1); err != nil {
		s.rt.StopOnNode(spawnID)
		s.rt.Drop(spawnID)
		if serr := s.st.Spawns().SetError(ctx, spawnID); serr != nil {
			log.Printf("provisionSpawn %s: SetError after SetActive failure also failed: %v", spawnID, serr)
		}
		return
	}
	// Fresh pod started with spawns.model -> the running model matches the record. Converge the flag
	// (store.Create already sets it true for new spawns; idempotent here).
	if merr := s.st.Spawns().MarkModelApplied(ctx, spawnID); merr != nil {
		log.Printf("provisionSpawn %s: MarkModelApplied after provision: %v", spawnID, merr)
	}
}

// placementFor computes node placement for a spawn of the given app version. Apps run anywhere
// regardless of review status — the only node distinction is TENANCY (cloud=multi-tenant,
// self-hosted=owner-only), enforced by registry.PickFor on the spawn owner. The review tier only gates
// app-spawn AUTHORIZATION: an unverified/unknown version may be run only by its author (PermissionDenied
// otherwise). Shared by CreateSpawn and ResumeSpawn.
func (s *Server) placementFor(ctx context.Context, owner, appID string, ver store.AppVersion) (registry.Placement, error) {
	if ver.Tier != store.TierReviewed && ver.Tier != store.TierScanned {
		creator, err := s.st.Apps().Creator(ctx, appID)
		if err != nil {
			return registry.Placement{}, connect.NewError(connect.CodeInternal, err)
		}
		if creator != owner {
			return registry.Placement{}, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("only the author can run an unverified version of %s", appID))
		}
	}
	return registry.Placement{Owner: owner}, nil
}

func (s *Server) StopSpawn(ctx context.Context, req *connect.Request[cpv1.StopSpawnRequest]) (*connect.Response[cpv1.StopSpawnResponse], error) {
	owner, ok := auth.OwnerFromContext(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("no owner"))
	}
	if err := s.stop(ctx, owner, req.Msg.SpawnId); err != nil {
		return nil, err
	}
	return connect.NewResponse(&cpv1.StopSpawnResponse{}), nil
}

// stop validates ownership via the store, tells the node to destroy the pod, drops the route, and
// soft-deletes the spawn (today's StopSpawn is a destroy; suspend is Part 3).
func (s *Server) stop(ctx context.Context, owner, spawnID string) error {
	unlock := s.locks.Lock(spawnID)
	defer unlock()
	sp, err := s.st.Spawns().Get(ctx, spawnID)
	if err != nil {
		return connect.NewError(connect.CodeNotFound, fmt.Errorf("unknown spawn"))
	}
	if sp.OwnerID != owner {
		return connect.NewError(connect.CodePermissionDenied, fmt.Errorf("not your spawn"))
	}
	s.rt.StopOnNode(spawnID)
	s.rt.Drop(spawnID)
	if err := s.st.Spawns().MarkDeleted(ctx, spawnID, time.Now().Unix()); err != nil {
		return connect.NewError(connect.CodeInternal, err)
	}
	_ = s.tel.Emit(telemetry.Event{Kind: "session_end", Owner: owner, SpawnID: spawnID, Timestamp: time.Now().UTC()})
	return nil
}

// --- A4 two-phase intent RPCs [AC1] ----------------------------------------

// GetPendingIntent returns the CP-committed tuple for an in-flight lifecycle op so the client
// can validate and sign it. Returns ready=true + the pending tuple when the lifecycle handler
// has registered it; ready=false if not yet registered (client should poll).
func (s *Server) GetPendingIntent(ctx context.Context, req *connect.Request[cpv1.GetPendingIntentRequest]) (*connect.Response[cpv1.GetPendingIntentResponse], error) {
	owner, ok := auth.OwnerFromContext(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("no owner"))
	}
	sp, err := s.st.Spawns().Get(ctx, req.Msg.SpawnId)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("unknown spawn"))
	}
	if sp.OwnerID != owner {
		return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("not your spawn"))
	}
	pi, ready := s.pendingIntents.get(req.Msg.SpawnId)
	return connect.NewResponse(&cpv1.GetPendingIntentResponse{Pending: pi, Ready: ready}), nil
}

// SubmitIntent delivers the client's SignedIntent + node access token, unblocking the pending
// provision. The node_access_token is the aud=node AS-signed token bound to the client's
// session key (the cnf claim). Both are threaded verbatim into StartSpawn as an AuthEnvelope.
//
// In dev mode (devASKey set), if node_access_token is empty the CP mints a cnf-bearing aud=node
// token from the intent's SPKI DER so the full A4 verification chain can run at the node in
// verify-and-log mode (NODE_AUTH_MODE=insecure) [AM12].
func (s *Server) SubmitIntent(ctx context.Context, req *connect.Request[cpv1.SubmitIntentRequest]) (*connect.Response[cpv1.SubmitIntentResponse], error) {
	owner, ok := auth.OwnerFromContext(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("no owner"))
	}
	sp, err := s.st.Spawns().Get(ctx, req.Msg.SpawnId)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("unknown spawn"))
	}
	if sp.OwnerID != owner {
		return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("not your spawn"))
	}

	nodeTok := req.Msg.NodeAccessToken
	// Dev-mode cnf-bearing node token minting [AM12]: if the client omits the node token and a dev
	// AS key is configured, mint one bound to the intent's SPKI DER so the node can run the full
	// eight-step verification chain in AuthModeVerifyLog (verify-and-log, not skip).
	if nodeTok == "" && s.devASKey != nil && req.Msg.Intent != nil && len(req.Msg.Intent.SpkiDer) > 0 {
		minted, mintErr := token.MintNode(s.devASKey, s.devASKeyID, owner, req.Msg.Intent.SpkiDer, s.now())
		if mintErr != nil {
			log.Printf("SubmitIntent %s: dev AS mint failed: %v (node token empty)", req.Msg.SpawnId, mintErr)
		} else {
			nodeTok = minted
		}
	}

	env := &authv1.AuthEnvelope{
		AccessToken: nodeTok,
		Intent:      req.Msg.Intent,
	}
	if err := s.pendingIntents.submit(req.Msg.SpawnId, owner, env); err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	return connect.NewResponse(&cpv1.SubmitIntentResponse{}), nil
}

// mintSessionEnv builds the session-open AuthEnvelope from a client-supplied envelope.
// In dev mode (devASKey set), if access_token is empty the CP mints a cnf-bearing aud=node
// token from the intent's SPKI DER so the full A4 verification chain runs [AM12].
func (s *Server) mintSessionEnv(owner string, sa *authv1.AuthEnvelope) *authv1.AuthEnvelope {
	if sa == nil || sa.Intent == nil {
		return nil
	}
	nodeTok := sa.AccessToken
	if nodeTok == "" && s.devASKey != nil && len(sa.Intent.SpkiDer) > 0 {
		minted, err := token.MintNode(s.devASKey, s.devASKeyID, owner, sa.Intent.SpkiDer, s.now())
		if err != nil {
			log.Printf("mintSessionEnv: dev AS mint failed: %v", err)
		} else {
			nodeTok = minted
		}
	}
	return &authv1.AuthEnvelope{AccessToken: nodeTok, Intent: sa.Intent}
}

// buildPendingIntent constructs the cp.v1.PendingIntent from the committed provision tuple.
// mounts comes from the store's mount list for the spawn (may be nil for CreateSpawn).
func buildPendingIntent(op intent.Op, spawnID string, gen uint64, targetNodeID, image, appRef, model, dataRef string, mounts []store.Mount) *cpv1.PendingIntent {
	pi := &cpv1.PendingIntent{
		Op:           string(op),
		SpawnId:      spawnID,
		Generation:   gen,
		TargetNodeId: targetNodeID,
		Image:        image,
		AppRef:       appRef,
		Model:        model,
		DataRef:      dataRef,
	}
	for _, m := range mounts {
		pi.Mounts = append(pi.Mounts, &cpv1.MountBinding{Name: m.Name, BackendUri: m.BackendURI})
	}
	return pi
}

// --- client-facing session RPCs (answered from CP's mirrored roster) ------

// node and cp SessionTransport enums share ordinals by construction (see the protos), so the cast is total.
func toNodeTransport(t cpv1.SessionTransport) nodev1.SessionTransport {
	return nodev1.SessionTransport(t)
}
func toCPTransport(t nodev1.SessionTransport) cpv1.SessionTransport { return cpv1.SessionTransport(t) }

func sessionStateString(st nodev1.SessionState) string {
	switch st {
	case nodev1.SessionState_SESSION_STATE_STARTING:
		return "starting"
	case nodev1.SessionState_SESSION_STATE_ACTIVE:
		return "active"
	case nodev1.SessionState_SESSION_STATE_CLOSING:
		return "closing"
	case nodev1.SessionState_SESSION_STATE_CLOSED:
		return "closed"
	case nodev1.SessionState_SESSION_STATE_ERROR:
		return "error"
	default:
		return "unspecified"
	}
}

// ownSpawn loads a spawn and verifies the caller owns it (shared by the session RPCs).
func (s *Server) ownSpawn(ctx context.Context, spawnID string) error {
	owner, ok := auth.OwnerFromContext(ctx)
	if !ok {
		return connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("no owner"))
	}
	sp, err := s.st.Spawns().Get(ctx, spawnID)
	if err != nil {
		return connect.NewError(connect.CodeNotFound, fmt.Errorf("unknown spawn"))
	}
	if sp.OwnerID != owner {
		return connect.NewError(connect.CodePermissionDenied, fmt.Errorf("not your spawn"))
	}
	return nil
}

// ListSessions returns the CP's mirrored roster for a spawn (no node round-trip).
func (s *Server) ListSessions(ctx context.Context, req *connect.Request[cpv1.ListSessionsRequest]) (*connect.Response[cpv1.ListSessionsResponse], error) {
	if err := s.ownSpawn(ctx, req.Msg.SpawnId); err != nil {
		return nil, err
	}
	mirror := s.rt.ListSessions(req.Msg.SpawnId)
	out := make([]*cpv1.SessionDescriptor, 0, len(mirror))
	for _, si := range mirror {
		out = append(out, &cpv1.SessionDescriptor{
			SessionId: si.SessionId, Transport: toCPTransport(si.Transport), Runnable: si.Runnable,
			Status: sessionStateString(si.State), Pinned: si.Pinned,
		})
	}
	return connect.NewResponse(&cpv1.ListSessionsResponse{Sessions: out}), nil
}

// CreateSession asks the hosting node to launch an additional session (node allocates the id).
func (s *Server) CreateSession(ctx context.Context, req *connect.Request[cpv1.CreateSessionRequest]) (*connect.Response[cpv1.CreateSessionResponse], error) {
	if err := s.ownSpawn(ctx, req.Msg.SpawnId); err != nil {
		return nil, err
	}
	if req.Msg.Transport == cpv1.SessionTransport_SESSION_TRANSPORT_UNSPECIFIED {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("transport is required"))
	}
	if req.Msg.Runnable == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("runnable is required"))
	}
	if err := s.rt.CreateSession(req.Msg.SpawnId, toNodeTransport(req.Msg.Transport), req.Msg.Runnable); err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, err)
	}
	return connect.NewResponse(&cpv1.CreateSessionResponse{}), nil
}

// CloseSession asks the hosting node to reap one session. Session "0" is pinned (reject; stop the spawn).
func (s *Server) CloseSession(ctx context.Context, req *connect.Request[cpv1.CloseSessionRequest]) (*connect.Response[cpv1.CloseSessionResponse], error) {
	if err := s.ownSpawn(ctx, req.Msg.SpawnId); err != nil {
		return nil, err
	}
	// Default empty -> "0" so a missing session_id is rejected as pinned rather than silently succeeding.
	sessionID := req.Msg.SessionId
	if sessionID == "" {
		sessionID = "0" // node.SessionZeroID, inlined (wire-stable) to avoid an import cycle
	}
	if sessionID == "0" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("session #0 is pinned; stop the spawn instead"))
	}
	if err := s.rt.CloseSession(req.Msg.SpawnId, sessionID); err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, err)
	}
	return connect.NewResponse(&cpv1.CloseSessionResponse{}), nil
}

func (s *Server) Session(ctx context.Context, stream *connect.BidiStream[cpv1.Frame, cpv1.Frame]) error {
	first, err := stream.Receive()
	if err != nil {
		return err
	}
	spawnID := first.SpawnId
	identity, _ := auth.IdentityFromContext(ctx)
	owner := identity.Owner
	if owner == "" {
		owner, _ = auth.OwnerFromContext(ctx)
	}
	sp, err := s.st.Spawns().Get(ctx, spawnID)
	if err != nil {
		return connect.NewError(connect.CodeNotFound, fmt.Errorf("unknown spawn"))
	}
	if owner != sp.OwnerID {
		return connect.NewError(connect.CodePermissionDenied, fmt.Errorf("not your spawn"))
	}

	// Per-session context for revocation cancellation.
	sessCtx, sessCancel := context.WithCancel(ctx)
	defer sessCancel()

	// Track current session registration; swapped on token rotation to release the old id promptly.
	var (
		curRelMu   sync.Mutex
		curRelease func()
	)
	if s.sessions != nil {
		curRelease = s.sessions.Add(identity.TokenID, identity.Owner, sessCancel)
	}
	defer func() {
		curRelMu.Lock()
		defer curRelMu.Unlock()
		if curRelease != nil {
			curRelease()
		}
	}()

	// Reauth deadline: AS-token sessions in prod close if no reauth received in time.
	reauthInterval := s.reauthInterval
	if reauthInterval <= 0 {
		reauthInterval = defaultReauthInterval
	}
	isProdAS := identity.TokenID != "" && !s.devMode
	var reauthTimer *time.Timer
	if isProdAS {
		reauthTimer = time.NewTimer(reauthInterval + reauthGrace)
	}
	var reauthCh <-chan time.Time
	if reauthTimer != nil {
		reauthCh = reauthTimer.C
		defer reauthTimer.Stop()
	}

	clientID := uuid.Must(uuid.NewV7()).String()
	cs := &clientStream{stream: stream, spawnID: spawnID}
	// A4: thread session-open AuthEnvelope from the bind frame into SessionOpen [AC1].
	var sessionEnv *authv1.AuthEnvelope
	if sa := first.GetSessionAuth(); sa != nil {
		sessionEnv = s.mintSessionEnv(owner, sa)
	}
	// cursor 0: the cp.v1 Session-RPC transport has no resume cursor (only the WS bind does).
	// session "0": this transport has no per-session selector yet (web uses the WS bind for that).
	done, err := s.rt.AttachClient(spawnID, "0", clientID, sp.OwnerID, sessionEnv, cs, 0)
	if err != nil {
		return connect.NewError(connect.CodeInternal, err)
	}
	_ = s.tel.Emit(telemetry.Event{Kind: "session_start", Owner: sp.OwnerID, SpawnID: spawnID, Timestamp: time.Now().UTC()})
	defer func() {
		s.rt.DetachClient(spawnID, "0", clientID)
		_ = s.tel.Emit(telemetry.Event{Kind: "session_end", Owner: sp.OwnerID, SpawnID: spawnID, Timestamp: time.Now().UTC()})
	}()

	if len(first.Data) > 0 {
		_ = s.rt.FromClient(spawnID, "0", clientID, first.Data)
	}
	recvErr := make(chan error, 1)
	go func() {
		for {
			f, err := stream.Receive()
			if err != nil {
				recvErr <- err
				return
			}
			// Reauth control frame: consume, verify, re-register. Never forwarded to FromClient.
			if f.ReauthToken != "" {
				if s.verify != nil {
					newID, verr := s.verify(f.ReauthToken)
					if verr != nil || newID.Owner != owner {
						if !s.devMode {
							recvErr <- connect.NewError(connect.CodePermissionDenied, fmt.Errorf("reauth failed"))
							return
						}
						log.Printf("session reauth failed (dev-tolerant): %v", verr)
					} else {
						// Re-register under new token_id; release old so only one id is active.
						if s.sessions != nil && newID.TokenID != "" && newID.TokenID != identity.TokenID {
							newRel := s.sessions.Add(newID.TokenID, newID.Owner, sessCancel)
							curRelMu.Lock()
							if curRelease != nil {
								curRelease()
							}
							curRelease = newRel
							curRelMu.Unlock()
						}
						// Reset the reauth deadline.
						if reauthTimer != nil {
							if !reauthTimer.Stop() {
								select {
								case <-reauthTimer.C:
								default:
								}
							}
							reauthTimer.Reset(reauthInterval + reauthGrace)
						}
						identity = newID
					}
				}
				continue
			}
			if ferr := s.rt.FromClient(spawnID, "0", clientID, f.Data); ferr != nil {
				recvErr <- ferr
				return
			}
		}
	}()
	select {
	case <-done:
		return nil
	case <-recvErr:
		return nil
	case <-sessCtx.Done():
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-reauthCh:
		return connect.NewError(connect.CodePermissionDenied, fmt.Errorf("reauth timeout"))
	}
}

// clientStream is the CP->client sender for the router.
type clientStream struct {
	stream  *connect.BidiStream[cpv1.Frame, cpv1.Frame]
	spawnID string
}

func (c *clientStream) Send(b []byte) error {
	return c.stream.Send(&cpv1.Frame{SpawnId: c.spawnID, Data: b})
}
