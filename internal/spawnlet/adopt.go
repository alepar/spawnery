package spawnlet

// adopt.go — spawnlet-restart re-adoption (SE3, design §4.2/§4.3). A restarted spawnlet finds its pods
// STILL RUNNING (gracefulDetachAll, sp-2tx8.3.2) and an EMPTY in-memory store. This file is the node-local
// half of putting that back together: find the pods we are not tracking (UntrackedPods), rebuild a Spawn
// for the ones the CP confirms (Adopt), and destroy — with a capture first — the ones it disowns (ReapPod).
//
// The node NEVER decides adopt-vs-reap here: that is the CP's call, matched on generation
// (internal/node/readopt.go drives the handshake). This file only executes the verdict.

import (
	"context"
	"fmt"
	"log"
	"net"
	"path/filepath"
	"strconv"

	"spawnery/internal/manifest"
	"spawnery/internal/runtime"
	"spawnery/internal/spawnlet/firewall"
	"spawnery/internal/storage"
	"spawnery/internal/storage/journal"
)

// UntrackedPods returns every spawnery-managed pod this node's runtime still has that this Manager is NOT
// tracking — the survivors of a previous node process (the in-mem store is empty after a restart), each
// carrying the container ids and the pod IP a rebuild needs (sp-2tx8.3.1).
//
// Scoped by the spawnery.node-id label: a pod created by a DIFFERENT node id is not ours — two spawnlets
// sharing one Docker daemon (dev stack + an e2e run, or multi-node-on-one-host) must neither adopt nor reap
// each other's pods. It is logged and left alone (design §4.3). Unlabeled pods (pre-label versions) are
// treated as ours.
//
// It has NO side effects: nothing is captured, stopped or adopted. The caller reports this set to the CP and
// applies the CP's per-spawn verdict. That asymmetry is the point — the old ReapOrphans destroyed on the
// node's own authority, so a CP blip became data loss.
func (m *Manager) UntrackedPods(ctx context.Context) ([]runtime.ManagedPod, error) {
	managed, err := m.pod.ListManaged(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]runtime.ManagedPod, 0, len(managed))
	for _, mp := range managed {
		if mp.NodeID != "" && mp.NodeID != m.cfg.NodeID {
			log.Printf("reconcile: leaving pod spawn=%s alone (node-id %q is not ours)", mp.SpawnID, mp.NodeID)
			continue
		}
		if _, live := m.store.Get(mp.SpawnID); live {
			continue // already ours (an earlier reconcile pass adopted it, or it never left)
		}
		out = append(out, mp)
	}
	return out, nil
}

// ReapPod destroys ONE untracked pod: capture-before-reap, then stop. This is the CP's REAP verdict (an
// unknown/dormant spawn or a superseded generation) and the fallback for an ADOPT whose rebuild failed —
// today's ReapOrphans behaviour, now applied per pod instead of to everything in sight.
//
// Crash-survival (spec §4): with DeltaCapture enabled, CaptureDelta runs on the agent container BEFORE
// pod.Stop, so the spawn's work survives for a future resume. Best-effort and non-fatal — a capture failure
// just means the next resume starts from the last known-good delta (or the base image if none existed).
//
// moby#47065 note: the layer-count guard in CaptureDelta wants the BaseImageRef of the launch image. A
// reaped pod has no Spawn record, so BaseImageRef is empty and the guard degrades to rejecting only truly
// zero-layer commits — unchanged from ReapOrphans.
//
// It also removes the pod's egress-floor rules (which the previous process never removed): the pod IP is
// about to be released back to the bridge, and a stale DROP would later bite whatever recycles it.
func (m *Manager) ReapPod(ctx context.Context, mp runtime.ManagedPod) error {
	log.Printf("reaping pod spawn=%s gen=%d (CP verdict: reap, or rebuild failed)", mp.SpawnID, mp.Generation)

	if m.cfg.DeltaCapture && mp.AgentID != "" {
		h := &runtime.PodHandle{SpawnID: mp.SpawnID, AgentID: mp.AgentID}
		if ref, cerr := m.pod.CaptureDelta(ctx, h); cerr != nil {
			log.Printf("capture-before-reap spawn=%s: %v (non-fatal; delta may be stale)", mp.SpawnID, cerr)
		} else {
			log.Printf("capture-before-reap spawn=%s ref=%s", mp.SpawnID, ref)
		}
	}

	err := m.pod.Stop(ctx, &runtime.PodHandle{SidecarID: mp.SidecarID, AgentID: mp.AgentID, SandboxID: mp.SandboxID})

	if m.egressEnforced() && mp.PodIP != "" {
		if ferr := m.fw.Remove(ctx, firewall.Rules(mp.PodIP, m.cfg.EgressAllowCIDRs)); ferr != nil {
			log.Printf("reap: remove egress floor for %s (ip %s): %v", mp.SpawnID, mp.PodIP, ferr)
		}
	}
	return err
}

// AdoptSpec is the CP-re-delivered launch spec for a pod that never stopped (design §3: the CP holds the
// authoritative ledger, the node persists nothing). It mirrors the fields of the StartSpawn the spawn was
// launched with — the node maps AdoptSpawn.spec onto it (internal/node/readopt.go) — minus everything an
// adoption has no use for: no auth (CP-initiated, no client intent to verify), no artifacts / rootfs
// artifacts (the pod is RUNNING; nothing to materialize or restore) and no secrets (the CP custodies only
// owner-sealed ciphertext it cannot open).
type AdoptSpec struct {
	AppRef     string
	Model      string
	Name       string
	AppID      string
	Image      string
	RunnableID string
	Mode       string
	Generation uint64
	Mounts     []MountBinding
	// BaseImageDigest is the digest the CP pinned for this spawn at start; reported back on ACTIVE.
	BaseImageDigest string
	// JournalKeyDeliveryPending is the CP's flag that this spawn has an OWNER-SEALED journaled mount whose
	// Kopia repo password died with the node process. The CP cannot re-deliver it (the ciphertext is sealed
	// to the OWNER); it re-arms the owner's key-travel path instead. Adopt marks the spawn owner-sealed so
	// the journaler waits for the delivery rather than forking the repo under a fresh node-local key.
	JournalKeyDeliveryPending bool
}

// Adopt rebuilds the in-memory Spawn for a pod that survived a spawnlet restart, and puts it back in the
// store — the ADOPT verdict of the re-adoption handshake (design §4.2). The pod is RUNNING and its dirs are
// bind-mounted into a live agent, so this function is deliberately a *reconstruction*, never a re-run of
// Create:
//
//   - mount host dirs are DERIVED (storage.Backend.HostDir), never Prepare'd — Prepare re-seeds (scratch)
//     or re-clones (github) a directory the agent is actively writing to;
//   - nothing is restored from the journal — the live host dirs are authoritative, the pinned manifests are
//     the LAST SUSPEND's and would roll the spawn back;
//   - no image is pulled, no container is created, no artifact/secret is re-materialized.
//
// What it DOES re-establish: the mount table (dirs, finalizers, journaled mounts, container-side targets),
// the owner-sealed journal marking, the egress floor (remove-then-apply: see below), the delta-chain depth
// from the durable delta store, and the continuous journal watchers.
//
// The floor is re-established as Remove-then-Apply, not a bare Apply: the previous process never removed
// its rules (DetachAll deliberately doesn't) and both appliers insert with `-I`, so a bare Apply would
// install a DUPLICATE set that teardown's single `-D` pass would leave behind — a stale DROP for whatever
// pod next recycles the IP. Remove is best-effort (a flushed chain has nothing to delete); Apply is
// fail-closed.
//
// Any failure rolls back everything it did and returns an error, and the CALLER then capture-before-reaps
// the pod (ReapPod). There is no half-adopted state: a spawn is fully rebuilt or it is gone.
//
// TODO(sp-2tx8.3.5): ControlToken is left EMPTY (it lives in the still-running sidecar's env and must be
// read back from there) and the per-spawn GitHub control listener is NOT re-served (its MITM CA is
// regenerated-on-miss today, so re-serving it would hand the agent a CA it does not trust). Until 3.5 lands,
// an adopted spawn cannot SetModel and cannot mint GitHub tokens in-agent. Both are 3.5's scope.
func (m *Manager) Adopt(ctx context.Context, mp runtime.ManagedPod, spec AdoptSpec) (sp *Spawn, err error) {
	id := mp.SpawnID
	if spec.Generation != 0 && spec.Generation != mp.Generation {
		return nil, fmt.Errorf("adopt %s: spec generation %d != pod generation %d (stale decision)", id, spec.Generation, mp.Generation)
	}
	if mp.AgentID == "" {
		return nil, fmt.Errorf("adopt %s: pod has no agent container (it never fully started)", id)
	}
	if mp.PodIP == "" {
		// No IP => no ACP re-dial and no floor to scope. A stopped pod (a suspended spawn, a dead
		// container) lands here — reap-with-capture is the right answer for it.
		return nil, fmt.Errorf("adopt %s: pod has no IP (not running?)", id)
	}

	mt, err := m.adoptMountTable(id, spec)
	if err != nil {
		return nil, fmt.Errorf("adopt %s: %w", id, err)
	}

	// Owner-sealed journaled mounts: the repo password died with the process and the CP cannot re-deliver
	// it — mark the spawn so the journaler routes the OWNER's re-delivered key into owner-sealed custody
	// instead of minting a fresh node-local one. The pod keeps running; only new snapshots wait for the key.
	if m.journalKeys != nil {
		for _, jm := range mt.journalMounts {
			if jm.Class == journal.OwnerSealed {
				m.journalKeys.MarkOwnerSealed(id)
				break
			}
		}
	}
	if spec.JournalKeyDeliveryPending {
		log.Printf("adopt %s: owner-sealed journal key must be re-delivered before new snapshots can run", id)
	}

	// Egress floor (fail-closed).
	var floorIP string
	if m.egressEnforced() {
		rules := firewall.Rules(mp.PodIP, m.cfg.EgressAllowCIDRs)
		if rerr := m.fw.Remove(ctx, rules); rerr != nil {
			// Expected when the chain was flushed (or on a lane where -D of a missing rule errors).
			log.Printf("adopt %s: pre-apply floor remove (ip %s): %v (continuing)", id, mp.PodIP, rerr)
		}
		if aerr := m.fw.Apply(ctx, rules); aerr != nil {
			return nil, fmt.Errorf("adopt %s: egress floor (fail-closed): %w", id, aerr)
		}
		floorIP = mp.PodIP
	}
	defer func() {
		if err != nil && floorIP != "" {
			if rerr := m.fw.Remove(context.WithoutCancel(ctx), firewall.Rules(floorIP, m.cfg.EgressAllowCIDRs)); rerr != nil {
				log.Printf("adopt %s: rollback floor remove (ip %s): %v", id, floorIP, rerr)
			}
		}
	}()

	// Delta chain depth continues from the durable node-local record (it survived the restart).
	var deltaDepth int
	if m.cfg.DeltaCapture {
		if drec, found, derr := m.deltaState.Load(id); derr != nil {
			log.Printf("adopt %s: delta state load: %v (starting depth at 0)", id, derr)
		} else if found {
			deltaDepth = drec.Depth
		}
	}

	// LaunchImageRef: the node cannot know whether the live container came up from the base or the delta
	// tag, and probing would pull. The base ref only makes CaptureDelta's moby#47065 layer guard permissive
	// (never destructive) — the same degradation the orphan-capture path already accepts.
	baseRef := spec.Image
	if baseRef == "" {
		baseRef = m.cfg.AgentImage
	}
	if spec.BaseImageDigest != "" {
		baseRef = spec.BaseImageDigest
	}

	controlURL := ""
	if mp.PodIP != "" {
		controlURL = "http://" + net.JoinHostPort(mp.PodIP, strconv.Itoa(m.cfg.SidecarPort+1)) + "/control/model"
	}

	sp = &Spawn{
		ID: id, Generation: mp.Generation,
		SidecarID: mp.SidecarID, AgentID: mp.AgentID, SandboxID: mp.SandboxID,
		MountDirs:       mt.mountDirs,
		MountBindings:   append([]MountBinding(nil), spec.Mounts...),
		MountFinalizers: mt.finalizers,
		JournalMounts:   mt.journalMounts,
		MountTargets:    mt.targets,
		FloorIP:         floorIP,
		PodIP:           mp.PodIP,
		Status:          "ready",
		Mode:            spec.Mode,
		ControlURL:      controlURL,
		// ControlToken: TODO(sp-2tx8.3.5) — read back from the sidecar's env.
		BaseImageDigest: spec.BaseImageDigest,
		LaunchImageRef:  baseRef,
		DeltaDepth:      deltaDepth,
	}
	// Watchers last: they are the only started goroutines here, so nothing above can fail under them.
	m.setWatchers(sp, m.startJournalWatchers(id, mp.Generation, mt.journalMounts))
	m.store.Put(sp)
	log.Printf("adopted spawn=%s gen=%d agent=%s pod-ip=%s mounts=%d journaled=%d",
		id, mp.Generation, mp.AgentID, mp.PodIP, len(mt.mountDirs), len(mt.journalMounts))
	return sp, nil
}

// adoptedMounts is the re-derived mount table of an adopted spawn.
type adoptedMounts struct {
	mountDirs     []string
	finalizers    []MountFinalizer
	journalMounts []journal.Mount
	targets       []string
}

// adoptMountTable re-derives what Create built, WITHOUT touching the filesystem: the same host dirs (via
// the pure Backend.HostDir), the same finalizers (so teardown finalizes through the same backend), the same
// journaled-mount set, and the same container-side bind targets (which the delta-scrub guard reads — an
// empty table there makes the guard scrub NOTHING, so getting it right is a data-safety matter).
//
// It mirrors CreateWithSelection's mount loop step for step; keep them in sync.
func (m *Manager) adoptMountTable(id string, spec AdoptSpec) (adoptedMounts, error) {
	var out adoptedMounts
	appPath := spec.AppRef
	if abs, aerr := filepath.Abs(appPath); aerr == nil {
		appPath = abs
	}
	mf, err := manifest.Parse(appPath)
	if err != nil {
		return out, fmt.Errorf("manifest: %w", err)
	}
	bindings, err := mountBindingsByName(mf.Storage.Mounts, spec.Mounts)
	if err != nil {
		return out, err
	}
	resolver := m.backendResolver
	if resolver == nil {
		resolver = storage.NewSchemeResolver(m.cfg.DataRoot)
	}
	rootMaterialize := m.useRootMaterializer()
	scratchBackend := m.scratchBackend()

	// /app: read-only, and root-materialized into DataRoot/<id>/app on the remap lane (Create put that dir
	// in mountDirs with a scratch finalizer; teardown must still clean it).
	appMountPath := appPath
	if rootMaterialize {
		appMountPath = filepath.Join(m.cfg.DataRoot, id, "app")
		out.mountDirs = append(out.mountDirs, appMountPath)
		out.finalizers = append(out.finalizers, MountFinalizer{HostDir: appMountPath, Backend: scratchBackend})
	}
	mounts := []runtime.Mount{{HostPath: appMountPath, ContainerPath: "/app", ReadOnly: true}}

	for _, mt := range mf.Storage.Mounts {
		binding := bindings[mt.Name]
		if binding.Name == "" {
			binding.Name = mt.Name
		}
		class, derr := journal.ParseDurability(mt.Durability)
		if derr != nil {
			return adoptedMounts{}, fmt.Errorf("mount %q durability: %w", mt.Name, derr)
		}
		backend, berr := resolveMountBackend(resolver, binding)
		if berr != nil {
			return adoptedMounts{}, fmt.Errorf("mount %q backend %q: %w", mt.Name, binding.BackendURI, berr)
		}
		hostDir := backend.HostDir(id, mt.Name)
		if rootMaterialize {
			// The bind target is the materialized dir; the backend's own dir is the staging/finalize side.
			preparedDir := backend.HostDir(id, mt.Name+stageMountNameSuffix)
			hostDir = filepath.Join(m.cfg.DataRoot, id, mt.Name)
			out.finalizers = append(out.finalizers, MountFinalizer{
				HostDir: preparedDir, Backend: backend, SyncFrom: hostDir, CleanupDir: hostDir,
			})
		} else {
			out.finalizers = append(out.finalizers, MountFinalizer{HostDir: hostDir, Backend: backend})
		}
		out.mountDirs = append(out.mountDirs, hostDir)
		if m.journal != nil && class.Journaled() {
			out.journalMounts = append(out.journalMounts, journal.Mount{Name: mt.Name, HostDir: hostDir, Class: class})
		}
		mounts = append(mounts, runtime.Mount{HostPath: hostDir, ContainerPath: "/app/" + mt.Path})
	}

	// The three node-managed dirs Create also binds in. They still exist on disk (nothing removed them) and
	// their paths are pure functions of the spawn id — they matter here only for the container-side target
	// list the delta-scrub guard consults.
	mounts = append(mounts,
		runtime.Mount{HostPath: m.secrets.DirFor(id), ContainerPath: SecretsMountPath},
		runtime.Mount{HostPath: m.artifacts.DirFor(id), ContainerPath: ArtifactsMountPath},
		runtime.Mount{HostPath: m.gitEnv.DirFor(id), ContainerPath: GitEnvMountPath},
	)
	out.targets = mountTargetsOf(mounts)
	return out, nil
}

// DetachSpawn removes id from the in-memory store and hands back its journal watchers for the caller to
// Stop — the undo of Adopt when the node's own post-rebuild work (ACP re-dial, session registration)
// fails. It deliberately does NOT touch the pod, run finalizers or remove the egress floor: the caller
// capture-before-reaps the pod itself (ReapPod, which also removes the floor for the pod IP), which is a
// different, CP-sanctioned destruction.
func (m *Manager) DetachSpawn(id string) []*journal.Watcher {
	sp, ok := m.store.Claim(id)
	if !ok {
		return nil
	}
	return m.takeWatchers(sp)
}
