package spawnlet

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"spawnery/internal/agentcaps"
	"spawnery/internal/manifest"
	"spawnery/internal/runtime"
	"spawnery/internal/spawnlet/firewall"
	"spawnery/internal/storage"
	"spawnery/internal/storage/journal"
)

// journalKeyDeliveryTimeout bounds how long an owner-sealed resume waits for the
// owner to deliver the repo password before falling back to the seeded dir
// (design §5 M8 — a defined, non-hang state). The interactive owner ceremony is
// expected to complete in seconds; the migrate slice (sp-u53.5.3) drives the
// full back-to-suspended timeout state machine.
const journalKeyDeliveryTimeout = 30 * time.Second

type ManagerConfig struct {
	AgentImage, SidecarImage, OpenRouterKey, DataRoot string

	// SecretsRoot is the per-node root for owner-sealed secret tmpfs dirs (design §6). Each spawn gets
	// a subdir here, bind-mounted into the agent at SecretsMountPath; the node writes unsealed plaintext
	// into it (0600). Default DataRoot/secrets. Production should point this at a tmpfs (memory-backed)
	// so plaintext never touches durable disk.
	SecretsRoot string
	SidecarPort int // default 8080

	NodeID           string // this node's id (stamped on container labels for reconcile); "" standalone
	NodeClass        string // "cloud" (always enforces) or "self-hosted" (honors EgressEnforce)
	EgressEnforce    bool   // self-hosted opt-out switch; ignored on cloud
	EgressAllowCIDRs []string

	MemLimitMB       int64   // memory limit in MiB; default 1024
	CPULimit         float64 // CPU cores; default 1.0
	PidsLimit        int64   // max pids per container; default 256
	ContainerRuntime string  // OCI runtime name; "" = Docker default
	DeltaCapture     bool    // if true, capture agent rootfs delta on suspend (DELTA_CAPTURE=1)
	AdvertiseIP      string  // node IP mosh advertises to spawnctl for terminal attach ("" => auto)

	// UsernsMode controls the Linux user-namespace isolation posture (spec §2).
	// "remap"  — Docker daemon runs with userns-remap; agent gets the default capability set.
	// "native" — runsc/gVisor sentry provides isolation; agent gets the default capability set.
	// "off"    — no kernel user-namespace isolation; agent is cap-drop=ALL (degraded, default).
	// cmd/spawnlet sets this from USERNS_MODE; buildManager probes + may downgrade to "off".
	UsernsMode string
	// UsernsRemapBase is the sub-UID base that the userns-remap daemon uses (0 when not in
	// remap mode). Learned at startup by probing docker info; exposed via RemapBase() for
	// the storage layer to compute host-side ownership (spec §2, task .8).
	UsernsRemapBase uint32

	// DeltaSquashDepth is the number of suspend captures after which the manager
	// surfaces a SQUASH-NEEDED warning (default 16). Squash execution is deferred
	// until a backend layer-export method is available; the warning surfaces the
	// growing chain so operators know when to intervene.
	// 0 → use default (16). Set to a very large value to disable.
	DeltaSquashDepth int

	// DeltaScrubPaths are path prefixes (absolute) exec-scrubbed from the agent
	// container via `rm -rf` BEFORE each CaptureDelta commit (live Docker-lane
	// capture-time scrub; best-effort, non-fatal).  The deltamerge package
	// applies the same filter during squash.  Default:
	// ["/var/cache/apt", "/var/lib/apt/lists", "/tmp"].
	DeltaScrubPaths []string

	// DeltaQuotaSoftMB: if > 0 and a spawn's captured delta image exceeds this
	// threshold, the watchdog suspends the spawn and logs a warning.
	// Precision: depends on the backend exposing DeltaSize (optional interface).
	// When DeltaSize is unavailable the quota is dormant (logged once).
	DeltaQuotaSoftMB int64

	// DeltaQuotaHardMB: if > 0 and a spawn's captured delta image exceeds this
	// threshold, the watchdog stops the spawn hard (no delta kept by Stop).
	DeltaQuotaHardMB int64
}

type Manager struct {
	pod     runtime.PodBackend
	cfg     ManagerConfig
	store   *Store
	backend storage.Backend
	fw      firewall.Applier
	// journal is the transient-tier journaler (node-local Kopia). nil disables
	// journaling entirely — scratch-only behavior is unchanged. Set via
	// SetJournal. The seam is exercised only for mounts whose durability class is
	// node-local/owner-sealed (design §1a); ephemeral mounts never touch it.
	journal journal.JournalManager
	// journalState durably pins per-mount manifest ids on suspend so a same-node
	// resume restores node-local journaled mounts without any CP protocol.
	journalState *journalStateStore
	// journalKeys is the owner-sealed journal-key receiver (sp-u53.5.4): the node's
	// SecretDelivery handler routes a delivered repo password here so a cross-node
	// resume can open the Kopia repo. Set by SetJournal when the journaler also
	// implements JournalKeyReceiver (it does for *journal.Manager with an
	// OwnerSealed custody configured); nil otherwise (node-local-only journaling).
	journalKeys JournalKeyReceiver
	// secrets injects owner-sealed secret plaintext into each spawn's tmpfs secrets dir (design §6).
	// Always set (NewManagerWithBackend defaults SecretsRoot); the node calls InjectSecret after unseal.
	secrets SecretInjector

	// deltaState durably records the per-spawn delta chain depth across node restarts so
	// a resumed spawn continues counting from where it left off.
	deltaState *deltaStateStore

	// scrubFn is called BEFORE each CaptureDelta commit to remove noisy paths from the
	// agent container's writable layer (live capture-time scrub, Docker lane).  Default
	// (set by NewManagerWithBackend) execs `rm -rf <paths>` directly against the agentID
	// container without routing through ExecRun/store-lookup — the spawn has already been
	// claimed from the store (removed) by the time teardown calls scrubFn.
	// Injected as a seam in tests so the hermetic unit tests do not shell out to Docker.
	scrubFn func(ctx context.Context, agentID string, paths []string) error

	// squashNeeded is called when DeltaDepth reaches DeltaSquashDepth.
	// nil → log a "SQUASH-NEEDED" warning line.
	// Injected in tests so they can observe the callback without log parsing.
	squashNeeded func(id string, depth int)

	// quotaWarnedOnce guards the "no size source" dormant-quota log (emitted once per
	// manager lifetime, not on every CheckQuotas call).
	quotaWarnedOnce bool
}

// JournalKeyReceiver injects an owner-delivered Kopia repo password into the
// journaler's owner-sealed custody and lets the resume path wait for it before
// restore (transient-tier §4). *journal.Manager satisfies it; the spawnlet holds
// only this narrow seam so the broad JournalManager interface stays unchanged.
type JournalKeyReceiver interface {
	DeliverKey(spawnID string, gen uint64, password string) error
	WaitDelivered(ctx context.Context, spawnID string) error
	MarkOwnerSealed(spawnID string)
}

// SetJournal installs the transient-tier journaler (design §1b) plus the
// node-local state dir where per-spawn pinned manifest ids are recorded on
// suspend (so a same-node resume can restore with no CP protocol). Optional:
// when unset, every mount behaves as scratch-only (Ephemeral) and the journal
// seams in Create/Stop are no-ops.
func (m *Manager) SetJournal(j journal.JournalManager, stateDir string) {
	m.journal = j
	m.journalState = &journalStateStore{dir: stateDir}
	// Capture the owner-sealed journal-key receiver if this journaler provides one
	// (a *journal.Manager with an OwnerSealed custody). Used by the node's
	// SecretDelivery handler and the cross-node resume restore wait.
	if r, ok := j.(JournalKeyReceiver); ok {
		m.journalKeys = r
	}
}

// DeliverJournalKey injects an owner-delivered Kopia repo password for spawnID at
// generation gen into the journaler's owner-sealed custody. The node's
// SecretDelivery handler calls this for a journal-key secret (journalkey.Prefix)
// after OpenDelivered. It does NOT require the spawn to be live in the store: on a
// cross-node resume the key arrives BEFORE the pod (and thus before the journal
// restore that consumes it). Errors if no owner-sealed journaler is configured.
func (m *Manager) DeliverJournalKey(spawnID string, gen uint64, password string) error {
	if m.journalKeys == nil {
		return fmt.Errorf("journal key delivery: no owner-sealed journaler configured")
	}
	return m.journalKeys.DeliverKey(spawnID, gen, password)
}

// NewManager builds a Manager on the Docker/runc path: the Docker pod backend + the DOCKER-USER
// egress floor. (cmd/spawnlet uses NewManagerWithBackend for the runsc/CRI path.)
func NewManager(rt runtime.ContainerRuntime, cfg ManagerConfig) *Manager {
	return NewManagerWithBackend(
		runtime.NewDockerPodBackend(rt, cfg.ContainerRuntime, cfg.AgentImage),
		firewall.HostFloorApplier{},
		cfg,
	)
}

// NewManagerWithBackend builds a Manager around an explicit pod backend + egress-floor applier,
// applying config defaults. Used to select the runc (Docker + DOCKER-USER) vs runsc (CRI +
// SPAWNLET-EGRESS) stacks by CONTAINER_RUNTIME.
func NewManagerWithBackend(pod runtime.PodBackend, fw firewall.Applier, cfg ManagerConfig) *Manager {
	if cfg.SidecarPort == 0 {
		cfg.SidecarPort = 8080
	}
	if cfg.MemLimitMB == 0 {
		cfg.MemLimitMB = 1024
	}
	if cfg.CPULimit == 0 {
		cfg.CPULimit = 1.0
	}
	if cfg.PidsLimit == 0 {
		cfg.PidsLimit = 256
	}
	if cfg.SecretsRoot == "" {
		cfg.SecretsRoot = filepath.Join(cfg.DataRoot, "secrets")
	}
	if cfg.DeltaSquashDepth == 0 {
		cfg.DeltaSquashDepth = 16
	}
	if len(cfg.DeltaScrubPaths) == 0 {
		cfg.DeltaScrubPaths = []string{"/var/cache/apt", "/var/lib/apt/lists", "/tmp"}
	}
	m := &Manager{
		pod:        pod,
		cfg:        cfg,
		store:      NewStore(),
		backend:    storage.NewScratch(cfg.DataRoot),
		fw:         fw,
		secrets:    SecretInjector{Root: cfg.SecretsRoot},
		deltaState: &deltaStateStore{dir: filepath.Join(cfg.DataRoot, "delta-state")},
	}
	// Default scrub function: exec `rm -rf <paths>` directly in the agent container before commit.
	// This runs while the container is still live (before pod.Stop).
	// IMPORTANT: we exec by agentID directly — NOT via ExecRun — because by the time teardown
	// calls scrubFn the spawn has already been removed from the store by Claim (in Stop/Suspend/
	// Delete), so ExecRun's store.Get would always return "no agent container".
	// The seam allows unit tests to inject a fake without shelling out to Docker.
	m.scrubFn = func(ctx context.Context, agentID string, paths []string) error {
		if agentID == "" {
			return fmt.Errorf("scrub: no agent container id")
		}
		args := append([]string{"rm", "-rf"}, paths...)
		argv := execArgv(ExecPrefixNonInteractiveFor(m.cfg.ContainerRuntime), agentID, args)
		out, err := exec.CommandContext(ctx, argv[0], argv[1:]...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("exec %v: %w (%s)", args, err, out)
		}
		return nil
	}
	return m
}

// egressEnforced reports whether the egress floor must be applied: cloud nodes always enforce
// (non-disableable); self-hosted honors the operator's EgressEnforce choice.
func (m *Manager) egressEnforced() bool {
	return m.cfg.NodeClass == "cloud" || m.cfg.EgressEnforce
}

func (m *Manager) Store() *Store { return m.store }

// RemapBase returns the userns-remap base UID learned at startup from the Docker daemon probe
// (spec §2). Returns 0 when USERNS_MODE is not "remap" or the probe found no active remap.
// Consumed by the storage layer (.8) to compute host-side ownership for userns-remapped mounts.
func (m *Manager) RemapBase() uint32 { return m.cfg.UsernsRemapBase }

// agentRootUID returns the host uid that the in-container agent-root maps to, used by
// the storage layer for host-side ownership of data mounts (spec §5): remap lane = the
// learned sub-uid base; native (runsc) lane = 0 (container uids are literal there);
// off/degraded = -1 (unknown — storage keeps the world-writable fallback).
func (m *Manager) agentRootUID() int {
	switch m.cfg.UsernsMode {
	case "remap":
		return int(m.cfg.UsernsRemapBase)
	case "native":
		return 0
	default:
		return -1
	}
}

// ExecPrefix returns the runtime exec invocation (docker/crictl exec -it ...) for execing into a
// spawn's agent container — used by the node's tmux raw-PTY relay.
func (m *Manager) ExecPrefix() []string { return ExecPrefixFor(m.cfg.ContainerRuntime) }

// TmuxAttachArgv returns the full argv to `docker/crictl exec -it <containerID> tmux attach -t
// <session>` — used by the node's per-client tmux raw-PTY relay to construct the exec command.
// Keeps execArgv unexported.
func (m *Manager) TmuxAttachArgv(containerID, session string) []string {
	return execArgv(ExecPrefixFor(m.cfg.ContainerRuntime), containerID, []string{"tmux", "attach", "-t", session})
}

// TmuxAttachArgvFor resolves spawnID's agent container and returns the argv to `exec -it <container>
// tmux attach -t <session>` — the per-(spawn,session) mosh relay attach for an additional session
// (sp-npxq.3). Like TmuxAttachArgv but spawn-id keyed (the node holds the spawn id, not the Spawn).
func (m *Manager) TmuxAttachArgvFor(spawnID, session string) ([]string, error) {
	sp, ok := m.store.Get(spawnID)
	if !ok || sp.AgentID == "" {
		return nil, fmt.Errorf("spawn %s has no agent container", spawnID)
	}
	return m.TmuxAttachArgv(sp.AgentID, session), nil
}

// ExecRun runs inner non-interactively in spawnID's agent container, to completion (sp-npxq.3). Used
// to create/reap additional sessions: launcher tmux-create (mosh), tmux-wrapped acp launcher, and
// `tmux kill-session`. All return promptly (tmux new-session -d / kill-session exit immediately; the
// mosh launcher exits after detaching its tmux session).
func (m *Manager) ExecRun(ctx context.Context, spawnID string, inner []string) error {
	sp, ok := m.store.Get(spawnID)
	if !ok || sp.AgentID == "" {
		return fmt.Errorf("spawn %s has no agent container", spawnID)
	}
	argv := execArgv(ExecPrefixNonInteractiveFor(m.cfg.ContainerRuntime), sp.AgentID, inner)
	out, err := exec.CommandContext(ctx, argv[0], argv[1:]...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("exec %v: %w (%s)", inner, err, out)
	}
	return nil
}

// AttachACPPort dials an additional acp session's in-pod ACP endpoint at podIP:port (sp-npxq.3),
// parallel to Attach's session-#0 podIP:7000 dial. The node opens an Nth Pump over the returned stream.
func (m *Manager) AttachACPPort(ctx context.Context, spawnID string, port int) (*runtime.AttachedStream, error) {
	sp, ok := m.store.Get(spawnID)
	if !ok {
		return nil, fmt.Errorf("spawn not found: %s", spawnID)
	}
	if sp.PodIP == "" {
		return nil, fmt.Errorf("spawn %s has no pod IP (rootless-without-bridge unsupported for TCP ACP)", spawnID)
	}
	return runtime.AttachTCP(ctx, net.JoinHostPort(sp.PodIP, strconv.Itoa(port)))
}

// Attach returns the agent's ACP stdio for a spawn, dispatching to the backend's transport (Docker
// stdio attach for the Docker lane, the in-pod UDS for the CRI lane).
func (m *Manager) Attach(ctx context.Context, sp *Spawn) (*runtime.AttachedStream, error) {
	return m.pod.Attach(ctx, &runtime.PodHandle{
		PodIP:     sp.PodIP,
		AgentID:   sp.AgentID,
		NetnsPath: sp.NetnsPath,
		SidecarID: sp.SidecarID,
		SandboxID: sp.SandboxID,
	})
}

// SpawnGeneration returns the generation of a live spawn (and whether it is tracked), so callers can
// fence stale-generation control messages against the container actually running.
func (m *Manager) SpawnGeneration(id string) (uint64, bool) {
	sp, ok := m.store.Get(id)
	if !ok {
		return 0, false
	}
	return sp.Generation, true
}

// RunningInventory returns the spawns this node currently manages (id + generation), for the CP
// reconcile carried on Register/Heartbeat.
func (m *Manager) RunningInventory() []runtime.ManagedPod {
	sps := m.store.List()
	out := make([]runtime.ManagedPod, 0, len(sps))
	for _, sp := range sps {
		out = append(out, runtime.ManagedPod{SpawnID: sp.ID, Generation: sp.Generation, NodeID: m.cfg.NodeID})
	}
	return out
}

// ReapOrphans tears down every spawnery-managed pod the runtime still has that this Manager is NOT
// tracking — leftovers from a previous node process (the in-mem store is empty after a restart). Call
// it at startup so a crashed/restarted node doesn't leak pods.
//
// Scoped by the spawnery.node-id label: pods created by a DIFFERENT node id are left alone — two
// spawnlets sharing one Docker daemon (dev stack + an e2e run, or multi-node-on-one-host) must not
// reap each other's live pods. Unlabeled pods (pre-label versions) are still reaped.
//
// Crash-survival (spec §4): when DeltaCapture is enabled, a CaptureDelta is attempted on the
// orphaned agent container BEFORE pod.Stop, so the spawn's work is preserved for a future resume.
// This is best-effort and non-fatal — a capture failure just means the next resume starts from the
// last known-good delta (or the base image if none existed).
//
// moby#47065 note: the moby layer-count guard in CaptureDelta requires the BaseImageRef of the
// launch image to compare against.  Orphan reaping does not have the Spawn record (the in-mem store
// was wiped on restart), so BaseImageRef is empty and the guard degrades to rejecting only truly
// zero-layer commits.
func (m *Manager) ReapOrphans(ctx context.Context) error {
	managed, err := m.pod.ListManaged(ctx)
	if err != nil {
		return err
	}
	for _, mp := range managed {
		if mp.NodeID != "" && mp.NodeID != m.cfg.NodeID {
			continue // another node's pod (shared daemon) — not ours to reap
		}
		if _, live := m.store.Get(mp.SpawnID); live {
			continue // still ours
		}
		log.Printf("reaping orphaned pod spawn=%s gen=%d (not in store; node restart)", mp.SpawnID, mp.Generation)

		// Capture-before-reap (spec §4 crash-survival): commit the agent's writable layer to
		// the delta tag BEFORE stopping so a future same-node resume picks up where it crashed.
		// Best-effort: non-fatal, logged.
		if m.cfg.DeltaCapture && mp.AgentID != "" {
			h := &runtime.PodHandle{SpawnID: mp.SpawnID, AgentID: mp.AgentID}
			if ref, cerr := m.pod.CaptureDelta(ctx, h); cerr != nil {
				log.Printf("capture-before-reap spawn=%s: %v (non-fatal; delta may be stale)", mp.SpawnID, cerr)
			} else {
				log.Printf("capture-before-reap spawn=%s ref=%s", mp.SpawnID, ref)
			}
		}

		_ = m.pod.Stop(ctx, &runtime.PodHandle{SidecarID: mp.SidecarID, AgentID: mp.AgentID, SandboxID: mp.SandboxID})
	}
	return nil
}

// StopAll tears down every spawn this Manager tracks, for graceful node shutdown — a SIGTERM'd node
// reaps its own pods instead of leaving orphans for the next process's reap-on-startup. Returns the
// number of spawns it stopped.
func (m *Manager) StopAll(ctx context.Context) int {
	sps := m.store.List()
	for _, sp := range sps {
		if err := m.Stop(ctx, sp.ID); err != nil {
			log.Printf("stopAll: stop %s: %v", sp.ID, err)
		}
	}
	return len(sps)
}

// AgentSelection is the per-spawn agent choice resolved by the CP. A zero value means "no selection"
// (use the node's configured image + the image's default command), preserving legacy behavior.
type AgentSelection struct {
	Image      string
	RunnableID string
	Mode       string
	// BaseImageDigest is the CP-pinned base image digest for cross-node resume (spec §4).
	// Empty on fresh create (the node resolves the digest at create time via ResolveImageDigest).
	// On resume/recreate the CP threads the stored digest down so the node uses the exact base.
	BaseImageDigest string
	// RootfsSourceGeneration and RootfsArtifacts are CP-pinned migration restore inputs.
	// Normal same-node resume leaves them empty and continues to use the local DeltaImageRef.
	RootfsSourceGeneration uint64
	RootfsArtifacts        []RootfsArtifact
}

// RootfsArtifact is the node/spawnlet-facing copy of a journal rootfs artifact descriptor.
// It deliberately carries explicit generation and artifact id; callers must never ask the
// journaler for "latest" during migration restore.
type RootfsArtifact struct {
	ArtifactID       string
	Generation       uint64
	Sequence         int
	BaseImageDigest  string
	Format           string
	ContentDigest    string
	UncompressedSize int64
	ProducerNodeID   string
	ProducerRuntime  string
}

type SuspendResult struct {
	MountMarkers    map[string]string
	RootfsArtifacts []RootfsArtifact
}

func (m *Manager) Create(ctx context.Context, id, appPath, model, name, appID string, generation uint64) (*Spawn, error) {
	return m.CreateWithSelection(ctx, id, appPath, model, name, appID, generation, AgentSelection{})
}

// CreateWithSelection is Create plus an explicit agent selection (image + runnable id + mode).
// For any selected runnable the container command is set to [sel.RunnableID]; the image's
// dispatcher entrypoint (entrypoint.sh) resolves the actual launch (serve+adapter, tmux-wrapped
// TUI, etc.) — the node just names the runnable. No selection leaves Cmd nil (image default).
func (m *Manager) CreateWithSelection(ctx context.Context, id, appPath, model, name, appID string, generation uint64, sel AgentSelection) (*Spawn, error) {
	agentImage := m.cfg.AgentImage
	if sel.Image != "" {
		agentImage = sel.Image
	}
	var agentCmd []string
	if sel.RunnableID != "" {
		if _, ok := agentcaps.FindRunnable(sel.RunnableID); !ok {
			return nil, fmt.Errorf("unknown runnable %q", sel.RunnableID)
		}
		// The image's dispatcher entrypoint owns the actual launch (serve+adapter / tmux-wrapped TUI);
		// the node just names the runnable. (Replaces the old spawn-tmux + agentcaps.Launch prepend.)
		agentCmd = []string{sel.RunnableID}
	}

	if abs, err := filepath.Abs(appPath); err == nil {
		appPath = abs
	}
	mf, err := manifest.Parse(appPath)
	if err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	// The opencode session title shown in the TUI/web: the spawn's friendly name, with the app id
	// appended in brackets (session titles are single-line, so no newline). Prefer the CP-assigned
	// app id; fall back to the manifest id for the standalone lane (no CP). Either part may be empty;
	// the adapter falls back to a default if both are.
	app := appID
	if app == "" {
		app = mf.ID
	}
	sessionTitle := sessionTitle(name, app)

	// Labels identify this pod so a restarted node (or the CP) can reconcile it against the ledger and
	// reap orphans / fence stale generations. Applied to the sandbox + both containers.
	labels := map[string]string{
		runtime.LabelManaged:    "true",
		runtime.LabelSpawnID:    id,
		runtime.LabelGeneration: strconv.FormatUint(generation, 10),
	}
	if m.cfg.NodeID != "" {
		labels[runtime.LabelNodeID] = m.cfg.NodeID
	}

	// /app is read-only; each declared mount is a rw overlay at /app/<path>,
	// backed (slice: scratch) by a host dir seeded from /app/<seed>.
	mounts := []runtime.Mount{{HostPath: appPath, ContainerPath: "/app", ReadOnly: true}}
	var mountDirs []string
	var journalMounts []journal.Mount

	// Same-node resume (design §3, roast C1): if this spawn id has a durable
	// node-local journal record, this Create is a resume — restore each mount's
	// PINNED manifest into its (freshly seeded) host dir before bind. Absent
	// record = fresh create; mounts fall back to the seeded scratch dir.
	var jrec journalRecord
	var haveJournalRecord bool
	if m.journal != nil && m.journalState != nil {
		if rec, ok, lerr := m.journalState.Load(id); lerr != nil {
			log.Printf("journal state load for %s: %v", id, lerr)
		} else {
			jrec, haveJournalRecord = rec, ok
		}
	}

	finalizeAll := func() {
		for _, d := range mountDirs {
			_ = m.backend.Finalize(ctx, d)
		}
	}
	agentUID := m.agentRootUID()
	for _, mt := range mf.Storage.Mounts {
		seedDir := filepath.Join(appPath, mt.Seed)
		hostDir, err := m.backend.Prepare(ctx, id, mt.Name, seedDir, agentUID)
		if err != nil {
			finalizeAll()
			return nil, fmt.Errorf("prepare mount %q: %w", mt.Name, err)
		}

		// Transient-tier seam (design §1a/§3). Journaling only engages for mounts
		// that opt into a journaled durability class; ephemeral mounts (the
		// default) leave the scratch path entirely untouched.
		class, derr := journal.ParseDurability(mt.Durability)
		if derr != nil {
			finalizeAll()
			return nil, fmt.Errorf("mount %q durability: %w", mt.Name, derr)
		}
		jm := journal.Mount{Name: mt.Name, HostDir: hostDir, Class: class}
		if m.journal != nil && jm.Class.Journaled() {
			journalMounts = append(journalMounts, jm)
			// Owner-sealed mounts route the repo password to the owner-sealed
			// custody (delivered, not node-locally minted): mark the spawn so the
			// journaler never forks the repo under a fresh node-local key.
			if jm.Class == journal.OwnerSealed && m.journalKeys != nil {
				m.journalKeys.MarkOwnerSealed(id)
			}
			// Same-node resume: restore the pinned manifest recorded at the last
			// suspend into hostDir before bind (over the freshly seeded scratch
			// dir). Non-fatal: a restore failure falls back to the seeded dir and
			// surfaces the scratch-reset reality rather than aborting the spawn.
			// (The owner-sealed cross-node / migration pin is CP-threaded — that
			// remains TODO(phase②) and rides the StartSpawn protocol.)
			if haveJournalRecord {
				if pin, ok := jrec.Manifests[mt.Name]; ok {
					// Owner-sealed resume: the repo password is custodied by the owner,
					// not this node — wait (bounded) for it to be delivered over the
					// secret-delivery path before opening the repo for Restore (design
					// §4/§5). A timeout falls back to the seeded dir (a defined, non-hang
					// state); the full back-to-suspended timeout state machine rides the
					// migrate slice (sp-u53.5.3).
					if jm.Class == journal.OwnerSealed && m.journalKeys != nil {
						wctx, cancel := context.WithTimeout(ctx, journalKeyDeliveryTimeout)
						if werr := m.journalKeys.WaitDelivered(wctx, id); werr != nil {
							log.Printf("journal restore for %s mount %s: owner-sealed key not delivered: %v", id, mt.Name, werr)
							cancel()
							mountDirs = append(mountDirs, hostDir)
							mounts = append(mounts, runtime.Mount{HostPath: hostDir, ContainerPath: "/app/" + mt.Path})
							continue
						}
						cancel()
					}
					if rerr := m.journal.Restore(ctx, id, mt.Name, pin, hostDir); rerr != nil {
						log.Printf("journal restore for %s mount %s (manifest %s): %v", id, mt.Name, pin, rerr)
					} else {
						log.Printf("journal: spawn=%s mount=%s restored from manifest=%s", id, mt.Name, pin)
					}
				}
			}
		}

		mountDirs = append(mountDirs, hostDir)
		mounts = append(mounts, runtime.Mount{HostPath: hostDir, ContainerPath: "/app/" + mt.Path})
	}

	// Owner-sealed secrets tmpfs (design §6): a per-spawn dir under SecretsRoot, bind-mounted into the
	// agent at SecretsMountPath. The node writes unsealed plaintext here on SecretDelivery; the agent
	// reads its credentials in place. Created empty at start (secrets arrive over the delivery protocol,
	// not at spawn time) and removed on teardown. SecretsRoot should be a tmpfs in production.
	secretsDir := m.secrets.DirFor(id)
	if err := os.MkdirAll(secretsDir, 0o700); err != nil {
		finalizeAll()
		return nil, fmt.Errorf("prepare secrets dir: %w", err)
	}
	mounts = append(mounts, runtime.Mount{HostPath: secretsDir, ContainerPath: SecretsMountPath})

	res := runtime.Resources{
		MemoryBytes: m.cfg.MemLimitMB << 20,
		NanoCPUs:    int64(m.cfg.CPULimit * 1e9),
		PidsLimit:   m.cfg.PidsLimit,
	}
	addr := fmt.Sprintf("127.0.0.1:%d", m.cfg.SidecarPort)
	// Per-pod control plane: a random bearer token gates the sidecar's /control/model endpoint,
	// which the node POSTs to in order to switch the live model (runtime model switch, sp-bp9w).
	// SIDECAR_CONTROL_ADDR binds 0.0.0.0 (not loopback) because the pod IP is unknown until StartPod
	// returns, and the node reaches the sidecar over the pod bridge IP; the bearer token (not the
	// bind scope) is the access control, and the agent container cannot read the sidecar's env.
	controlToken := newControlToken()
	controlPort := m.cfg.SidecarPort + 1
	controlAddr := fmt.Sprintf("0.0.0.0:%d", controlPort)

	// Phase 1: sandbox + sidecar (the trusted, key-holding container).
	h, err := m.pod.StartPod(ctx, runtime.PodSpec{
		ID:           id,
		SidecarImage: m.cfg.SidecarImage,
		SidecarEnv: []string{
			"OPENROUTER_API_KEY=" + m.cfg.OpenRouterKey,
			"SIDECAR_ADDR=" + addr,
			"SIDECAR_CONTROL_TOKEN=" + controlToken,
			"SIDECAR_CONTROL_ADDR=" + controlAddr,
		},
		Resources: res,
		Runtime:   m.cfg.ContainerRuntime,
		Labels:    labels,
	})
	if err != nil {
		finalizeAll()
		return nil, err
	}

	// Egress floor: applied after the pod IP exists, before the untrusted agent starts (fail-closed).
	var floorIP string
	if m.egressEnforced() {
		if h.PodIP == "" {
			_ = m.pod.Stop(ctx, h)
			finalizeAll()
			return nil, fmt.Errorf("egress floor (fail-closed): no pod IP to scope the floor")
		}
		if ferr := m.fw.Apply(ctx, firewall.Rules(h.PodIP, m.cfg.EgressAllowCIDRs)); ferr != nil {
			_ = m.pod.Stop(ctx, h)
			finalizeAll()
			return nil, fmt.Errorf("egress floor (fail-closed): %w", ferr)
		}
		floorIP = h.PodIP
	}

	// Delta-survival image resolution (spec §4): runs AFTER the pod/floor are up (so a failure here
	// tears the pod down via the Stop+finalizeAll paths) and BEFORE StartAgent.
	//
	// baseRef: the base image tag/digest. If the CP threaded a pinned digest (cross-node resume),
	// use it; otherwise use the agentImage tag (fresh create or same-node resume).
	baseRef := agentImage
	if sel.BaseImageDigest != "" {
		baseRef = sel.BaseImageDigest
	}
	// Pin: resolve and record the digest (best-effort; non-fatal so dev daemons without
	// RepoDigests — which expose only an image ID — still spawn).
	baseDigest := sel.BaseImageDigest
	if baseDigest == "" {
		if dg, derr := m.pod.ResolveImageDigest(ctx, baseRef); derr == nil {
			baseDigest = dg
		} else {
			log.Printf("spawn %s: resolve base digest for %q: %v (non-fatal; delta-survival pinning skipped)", id, baseRef, derr)
		}
	}
	if len(sel.RootfsArtifacts) > 0 {
		if err := m.restoreRootfsArtifacts(ctx, id, sel.RootfsSourceGeneration, baseRef, sel.RootfsArtifacts); err != nil {
			_ = m.pod.Stop(ctx, h)
			finalizeAll()
			return nil, err
		}
	}
	// Launch image: delta tag if already present locally (same-node resume), else base.
	launchImage, eerr := m.pod.EnsureImage(ctx, baseRef, runtime.DeltaTag(id))
	if eerr != nil {
		_ = m.pod.Stop(ctx, h)
		finalizeAll()
		return nil, fmt.Errorf("ensure launch image: %w", eerr)
	}

	// Phase 2: the untrusted agent, into the existing pod.
	if err := m.pod.StartAgent(ctx, h, runtime.AgentSpec{
		Image: launchImage,
		Cmd:   agentCmd,
		Env: []string{
			"OPENAI_BASE_URL=http://" + addr + "/v1",
			"SPAWN_MODEL=" + model,
			"SPAWN_SESSION_TITLE=" + sessionTitle,
		},
		Mounts:      mounts,
		Resources:   res,
		Runtime:     m.cfg.ContainerRuntime,
		DropAllCaps: runtime.CapPolicyForUsernsMode(m.cfg.UsernsMode) == runtime.CapDropAll,
		Labels:      labels,
	}); err != nil {
		_ = m.pod.Stop(ctx, h)
		finalizeAll()
		return nil, err
	}

	// Node-reachable control endpoint (pod IP + control port). Empty PodIP => unreachable URL;
	// the reconciler/node handler treats that as "no live control plane".
	controlURL := ""
	if h.PodIP != "" {
		controlURL = "http://" + net.JoinHostPort(h.PodIP, strconv.Itoa(controlPort)) + "/control/model"
	}
	// Continuous journaling (design §2, sp-u53.5.2): start a per-mount file watcher
	// driving RequestSnapshot for the spawn's lifetime. The journal's adaptive
	// debounce + serial queue coalesce the events, and a periodic fallback inside
	// the watcher catches dropped events. Guarded: only journaled mounts get a
	// watcher, so scratch-only spawns are untouched. The pod is already up, so the
	// host dirs exist and any resume restore has landed.
	watchers := m.startJournalWatchers(id, generation, journalMounts)

	// Delta chain depth continuation: load the persisted depth so a resumed spawn
	// keeps counting from where it left off. Non-fatal: on load failure we start at 0.
	var deltaDepth int
	if m.cfg.DeltaCapture {
		if drec, found, derr := m.deltaState.Load(id); derr != nil {
			log.Printf("delta state load for %s: %v (starting depth at 0)", id, derr)
		} else if found {
			deltaDepth = drec.Depth
		}
	}

	sp := &Spawn{
		ID: id, Generation: generation, SidecarID: h.SidecarID, AgentID: h.AgentID,
		MountDirs: mountDirs, JournalMounts: journalMounts, journalWatchers: watchers,
		FloorIP: floorIP, PodIP: h.PodIP, NetnsPath: h.NetnsPath, SandboxID: h.SandboxID,
		Status: "ready", Mode: sel.Mode, ControlToken: controlToken, ControlURL: controlURL,
		BaseImageDigest: baseDigest,
		LaunchImageRef:  launchImage, // delta tag on same-node resume, base ref on fresh create
		DeltaDepth:      deltaDepth,
	}
	m.store.Put(sp)
	return sp, nil
}

// startJournalWatchers starts one continuous file watcher per journaled mount,
// each driving RequestSnapshot(spawnID, gen, mount) on changes (design §2). A
// watcher that fails to construct (e.g. the inotify instance limit) is skipped
// with a log line — the final suspend snapshot and the per-mount periodic
// fallback still bound the loss window. Returns the started watchers for teardown.
func (m *Manager) startJournalWatchers(id string, gen uint64, mounts []journal.Mount) []*journal.Watcher {
	if m.journal == nil || len(mounts) == 0 {
		return nil
	}
	var watchers []*journal.Watcher
	for _, mt := range mounts {
		mt := mt // capture per-iteration for the trigger closure
		trigger := func() {
			// Async + best-effort: RequestSnapshot returns immediately (the queue
			// runs the snapshot in the background); context.Background keeps the
			// snapshot independent of any request ctx.
			m.journal.RequestSnapshot(context.Background(), id, gen, mt)
		}
		w, err := journal.NewWatcher(mt.HostDir, journal.DefaultWatchInterval, trigger)
		if err != nil {
			log.Printf("journal watcher for %s mount %s: %v (final-snapshot + periodic fallback still apply)", id, mt.Name, err)
			continue
		}
		w.Start(context.Background())
		watchers = append(watchers, w)
	}
	return watchers
}

// snapshotJournal takes the final per-mount journal snapshot, stringifies the resulting manifest
// ids into a markers map, and persists them to journalState (for same-node resume without CP
// protocol). The caller is responsible for the `m.journal != nil && len(sp.JournalMounts) > 0`
// guard. journal.Close is intentionally NOT called here — it stays in teardown so FinishSuspend
// (snapshot=false) still closes the repo.
func (m *Manager) snapshotJournal(ctx context.Context, sp *Spawn) (map[string]string, error) {
	id := sp.ID
	ids, err := m.journal.FinalSnapshot(ctx, id, sp.Generation, sp.JournalMounts)
	if err != nil {
		return nil, err
	}
	markers := map[string]string{}
	for mount, mid := range ids {
		markers[mount] = string(mid)
		log.Printf("journal: spawn=%s gen=%d mount=%s final manifest=%s", id, sp.Generation, mount, mid)
	}
	if m.journalState != nil {
		rec := journalRecord{Generation: sp.Generation, Manifests: ids}
		if serr := m.journalState.Save(id, rec); serr != nil {
			log.Printf("journal state save for %s: %v", id, serr)
		}
	}
	return markers, nil
}

func (m *Manager) restoreRootfsArtifacts(ctx context.Context, id string, sourceGeneration uint64, baseRef string, artifacts []RootfsArtifact) error {
	if m.journal == nil {
		return fmt.Errorf("rootfs artifact restore for %s: no journaler configured", id)
	}
	if sourceGeneration == 0 {
		return fmt.Errorf("rootfs artifact restore for %s: missing source generation", id)
	}
	for _, art := range artifacts {
		if art.ArtifactID == "" {
			return fmt.Errorf("rootfs artifact restore for %s: empty artifact id (restore must be pinned)", id)
		}
		if art.Generation != 0 && art.Generation != sourceGeneration {
			return fmt.Errorf("rootfs artifact restore for %s: artifact %s generation %d does not match source generation %d",
				id, art.ArtifactID, art.Generation, sourceGeneration)
		}
		if art.BaseImageDigest != "" && art.BaseImageDigest != baseRef {
			return fmt.Errorf("rootfs artifact restore for %s: artifact %s base digest %s does not match pinned base digest %s",
				id, art.ArtifactID, art.BaseImageDigest, baseRef)
		}
		var payload bytes.Buffer
		desc, err := m.journal.GetArtifact(ctx, id, sourceGeneration, art.ArtifactID, &payload)
		if err != nil {
			return fmt.Errorf("rootfs artifact restore for %s: get artifact %s: %w", id, art.ArtifactID, err)
		}
		if desc.Generation != 0 && desc.Generation != sourceGeneration {
			return fmt.Errorf("rootfs artifact restore for %s: journal returned artifact %s generation %d, want %d",
				id, art.ArtifactID, desc.Generation, sourceGeneration)
		}
		if desc.BaseImageDigest != "" && desc.BaseImageDigest != baseRef {
			return fmt.Errorf("rootfs artifact restore for %s: journal returned artifact %s base digest %s, want %s",
				id, art.ArtifactID, desc.BaseImageDigest, baseRef)
		}
		if _, err := m.pod.ImportDelta(ctx, id, baseRef, bytes.NewReader(payload.Bytes())); err != nil {
			return fmt.Errorf("rootfs artifact restore for %s: import artifact %s: %w", id, art.ArtifactID, err)
		}
	}
	return nil
}

func (m *Manager) rootfsProducerRuntime() string {
	if m.cfg.ContainerRuntime != "" {
		return m.cfg.ContainerRuntime
	}
	return "docker"
}

func rootfsArtifactFromJournal(desc journal.ArtifactDescriptor) RootfsArtifact {
	return RootfsArtifact{
		ArtifactID:       desc.ArtifactID,
		Generation:       desc.Generation,
		Sequence:         desc.Sequence,
		BaseImageDigest:  desc.BaseImageDigest,
		Format:           desc.Format,
		ContentDigest:    desc.ContentDigest,
		UncompressedSize: desc.UncompressedSize,
		ProducerNodeID:   desc.ProducerNodeID,
		ProducerRuntime:  desc.ProducerRuntime,
	}
}

// PreflightRuntime validates a configured non-default container runtime at startup (delegates to the
// backend's smoke check). Callers should fail hard rather than discover a broken runtime at first spawn.
func (m *Manager) PreflightRuntime(ctx context.Context) error {
	return m.pod.Preflight(ctx)
}

func (m *Manager) Stop(ctx context.Context, id string) error {
	// Claim atomically removes the spawn from the store so a concurrent quota-watchdog
	// Stop or CP-driven Delete cannot race into a double-teardown.
	sp, ok := m.store.Claim(id)
	if !ok {
		return fmt.Errorf("unknown spawn %s", id)
	}
	// snapshot=true: best-effort final journal snapshot, never blocks teardown (fail-closed is suspend-gate-only).
	_, _ = m.teardown(ctx, sp, false, false, false, true)
	return nil
}

// Suspend tears the spawn's pod down exactly like Stop, but RETURNS the per-mount persist markers
// (mount name -> pinned manifest id) produced by the journal final snapshot, so the CP can record
// them against the suspended spawn (sp-a7fs). The map is empty for scratch-only spawns (or when no
// journaler is installed). Like Stop, teardown completes even if the caller's ctx is already
// cancelled. The CP-side per-spawn lock + generation fence (the node drops a stale Suspend before
// calling here) guarantee at most one in-flight suspend/stop per spawn.
func (m *Manager) Suspend(ctx context.Context, id string) (map[string]string, error) {
	// Claim atomically removes the spawn so concurrent watchdog/CP teardowns cannot race.
	sp, ok := m.store.Claim(id)
	if !ok {
		return nil, fmt.Errorf("unknown spawn %s", id)
	}
	// snapshot=true: best-effort final journal snapshot, never blocks teardown (fail-closed is suspend-gate-only).
	res, err := m.teardown(ctx, sp, true, false, false, true)
	return res.MountMarkers, err
}

func (m *Manager) SuspendForMigration(ctx context.Context, id string, captureRootfsArtifact bool) (SuspendResult, error) {
	sp, ok := m.store.Claim(id)
	if !ok {
		return SuspendResult{}, fmt.Errorf("unknown spawn %s", id)
	}
	// snapshot=true: best-effort final journal snapshot, never blocks teardown (fail-closed is suspend-gate-only).
	return m.teardown(ctx, sp, true, false, captureRootfsArtifact, true)
}

// SnapshotForSuspend is the non-destructive suspend GATE (spec §4, fail-closed): it quiesces
// the agent (Pause), takes the final journal snapshot, and returns the per-mount persist markers
// — WITHOUT removing the spawn from the store or stopping the pod. The node calls this BEFORE
// reaping ACP sessions, so sessions are cleanly torn down between the quiesce and the teardown.
//
// On snapshot SUCCESS the agent is left PAUSED and the journal watchers are stopped (roast-M17:
// no writes between snapshot and pod.Stop). The caller must follow up with FinishSuspend to
// complete the teardown.
//
// On snapshot FAILURE the agent is Unpaused and the journal watchers are restarted — the spawn
// is fully restored to its live state and an error is returned. The caller may retry or leave
// the spawn running.
//
// Pause failure is NON-FATAL (spec §3): we log and snapshot the live tree anyway. The roast-M17
// guarantee (no writes between snapshot and stop) is best-effort when Pause fails.
func (m *Manager) SnapshotForSuspend(ctx context.Context, id string) (SuspendResult, error) {
	sp, ok := m.store.Get(id)
	if !ok {
		return SuspendResult{}, fmt.Errorf("unknown spawn %s", id)
	}
	// Cleanup/abort must run even if the caller's ctx is already cancelled.
	ctx = context.WithoutCancel(ctx)

	// Stop continuous journal watchers so no background RequestSnapshot races the snapshot below.
	for _, w := range sp.journalWatchers {
		w.Stop()
	}
	sp.journalWatchers = nil

	h := &runtime.PodHandle{
		PodIP:     sp.PodIP,
		AgentID:   sp.AgentID,
		NetnsPath: sp.NetnsPath,
		SidecarID: sp.SidecarID,
		SandboxID: sp.SandboxID,
	}
	if perr := m.pod.Pause(ctx, h); perr != nil {
		// Non-fatal (spec §3): snapshot the live tree. Roast-M17 is best-effort.
		log.Printf("suspend gate: pause %s: %v (non-fatal; snapshotting live tree)", id, perr)
	}

	result := SuspendResult{MountMarkers: map[string]string{}}
	if m.journal != nil && len(sp.JournalMounts) > 0 {
		markers, err := m.snapshotJournal(ctx, sp)
		if err != nil {
			// Abort/restore: unpause so the agent can keep running, restart watchers.
			if uerr := m.pod.Unpause(ctx, h); uerr != nil {
				log.Printf("suspend gate abort: unpause %s: %v", id, uerr)
			}
			sp.journalWatchers = m.startJournalWatchers(id, sp.Generation, sp.JournalMounts)
			return SuspendResult{}, fmt.Errorf("suspend gate: journal final snapshot for %s: %w", id, err)
		}
		result.MountMarkers = markers
	}
	// Success: agent left paused, watchers stopped, markers persisted by snapshotJournal.
	// Spawn stays in store until FinishSuspend claims and tears it down.
	return result, nil
}

// FinishSuspend completes the suspend teardown started by SnapshotForSuspend (spec §4): it
// claims the spawn from the store, captures the rootfs delta (on the paused container — commit
// works on paused containers), stops the pod, removes the egress floor, finalizes mount dirs,
// and closes the journal repo. The journal snapshot was already taken by SnapshotForSuspend, so
// FinishSuspend passes snapshot=false to teardown.
//
// The returned SuspendResult carries RootfsArtifacts (when captureRootfsArtifact=true and
// DeltaCapture is enabled). MountMarkers is intentionally empty — the node already holds them
// from the SnapshotForSuspend call and does not need them re-returned here.
func (m *Manager) FinishSuspend(ctx context.Context, id string, captureRootfsArtifact bool) (SuspendResult, error) {
	sp, ok := m.store.Claim(id)
	if !ok {
		return SuspendResult{}, fmt.Errorf("unknown spawn %s", id)
	}
	// capture=true: rootfs CaptureDelta on the paused container (non-fatal as always).
	// gc=false: delta image preserved for same-node restart-resume.
	// snapshot=false: SnapshotForSuspend already took the final journal snapshot.
	return m.teardown(ctx, sp, true, false, captureRootfsArtifact, false)
}

// Delete tears down the spawn (without capturing a delta) and runs GC: releases the per-spawn
// delta image (ReleaseDelta) and purges the durable delta + journal state files.  This is the
// destroy path — the CP issues an explicit delete when it has confirmed the spawn will not
// resume.  Stop does NOT GC (the delta image must survive for same-node restart-resume).
//
// Wiring note: the node's CPMessage_Stop→STOPPED destroy path currently calls Stop; switching it
// to Delete is a REQUIRED follow-up in internal/node (out of allowed files for this task).
func (m *Manager) Delete(ctx context.Context, id string) error {
	// Claim atomically removes the spawn so concurrent watchdog/CP teardowns cannot race.
	sp, ok := m.store.Claim(id)
	if !ok {
		return fmt.Errorf("unknown spawn %s", id)
	}
	// snapshot=true: best-effort final journal snapshot, never blocks teardown (fail-closed is suspend-gate-only).
	_, _ = m.teardown(ctx, sp, false, true, false, true)
	return nil
}

// teardown is the shared Stop/Suspend/Delete body: stop the pod, remove the egress floor, run the
// journal suspend barrier (final snapshot + node-local pin save), finalize the scratch dirs, and
// drop the spawn from the in-mem store. It returns the per-mount persist markers from the final
// snapshot (empty when journaling is off / the spawn has no journaled mounts) so Suspend can hand
// them to the CP; Stop and Delete discard them.
//
//   - capture=true (Suspend path): trigger the rootfs delta capture BEFORE pod.Stop (live container).
//   - gc=true (Delete path): release the delta image after pod.Stop and purge durable state files.
//     (Stop and Suspend both have gc=false — the delta image must survive for same-node restart-resume.)
//   - snapshot=true: take a final journal snapshot + persist node-local state (best-effort, non-fatal).
//     false when SnapshotForSuspend (the gate) already did it — FinishSuspend calls with snapshot=false.
func (m *Manager) teardown(ctx context.Context, sp *Spawn, capture, gc, captureRootfsArtifact, snapshot bool) (SuspendResult, error) {
	id := sp.ID
	result := SuspendResult{MountMarkers: map[string]string{}}
	var resultErr error
	// Teardown must complete even if the caller's ctx is already cancelled (e.g. the CP connection
	// dropped mid-startup and the readiness probe failed): detach so firewall + mount cleanup run.
	ctx = context.WithoutCancel(ctx)
	// Stop the continuous journal watchers FIRST so no background RequestSnapshot
	// races the suspend barrier below (the serial queue would drop a post-suspend
	// request anyway, but stopping here also reclaims the watcher goroutines).
	for _, w := range sp.journalWatchers {
		w.Stop()
	}

	// Delta capture (spec §2/§4): commit the agent container's writable layer to a local image tag
	// BEFORE pod.Stop (which removes the container). Non-fatal: a capture failure is logged and the
	// teardown continues normally — the next resume falls back to the base image (cold-ish start).
	// Orthogonal to the journal block below (journal handles data mounts; delta handles rootfs).
	if capture && m.cfg.DeltaCapture && sp.AgentID != "" {
		// Live capture-time scrub: `rm -rf <paths>` in the agent container BEFORE commit so the
		// committed layer does not include apt caches, /tmp noise, etc. Best-effort, non-fatal.
		if len(m.cfg.DeltaScrubPaths) > 0 && m.scrubFn != nil {
			// Pass sp.AgentID directly: the spawn has already been removed from the store
			// by Claim above, so passing the spawn id and re-looking-up via ExecRun would
			// always fail with "spawn X has no agent container".
			if serr := m.scrubFn(ctx, sp.AgentID, m.cfg.DeltaScrubPaths); serr != nil {
				log.Printf("delta scrub for %s: %v (non-fatal; proceeding with capture)", id, serr)
			}
		}
		h := &runtime.PodHandle{
			SpawnID:   sp.ID,
			AgentID:   sp.AgentID,
			SidecarID: sp.SidecarID,
			// Use the launch image (delta on resume, base on fresh create) as the layer-count
			// reference for the moby#47065 guard — NOT the original base — so chained captures
			// correctly detect a zero-layer commit on the 2nd+ suspend (spec §3 validation).
			BaseImageRef: sp.LaunchImageRef,
		}
		if ref, cerr := m.pod.CaptureDelta(ctx, h); cerr != nil {
			log.Printf("delta capture for %s: %v (non-fatal; next resume uses base image)", id, cerr)
			if captureRootfsArtifact {
				resultErr = fmt.Errorf("rootfs artifact capture for %s: capture delta: %w", id, cerr)
			}
		} else {
			sp.DeltaImageRef = ref
			sp.DeltaDepth++
			// Persist the updated depth so a resume continuation starts at the right depth.
			if serr := m.deltaState.Save(id, deltaRecord{Depth: sp.DeltaDepth}); serr != nil {
				log.Printf("delta state save for %s: %v", id, serr)
			}
			log.Printf("delta captured spawn=%s ref=%s depth=%d", id, ref, sp.DeltaDepth)
			// Squash-needed heuristic: warn (or call injected callback) when the chain grows long.
			if sp.DeltaDepth >= m.cfg.DeltaSquashDepth {
				if m.squashNeeded != nil {
					m.squashNeeded(id, sp.DeltaDepth)
				} else {
					log.Printf("SQUASH-NEEDED spawn=%s depth=%d threshold=%d "+
						"(squash exec deferred until backend layer-export method available)",
						id, sp.DeltaDepth, m.cfg.DeltaSquashDepth)
				}
			}
			if captureRootfsArtifact {
				if m.journal == nil {
					resultErr = fmt.Errorf("rootfs artifact capture for %s: no journaler configured", id)
				} else {
					var payload bytes.Buffer
					if err := m.pod.ExportDelta(ctx, id, &payload); err != nil {
						resultErr = fmt.Errorf("rootfs artifact capture for %s: export delta: %w", id, err)
					} else {
						desc := journal.ArtifactDescriptor{
							Type:            journal.ArtifactRootfsDelta,
							Sequence:        sp.DeltaDepth,
							BaseImageDigest: sp.BaseImageDigest,
							Format:          journal.ArtifactFormatOCILayout,
							ProducerNodeID:  m.cfg.NodeID,
							ProducerRuntime: m.rootfsProducerRuntime(),
						}
						stored, err := m.journal.PutArtifact(ctx, id, sp.Generation, desc, bytes.NewReader(payload.Bytes()))
						if err != nil {
							resultErr = fmt.Errorf("rootfs artifact capture for %s: put artifact: %w", id, err)
						} else {
							result.RootfsArtifacts = append(result.RootfsArtifacts, rootfsArtifactFromJournal(stored))
						}
					}
				}
				if resultErr != nil {
					log.Printf("%v", resultErr)
				}
			}
		}
	}

	_ = m.pod.Stop(ctx, &runtime.PodHandle{SidecarID: sp.SidecarID, AgentID: sp.AgentID, SandboxID: sp.SandboxID})
	if sp.FloorIP != "" {
		if err := m.fw.Remove(ctx, firewall.Rules(sp.FloorIP, m.cfg.EgressAllowCIDRs)); err != nil {
			log.Printf("egress floor cleanup for %s (ip %s): %v", id, sp.FloorIP, err)
		}
	}

	// Suspend seam (design §2, roast M17): the pod is stopped (tree quiescent),
	// so drain pending snapshots and take the final per-mount snapshot BEFORE the
	// scratch backend nukes the host dirs below. Guarded: only runs when a
	// journaler is installed and this spawn actually has journaled mounts —
	// scratch-only spawns skip it entirely. snapshot=false when SnapshotForSuspend
	// (the gate) already handled this — FinishSuspend skips the snapshot and lets
	// Close alone finalize the repo.
	if m.journal != nil && len(sp.JournalMounts) > 0 {
		if snapshot {
			// Non-fatal: teardown must still complete. With no markers, the CP records an empty
			// marker set (a same-node resume falls back to the seeded scratch dir).
			if markers, serr := m.snapshotJournal(ctx, sp); serr != nil {
				log.Printf("journal final snapshot for %s: %v", id, serr)
			} else {
				for k, v := range markers {
					result.MountMarkers[k] = v
				}
			}
		}
		if err := m.journal.Close(ctx, id); err != nil {
			log.Printf("journal close for %s: %v", id, err)
		}
	}

	for _, d := range sp.MountDirs {
		_ = m.backend.Finalize(ctx, d)
	}
	// Owner-sealed secret plaintext must not outlive the episode (design §6 never-persist): drop the
	// per-spawn secrets dir. Best-effort — a leftover dir is reseeded empty on the next Create.
	if serr := m.secrets.Remove(id); serr != nil {
		log.Printf("secrets dir cleanup for %s: %v", id, serr)
	}

	// GC path (Delete only): release the delta image and purge durable state files.
	// Stop and Suspend leave the delta image in place for same-node restart-resume.
	if gc {
		if gerr := m.pod.ReleaseDelta(ctx, id); gerr != nil {
			log.Printf("delta release for %s: %v (non-fatal)", id, gerr)
		}
		if derr := m.deltaState.Delete(id); derr != nil {
			log.Printf("delta state delete for %s: %v", id, derr)
		}
		if m.journalState != nil {
			if jerr := m.journalState.Delete(id); jerr != nil {
				log.Printf("journal state delete for %s: %v", id, jerr)
			}
		}
	}

	// The spawn was removed from the store atomically by Claim (in Stop/Suspend/Delete)
	// before teardown was called, so no store.Delete is needed here.
	return result, resultErr
}

// InjectSecret writes one unsealed secret's plaintext into spawnID's tmpfs secrets dir at target
// (design §6). The node calls this after OpenDelivered; the agent reads the file in place. Returns the
// host path written (for logging). Plaintext is the caller's responsibility to obtain via the sub-key.
func (m *Manager) InjectSecret(spawnID, target string, plaintext []byte) (string, error) {
	if _, ok := m.store.Get(spawnID); !ok {
		return "", fmt.Errorf("unknown spawn %s", spawnID)
	}
	return m.secrets.Write(spawnID, target, plaintext)
}

// newControlToken returns a 256-bit random hex string used as the sidecar control-endpoint
// bearer token (one per pod). Mirrors the crypto/rand+hex pattern in server.go's newID.
func newControlToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
