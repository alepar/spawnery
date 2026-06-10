package spawnlet

import (
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

	"spawnery/internal/agentcaps"
	"spawnery/internal/manifest"
	"spawnery/internal/runtime"
	"spawnery/internal/spawnlet/firewall"
	"spawnery/internal/storage"
	"spawnery/internal/storage/journal"
)

type ManagerConfig struct {
	AgentImage, SidecarImage, OpenRouterKey, DataRoot string

	// SecretsRoot is the per-node root for owner-sealed secret tmpfs dirs (design §6). Each spawn gets
	// a subdir here, bind-mounted into the agent at SecretsMountPath; the node writes unsealed plaintext
	// into it (0600). Default DataRoot/secrets. Production should point this at a tmpfs (memory-backed)
	// so plaintext never touches durable disk.
	SecretsRoot string
	SidecarPort                                       int // default 8080

	NodeID           string // this node's id (stamped on container labels for reconcile); "" standalone
	NodeClass        string // "cloud" (always enforces) or "self-hosted" (honors EgressEnforce)
	EgressEnforce    bool   // self-hosted opt-out switch; ignored on cloud
	EgressAllowCIDRs []string

	MemLimitMB       int64   // memory limit in MiB; default 1024
	CPULimit         float64 // CPU cores; default 1.0
	PidsLimit        int64   // max pids per container; default 256
	ContainerRuntime string  // OCI runtime name; "" = Docker default
	HardenRootfs     bool    // if true, run agent with read-only rootfs + /tmp tmpfs
	AdvertiseIP      string  // node IP mosh advertises to spawnctl for terminal attach ("" => auto)
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
	// secrets injects owner-sealed secret plaintext into each spawn's tmpfs secrets dir (design §6).
	// Always set (NewManagerWithBackend defaults SecretsRoot); the node calls InjectSecret after unseal.
	secrets SecretInjector
}

// SetJournal installs the transient-tier journaler (design §1b) plus the
// node-local state dir where per-spawn pinned manifest ids are recorded on
// suspend (so a same-node resume can restore with no CP protocol). Optional:
// when unset, every mount behaves as scratch-only (Ephemeral) and the journal
// seams in Create/Stop are no-ops.
func (m *Manager) SetJournal(j journal.JournalManager, stateDir string) {
	m.journal = j
	m.journalState = &journalStateStore{dir: stateDir}
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
	return &Manager{
		pod:     pod,
		cfg:     cfg,
		store:   NewStore(),
		backend: storage.NewScratch(cfg.DataRoot),
		fw:      fw,
		secrets: SecretInjector{Root: cfg.SecretsRoot},
	}
}

// egressEnforced reports whether the egress floor must be applied: cloud nodes always enforce
// (non-disableable); self-hosted honors the operator's EgressEnforce choice.
func (m *Manager) egressEnforced() bool {
	return m.cfg.NodeClass == "cloud" || m.cfg.EgressEnforce
}

func (m *Manager) Store() *Store { return m.store }

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
func (m *Manager) ReapOrphans(ctx context.Context) error {
	managed, err := m.pod.ListManaged(ctx)
	if err != nil {
		return err
	}
	for _, mp := range managed {
		if _, live := m.store.Get(mp.SpawnID); live {
			continue // still ours
		}
		log.Printf("reaping orphaned pod spawn=%s gen=%d (not in store; node restart)", mp.SpawnID, mp.Generation)
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
	for _, mt := range mf.Storage.Mounts {
		seedDir := filepath.Join(appPath, mt.Seed)
		hostDir, err := m.backend.Prepare(ctx, id, mt.Name, seedDir)
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
			// Same-node resume: restore the pinned manifest recorded at the last
			// suspend into hostDir before bind (over the freshly seeded scratch
			// dir). Non-fatal: a restore failure falls back to the seeded dir and
			// surfaces the scratch-reset reality rather than aborting the spawn.
			// (The owner-sealed cross-node / migration pin is CP-threaded — that
			// remains TODO(phase②) and rides the StartSpawn protocol.)
			if haveJournalRecord {
				if pin, ok := jrec.Manifests[mt.Name]; ok {
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

	// Phase 2: the untrusted agent, into the existing pod.
	if err := m.pod.StartAgent(ctx, h, runtime.AgentSpec{
		Image: agentImage,
		Cmd:   agentCmd,
		Env: []string{
			"OPENAI_BASE_URL=http://" + addr + "/v1",
			"SPAWN_MODEL=" + model,
			"SPAWN_SESSION_TITLE=" + sessionTitle,
		},
		Mounts:         mounts,
		Resources:      res,
		Runtime:        m.cfg.ContainerRuntime,
		DropAllCaps:    true,
		ReadonlyRootfs: m.cfg.HardenRootfs,
		Labels:         labels,
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
	sp := &Spawn{ID: id, Generation: generation, SidecarID: h.SidecarID, AgentID: h.AgentID, MountDirs: mountDirs, JournalMounts: journalMounts, FloorIP: floorIP, PodIP: h.PodIP, NetnsPath: h.NetnsPath, SandboxID: h.SandboxID, Status: "ready", Mode: sel.Mode, ControlToken: controlToken, ControlURL: controlURL}
	m.store.Put(sp)
	return sp, nil
}

// PreflightRuntime validates a configured non-default container runtime at startup (delegates to the
// backend's smoke check). Callers should fail hard rather than discover a broken runtime at first spawn.
func (m *Manager) PreflightRuntime(ctx context.Context) error {
	return m.pod.Preflight(ctx)
}

func (m *Manager) Stop(ctx context.Context, id string) error {
	sp, ok := m.store.Get(id)
	if !ok {
		return fmt.Errorf("unknown spawn %s", id)
	}
	m.teardown(ctx, sp)
	return nil
}

// Suspend tears the spawn's pod down exactly like Stop, but RETURNS the per-mount persist markers
// (mount name -> pinned manifest id) produced by the journal final snapshot, so the CP can record
// them against the suspended spawn (sp-a7fs). The map is empty for scratch-only spawns (or when no
// journaler is installed). Like Stop, teardown completes even if the caller's ctx is already
// cancelled. The CP-side per-spawn lock + generation fence (the node drops a stale Suspend before
// calling here) guarantee at most one in-flight suspend/stop per spawn.
func (m *Manager) Suspend(ctx context.Context, id string) (map[string]string, error) {
	sp, ok := m.store.Get(id)
	if !ok {
		return nil, fmt.Errorf("unknown spawn %s", id)
	}
	return m.teardown(ctx, sp), nil
}

// teardown is the shared Stop/Suspend body: stop the pod, remove the egress floor, run the journal
// suspend barrier (final snapshot + node-local pin save), finalize the scratch dirs, and drop the
// spawn from the in-mem store. It returns the per-mount persist markers from the final snapshot
// (empty when journaling is off / the spawn has no journaled mounts) so Suspend can hand them to the
// CP; Stop discards them.
func (m *Manager) teardown(ctx context.Context, sp *Spawn) map[string]string {
	id := sp.ID
	// Teardown must complete even if the caller's ctx is already cancelled (e.g. the CP connection
	// dropped mid-startup and the readiness probe failed): detach so firewall + mount cleanup run.
	ctx = context.WithoutCancel(ctx)
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
	// scratch-only spawns skip it entirely.
	markers := map[string]string{}
	if m.journal != nil && len(sp.JournalMounts) > 0 {
		ids, err := m.journal.FinalSnapshot(ctx, id, sp.Generation, sp.JournalMounts)
		if err != nil {
			// Non-fatal: teardown must still complete. With no markers, the CP records an empty
			// marker set (a same-node resume falls back to the seeded scratch dir).
			log.Printf("journal final snapshot for %s: %v", id, err)
		} else {
			// Node-local: persist the pinned manifest ids durably on this node so
			// the next same-node resume (Create) restores from them — no CP
			// protocol required (transient-tier §4). The same ids are returned to
			// the caller as the per-mount persist markers the CP records on suspend
			// (CP↔node suspend-marker protocol, design §3 M6, sp-a7fs).
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
	m.store.Delete(id)
	return markers
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
