package spawnlet

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"spawnery/internal/agentcaps"
	"spawnery/internal/githubcred"
	"spawnery/internal/manifest"
	"spawnery/internal/pki"
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
	CertificateRevocations                            pki.CertificateRevocationChecker

	// SecretsRoot is the per-node root for owner-sealed secret tmpfs dirs (design §6). Each spawn gets
	// a subdir here, bind-mounted into the agent at SecretsMountPath; the node writes unsealed plaintext
	// into it (0600). Default DataRoot/secrets. Production should point this at a tmpfs (memory-backed)
	// so plaintext never touches durable disk.
	SecretsRoot string
	// ArtifactsRoot is the per-node root for non-sensitive artifact staging dirs (sp-l5sx.3). Each spawn
	// gets a subdir here, bind-mounted into the agent at ArtifactsMountPath; spawnlet materializes
	// StartSpawn.artifacts here at create/resume time. Default DataRoot/artifacts. Production should
	// point this at a tmpfs (memory-backed) so transient payload bytes do not accumulate on durable disk.
	ArtifactsRoot string
	// GitEnvRoot is the per-node root for per-spawn WRITABLE git-env dirs (sp-7amh). Each spawn gets a
	// subdir here, chowned to the agent's mapped uid and bind-mounted at GitEnvMountPath, holding the
	// agent-owned GIT_CONFIG_GLOBAL. Default DataRoot/git-env. Production should point this at a tmpfs.
	GitEnvRoot string
	// GitHubCredentialsRoot is a node-only tmpfs root for short-lived access tokens used by storage
	// backends. It is never bind-mounted into the agent; agent-render credentials use SecretsRoot.
	GitHubCredentialsRoot string
	GitHubRepos           storage.GitHubRepoService
	GitHubGitRunner       storage.GitRunner
	SidecarPort           int // default 8080

	// GitHubHost / GitHubAllowInsecureHost point github: mounts at a non-github.com git host (e.g. a
	// local Gitea) via SchemeResolver.SetGitHubHostOverride. Empty host preserves the production
	// default (github.com, secure). Set by cmd/spawnlet from GITHUB_HOST / GITHUB_ALLOW_INSECURE_HOST.
	GitHubHost              string
	GitHubAllowInsecureHost bool
	// GitHubStaticCredentials, when non-nil, replaces the Manager's own AS-mint-backed credential
	// provider with a fixed-token provider (e.g. a Gitea PAT). This bypasses the AS mint entirely;
	// the node skips mint-at-provision for github mounts (GitHubStaticCredentialsEnabled). Set by
	// cmd/spawnlet from GITHUB_STATIC_TOKEN. Production leaves this nil.
	GitHubStaticCredentials storage.GitHubCredentialProvider

	// SidecarCABundleFile, when set, is a HOST path to a merged CA bundle (system roots + an extra
	// trusted CA) bind-mounted read-only into the SIDECAR container (at SidecarCABundleMountPath),
	// with SSL_CERT_FILE (SidecarCABundleFileEnv) pointed at the mounted copy. Go's
	// x509.SystemCertPool honours SSL_CERT_FILE — but it REPLACES rather than appends, so the file
	// at this path must already be a merge (system roots + the extra CA), not the extra CA alone,
	// or the sidecar loses its real roots.
	//
	// DEV/TEST-ONLY (sp-wwtc.3): this is how the e2e-vm profile lets the sidecar's STRICT upstream
	// transport (newDefaultUpstreamTransport — InsecureSkipVerify stays off, unmodified) verify the
	// VM's golden CA when it MITMs github.com (fronting Gitea over real TLS) — without weakening
	// upstream verification and without baking a test CA into the production sidecar image (the
	// bundle lives on the node host and is only mounted when this is set). Empty (default): no
	// mount, no env override — the sidecar image's own system roots are used untouched. Set by
	// cmd/spawnlet from SIDECAR_CA_BUNDLE_FILE.
	SidecarCABundleFile string

	NodeID           string // this node's id (stamped on container labels for reconcile); "" standalone
	NodeClass        string // "cloud" (always enforces) or "self-hosted" (honors EgressEnforce)
	EgressEnforce    bool   // self-hosted opt-out switch; ignored on cloud
	EgressAllowCIDRs []string

	// EgressFloorForceOff bypasses the egress floor entirely — neither applied nor checked.
	// DEV-ONLY: this flag MUST NOT be set in production. It exists so the rootless dev node
	// can run under NODE_CLASS=cloud (multi-tenant placement) without kernel iptables access.
	// Treat this as an explicit local-development security bypass.
	EgressFloorForceOff bool

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
	// container via `rm -rf` before a suspend capture — on the gate path, BEFORE the
	// agent is paused (an exec cannot enter a paused container); see scrubForCapture.
	// Best-effort, non-fatal.  The deltamerge package
	// applies the same filter during squash.  Default:
	// ["/var/cache/apt", "/var/lib/apt/lists", "/tmp"]. When /tmp is scrubbed,
	// the default scrub recreates it with mode 1777 before CaptureDelta so tmux can
	// create its socket directory after resume.
	// Paths that overlap one of the spawn's mounts (at, under, or containing a container-side bind
	// target) are SKIPPED and logged at WARN — scrubbing them would delete the spawn's persistent
	// mounted data, and since the scrub runs before the mount snapshot the deletion would be captured
	// as authoritative (scrubguard.go, spec 2026-07-12-suspend-torn-snapshot-fix-design.md §4.2).
	DeltaScrubPaths []string
}

type Manager struct {
	pod              runtime.PodBackend
	cfg              ManagerConfig
	store            *Store
	backendResolver  storage.BackendResolver
	rootMaterializer runtime.RootMaterializer
	fw               firewall.Applier
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
	// artifacts stages non-sensitive create-time artifacts into a per-spawn tmpfs dir (sp-l5sx.3).
	// Always set (NewManagerWithBackend defaults ArtifactsRoot); bind-mounted into the agent at
	// ArtifactsMountPath; sensitive artifacts are routed to secrets by Materialize.
	artifacts ArtifactStager
	// gitEnv manages per-spawn WRITABLE git-env dirs (sp-7amh). Always set; bind-mounted at
	// GitEnvMountPath so the agent owns GIT_CONFIG_GLOBAL and can run `git config --global`.
	gitEnv GitEnv
	// githubCreds stores node-storage GitHub access tokens in a node-only tmpfs root. This root must
	// not be SecretInjector.DirFor(spawnID), because that path is bind-mounted into the agent.
	githubCreds GitHubCredentialStore
	// ghControl is the optional node-side GitHub credential control server (sp-n7iy.3). When set,
	// CreateWithSelection wires the sidecar's MITM proxy env (SIDECAR_GITHUB_PROXY_ADDR) and the
	// node PUSHES the CA + token into the sidecar after StartPod
	// (sp-2tx8.9 §3.1 — PushCredentials; there is no inbound listener any more). Nil = no control
	// server (dev/insecure lane); the sidecar gets no proxy wiring and the proxy cannot fetch tokens.
	ghControl GitHubControlServer

	// watchersMu guards sp.journalWatchers on each Spawn against concurrent access from
	// SnapshotForSuspend (which uses store.Get, leaving the spawn in the store) and
	// teardown (called via Stop after store.Claim). Both callers hold the same *Spawn
	// pointer, so without a lock a concurrent write (nil-out on success or restart on
	// abort) and a read (range in teardown) constitute a data race (sp-csks).
	// All accesses to sp.journalWatchers go through takeWatchers / setWatchers; w.Stop
	// is called outside the lock to avoid holding it across a potentially-blocking call.
	watchersMu sync.Mutex

	// deltaState durably records the per-spawn delta chain depth across node restarts so
	// a resumed spawn continues counting from where it left off.
	deltaState *deltaStateStore

	// scrubFn removes noisy paths from the agent container's writable layer before a suspend capture
	// (see scrubForCapture — it runs while the agent is still RUNNING, and on the gate path BEFORE the
	// Pause).  Default (set by NewManagerWithBackend) execs `rm -rf <paths>` directly against the agentID
	// container without routing through ExecRun/store-lookup: on the non-gate paths the spawn has already
	// been claimed from the store (removed), so an ExecRun lookup would always fail.
	// Injected as a seam in tests so the hermetic unit tests do not shell out to Docker.
	scrubFn func(ctx context.Context, agentID string, paths []string) error

	// squashNeeded is called when DeltaDepth reaches DeltaSquashDepth.
	// nil → log a "SQUASH-NEEDED" warning line.
	// Injected in tests so they can observe the callback without log parsing.
	squashNeeded func(id string, depth int)

	// forkSyncFn flushes host filesystem buffers while the source agent is paused for a fork capture.
	// The default calls sync(1); tests replace it with a recorder.
	forkSyncFn func(context.Context) error

	// forkGenerationHold protects the source generation's journal key/blobs from revoke/prune while
	// a fork is seeding. Required when the configured journal backend depends on generation-key
	// fencing; filesystem-backed dev/tests can leave it optional.
	generationKeys             *journal.GenerationKeyManager
	forkGenerationHold         func(spawnID string, gen uint64, reason string) generationHold
	forkGenerationHoldRequired bool
}

// GitHubControlServer is the node-side interface for the per-spawn GitHub credential plane. It is
// implemented by *node.githubControlServer and injected via SetGitHubControlServer. Nil-safe: not
// setting it leaves the manager in "no github control" mode (the sidecar gets no proxy wiring).
//
// It has no Serve: the node does NOT listen. sp-2tx8.9 inverted the channel — the node PUSHES the CA +
// token into the sidecar's own control listener (PushCredentials) and never binds an address a pod
// could dial.
type GitHubControlServer interface {
	// Stop cancels the spawn's push/rejection-watch loops and purges its CA. Called on stop/suspend.
	Stop(spawnID string)
	// SpawnCACert returns the PEM-encoded public certificate for spawnID's CA (generated lazily on
	// first call, stable across calls for the same spawn). Used to write the cert into the
	// agent-visible git-env before StartAgent.
	SpawnCACert(spawnID string) ([]byte, error)
	// PushCredentials delivers the spawn's MITM CA + a live GitHub access token to the sidecar's
	// control listener (sp-2tx8.9 §3.1). controlURL is the sidecar's /control/model URL; the
	// implementation rewrites the path to /control/github and authenticates with controlToken
	// (SIDECAR_CONTROL_TOKEN).
	//
	// It returns nil — a NO-OP, not a failure — when the spawn has no GitHub link: a spawn with no
	// github mount has no token to push, and the sidecar rejects a token-less push with a 400. Every
	// other error is real, and the create path is FAIL-CLOSED on it.
	PushCredentials(ctx context.Context, spawnID, controlURL, controlToken string) error
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

// SetGitHubControlServer installs the per-spawn GitHub credential control server (sp-n7iy.3).
// Call before CreateWithSelection to enable the sidecar's git-proxy wiring and the credential push.
// nil-safe field: not calling this leaves the manager in "no control server" mode.
func (m *Manager) SetGitHubControlServer(s GitHubControlServer) {
	m.ghControl = s
}

// NewManager builds a Manager on the Docker/runc path: the Docker pod backend + the DOCKER-USER
// egress floor. (cmd/spawnlet uses NewManagerWithBackend for the runsc/CRI path.)
func NewManager(rt runtime.ContainerRuntime, cfg ManagerConfig) *Manager {
	m := NewManagerWithBackend(
		runtime.NewDockerPodBackend(rt, cfg.ContainerRuntime, cfg.AgentImage),
		firewall.HostFloorApplier{},
		cfg,
	)
	if mat, ok := rt.(runtime.RootMaterializer); ok {
		m.rootMaterializer = mat
	}
	return m
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
	if cfg.ArtifactsRoot == "" {
		cfg.ArtifactsRoot = filepath.Join(cfg.DataRoot, "artifacts")
	}
	if cfg.GitEnvRoot == "" {
		cfg.GitEnvRoot = filepath.Join(cfg.DataRoot, "git-env")
	}
	if cfg.GitHubCredentialsRoot == "" {
		cfg.GitHubCredentialsRoot = filepath.Join(cfg.DataRoot, "github-creds")
	}
	if cfg.DeltaSquashDepth == 0 {
		cfg.DeltaSquashDepth = 16
	}
	if len(cfg.DeltaScrubPaths) == 0 {
		cfg.DeltaScrubPaths = []string{"/var/cache/apt", "/var/lib/apt/lists", "/tmp"}
	}
	// agentUID mirrors (*Manager).agentRootUID(), computed here since Manager doesn't exist yet:
	// the ArtifactStager needs it at construction to chown its report/ subdir (sp-mwco.2.7).
	agentUID := -1
	switch cfg.UsernsMode {
	case "remap":
		agentUID = int(cfg.UsernsRemapBase)
	case "native":
		agentUID = 0
	}
	m := &Manager{
		pod:             pod,
		cfg:             cfg,
		store:           NewStore(),
		backendResolver: storage.NewSchemeResolverWithGitHub(cfg.DataRoot, nil),
		fw:              fw,
		secrets:         SecretInjector{Root: cfg.SecretsRoot},
		artifacts:       ArtifactStager{Root: cfg.ArtifactsRoot, CacheDir: filepath.Join(cfg.DataRoot, "artifact-cache"), AgentUID: agentUID},
		gitEnv:          GitEnv{Root: cfg.GitEnvRoot},
		githubCreds:     GitHubCredentialStore{Root: cfg.GitHubCredentialsRoot},
		deltaState:      &deltaStateStore{dir: filepath.Join(cfg.DataRoot, "delta-state")},
	}
	if resolver, ok := m.backendResolver.(*storage.SchemeResolver); ok {
		// Static-token lane (local Gitea): a fixed-token provider replaces the AS-mint-backed
		// Manager provider. Otherwise the Manager itself resolves per-mount node-rendered tokens.
		if cfg.GitHubStaticCredentials != nil {
			resolver.SetGitHubCredentials(cfg.GitHubStaticCredentials)
		} else {
			resolver.SetGitHubCredentials(m)
		}
		resolver.SetGitHubServices(cfg.GitHubRepos, cfg.GitHubGitRunner)
		resolver.SetGitHubHostOverride(cfg.GitHubHost, cfg.GitHubAllowInsecureHost)
	}
	m.forkSyncFn = func(ctx context.Context) error {
		return exec.CommandContext(ctx, "sync").Run()
	}
	// Default scrub function: exec scrub commands directly in the agent container before commit.
	// This runs while the container is still live (before pod.Stop).
	// IMPORTANT: we exec by agentID directly — NOT via ExecRun — because by the time teardown
	// calls scrubFn the spawn has already been removed from the store by Claim (in Stop/Suspend/
	// Delete), so ExecRun's store.Get would always return "no agent container".
	// The seam allows unit tests to inject a fake without shelling out to Docker.
	m.scrubFn = func(ctx context.Context, agentID string, paths []string) error {
		if agentID == "" {
			return fmt.Errorf("scrub: no agent container id")
		}
		var firstErr error
		for _, args := range defaultScrubCommands(paths) {
			argv := execArgv(ExecPrefixNonInteractiveFor(m.cfg.ContainerRuntime), agentID, args)
			out, err := exec.CommandContext(ctx, argv[0], argv[1:]...).CombinedOutput()
			if err != nil && firstErr == nil {
				firstErr = fmt.Errorf("exec %v: %w (%s)", args, err, out)
			}
		}
		return firstErr
	}
	return m
}

func defaultScrubCommands(paths []string) [][]string {
	if len(paths) == 0 {
		return nil
	}
	commands := [][]string{append([]string{"rm", "-rf"}, paths...)}
	if scrubPathsIncludeTmp(paths) {
		commands = append(commands,
			[]string{"mkdir", "-p", "/tmp"},
			[]string{"chmod", "1777", "/tmp"},
		)
	}
	return commands
}

// scrubForCapture runs the best-effort layer-hygiene scrub (`rm -rf <DeltaScrubPaths>`, via an exec in
// the agent container) so a captured delta does not carry apt caches and /tmp noise.
//
// It MUST run while the agent is still RUNNING: the scrub is an exec, and an exec cannot enter a paused
// container ("container is paused"). It therefore runs BEFORE the suspend gate's Pause — never after it.
// It used to live in teardown, which forced an Unpause between the mount snapshot and the rootfs capture
// and produced a TORN snapshot (sp-2tx8.2.1, spec §4.1). The scrub's guarantee is best-effort hygiene and
// it can tolerate a race (a live process recreating /tmp/x before the pause costs a few bytes of layer);
// the snapshot's guarantee is consistency and it can tolerate none. The tolerant operation goes outside
// the quiesced window; the intolerant one stays strictly inside it.
//
// Accepted side effect (spec §4.4): an ABORTED suspend (the gate's journal snapshot fails, the spawn keeps
// running) now leaves the scrub paths already cleaned. DeltaScrubPaths are disposable by definition (/tmp,
// package caches) — a successful suspend/resume wipes them anyway — so a certain torn snapshot is not worth
// trading for an unlikely, harmless /tmp clean.
//
// Non-fatal by contract: a scrub failure is logged and the suspend proceeds with a fatter delta layer.
func (m *Manager) scrubForCapture(ctx context.Context, sp *Spawn) {
	if !m.cfg.DeltaCapture || m.scrubFn == nil || len(m.cfg.DeltaScrubPaths) == 0 || sp.AgentID == "" {
		return
	}
	// Delegate to the MOUNT-GUARDED scrub (sp-2tx8.2.2, scrubguard.go): a configured scrub path that
	// overlaps one of this spawn's mounts is skipped and logged, never deleted. That guard is what makes
	// running the scrub before the mount snapshot safe — without it this reorder would rm -rf the user's
	// persistent data and then snapshot the deletion as authoritative.
	m.runDeltaScrub(ctx, sp)
}

func scrubPathsIncludeTmp(paths []string) bool {
	for _, p := range paths {
		if filepath.Clean(p) == "/tmp" {
			return true
		}
	}
	return false
}

// egressEnforced reports whether the egress floor must be applied: cloud nodes always enforce
// (non-disableable); self-hosted honors the operator's EgressEnforce choice.
// EgressFloorForceOff overrides both — DEV-ONLY, see ManagerConfig.EgressFloorForceOff.
func (m *Manager) egressEnforced() bool {
	if m.cfg.EgressFloorForceOff {
		return false // DEV-ONLY override; MUST NOT be set in production
	}
	return m.cfg.NodeClass == "cloud" || m.cfg.EgressEnforce
}

func (m *Manager) Store() *Store { return m.store }

// EgressEnforced reports whether the egress floor is currently enforced.
// Used by the node (attach.go) to compute ProvisionFlags.SetupNetwork.
func (m *Manager) EgressEnforced() bool { return m.egressEnforced() }

// GitHubControlEnabled reports whether the GitHub credential control server is installed.
// Used by the node (attach.go) to compute ProvisionFlags.SetupNetwork.
func (m *Manager) GitHubControlEnabled() bool { return m.ghControl != nil }

// GitHubStaticCredentialsEnabled reports whether this node resolves github: mount tokens from a
// fixed static provider (GITHUB_STATIC_TOKEN) instead of the AS mint. When true the node skips
// mint-at-provision — the static provider supplies the clone token directly (local-Gitea lane).
func (m *Manager) GitHubStaticCredentialsEnabled() bool { return m.cfg.GitHubStaticCredentials != nil }

// HasJournalPins reports whether spawnID has a durable journal record with at least one
// mount manifest pinned — i.e., a same-node resume will attempt to restore journal state.
// Used by the node (attach.go) to compute ProvisionFlags.RestoreSnapshot.
func (m *Manager) HasJournalPins(id string) bool {
	if m.journal == nil || m.journalState == nil {
		return false
	}
	rec, ok, err := m.journalState.Load(id)
	return err == nil && ok && len(rec.Manifests) > 0
}

// RemapBase returns the userns-remap base UID learned at startup from the Docker daemon probe
// (spec §2). Returns 0 when USERNS_MODE is not "remap" or the probe found no active remap.
// Consumed by the storage layer (.8) to compute host-side ownership for userns-remapped mounts.
func (m *Manager) RemapBase() uint32 { return m.cfg.UsernsRemapBase }

func (m *Manager) scratchBackend() storage.Backend {
	return storage.NewScratch(m.cfg.DataRoot)
}

// syncMaterializedMount mirrors the actual root-owned bind target back into the backend-prepared
// working dir before Finalize runs, so remap-mode mounts still route through the resolved backend.
func syncMaterializedMount(srcDir, dstDir string) error {
	srcAbs, err := filepath.Abs(srcDir)
	if err != nil {
		return err
	}
	dstAbs, err := filepath.Abs(dstDir)
	if err != nil {
		return err
	}
	if filepath.Clean(srcAbs) == filepath.Clean(dstAbs) {
		return fmt.Errorf("sync materialized mount: source and destination must not be the same path (%s)", srcDir)
	}
	if srcInfo, err := os.Stat(srcDir); err == nil {
		if dstInfo, derr := os.Stat(dstDir); derr == nil && os.SameFile(srcInfo, dstInfo) {
			return fmt.Errorf("sync materialized mount: source and destination must not be the same file (%s)", srcDir)
		}
	}

	parent := filepath.Dir(dstDir)
	tmpDir, err := os.MkdirTemp(parent, filepath.Base(dstDir)+".sync-*")
	if err != nil {
		return err
	}
	cleanupTmp := true
	defer func() {
		if cleanupTmp {
			_ = os.RemoveAll(tmpDir)
		}
	}()

	if err := filepath.WalkDir(srcDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == srcDir {
			return nil
		}
		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		dstPath := filepath.Join(tmpDir, rel)
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		mode := info.Mode()
		switch {
		case mode.IsDir():
			return os.MkdirAll(dstPath, mode.Perm())
		case mode&os.ModeSymlink != 0:
			if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
				return err
			}
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(target, dstPath)
		case mode.IsRegular():
			if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
				return err
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if err := os.WriteFile(dstPath, b, mode.Perm()); err != nil {
				return err
			}
			return os.Chmod(dstPath, mode.Perm())
		default:
			return fmt.Errorf("sync materialized mount: unsupported file mode %v at %s", mode, path)
		}
	}); err != nil {
		return err
	}

	backupDir := filepath.Join(parent, fmt.Sprintf(".%s.backup-%d", filepath.Base(dstDir), time.Now().UnixNano()))
	hadDst := false
	if _, err := os.Lstat(dstDir); err == nil {
		hadDst = true
		if err := os.Rename(dstDir, backupDir); err != nil {
			return err
		}
	}
	if err := os.Rename(tmpDir, dstDir); err != nil {
		if hadDst {
			_ = os.Rename(backupDir, dstDir)
		}
		return err
	}
	cleanupTmp = false
	if hadDst {
		if err := os.RemoveAll(backupDir); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) finalizeMountDirs(ctx context.Context, mountDirs []string, finalizers []MountFinalizer) error {
	if len(finalizers) == 0 {
		scratch := m.scratchBackend()
		var errs []error
		for _, dir := range mountDirs {
			if err := scratch.Finalize(ctx, dir); err != nil {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	}
	var errs []error
	for _, finalizer := range finalizers {
		backend := finalizer.Backend
		if backend == nil {
			backend = m.scratchBackend()
		}
		if finalizer.SyncFrom != "" {
			if err := syncMaterializedMount(finalizer.SyncFrom, finalizer.HostDir); err != nil {
				errs = append(errs, fmt.Errorf("sync materialized mount %s -> %s: %w", finalizer.SyncFrom, finalizer.HostDir, err))
				continue
			}
		}
		if err := backend.Finalize(ctx, finalizer.HostDir); err != nil {
			errs = append(errs, fmt.Errorf("finalize mount dir %s: %w", finalizer.HostDir, err))
			continue
		}
		if finalizer.CleanupDir != "" {
			_ = m.scratchBackend().Finalize(ctx, finalizer.CleanupDir)
		}
	}
	return errors.Join(errs...)
}

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

func (m *Manager) useRootMaterializer() bool {
	return m.cfg.UsernsMode == "remap" && m.rootMaterializer != nil
}

func (m *Manager) materializeRootOwned(ctx context.Context, helperImage, sourcePath, targetPath string, dirMode, fileMode os.FileMode) error {
	return m.rootMaterializer.MaterializeRootOwned(ctx, runtime.RootMaterializeSpec{
		Image:      helperImage,
		SourcePath: sourcePath,
		TargetPath: targetPath,
		DirMode:    dirMode,
		FileMode:   fileMode,
	})
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

// TmuxHasSession runs `tmux has-session -t session` non-interactively in agentID's container and
// reports whether the named tmux session exists. Returns (true, nil) when exit 0, (false, nil) when
// tmux exits non-zero (session absent — the normal "not yet created" case), and (false, err) when the
// exec itself cannot be started (daemon unreachable, binary missing, etc.). Used by the node to gate
// tmux-mode spawns ACTIVE until the in-container session is present (sp-m859.4).
func (m *Manager) TmuxHasSession(ctx context.Context, agentID, session string) (bool, error) {
	argv := execArgv(ExecPrefixNonInteractiveFor(m.cfg.ContainerRuntime), agentID, []string{"tmux", "has-session", "-t", session})
	err := exec.CommandContext(ctx, argv[0], argv[1:]...).Run()
	if err == nil {
		return true, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return false, nil // tmux has-session exits non-zero when the session does not exist
	}
	return false, fmt.Errorf("tmux has-session: %w", err)
}

// AgentRunning reports whether spawnID's agent container is still running, by running a trivial
// no-op (`true`) non-interactively inside it — used by the node's AwaitApplyReport liveness probe
// (sp-mwco.2.12 ITEM D) to end the wait as soon as the agent is confirmed gone, instead of sitting
// out a timeout that a dead container was never going to satisfy. Returns (true, nil) on exit 0,
// (false, nil) when the exec runs but the container is gone (docker/crictl exec exits non-zero —
// same convention as TmuxHasSession), and (false, err) when the exec itself could not even be
// launched (daemon unreachable, binary missing, unknown spawn) — an "unknown" result the caller
// must not treat as death.
//
// RATIONALE: this reuses the existing exec seam (same as TmuxHasSession) rather than growing
// PodBackend a new AgentRunning method across the Docker + CRI backends and every test fake. Cost:
// a docker/crictl-daemon outage looks identical to "container gone" — but a dead daemon means the
// spawn is doomed anyway, and AwaitApplyReport's two-consecutive-false rule plus the injectable
// probe on the attacher (internal/node) make a later swap to a proper backend inspect a
// contained, one-seam change if that ambiguity ever needs resolving.
func (m *Manager) AgentRunning(ctx context.Context, spawnID string) (bool, error) {
	sp, ok := m.store.Get(spawnID)
	if !ok || sp.AgentID == "" {
		return false, fmt.Errorf("spawn %s has no agent container", spawnID)
	}
	argv := execArgv(ExecPrefixNonInteractiveFor(m.cfg.ContainerRuntime), sp.AgentID, []string{"true"})
	err := exec.CommandContext(ctx, argv[0], argv[1:]...).Run()
	if err == nil {
		return true, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return false, nil // exec ran but the container/exec target is gone
	}
	return false, fmt.Errorf("agent running probe: %w", err)
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

// ExecStream runs inner non-interactively in spawnID's agent container, streaming its stdout/stderr to
// the given writers as they arrive, and returns the inner command's exit code. It is the user-facing
// `spawnctl exec` path (sp-8v39). Unlike ExecRun (buffered, error-on-nonzero), a non-zero command exit
// is returned as exitCode with a nil error; err is reserved for setup, transport, and cancellation
// failures. `docker
// exec` propagates the inner exit code and demuxes stdout/stderr natively; `crictl exec` (runsc/CRI
// lane) demuxes but does NOT propagate the code (see runExecStream's parseCrictlExit).
func (m *Manager) ExecStream(ctx context.Context, spawnID string, inner []string, stdout, stderr io.Writer) (int, error) {
	sp, ok := m.store.Get(spawnID)
	if !ok || sp.AgentID == "" {
		return 1, fmt.Errorf("spawn %s has no agent container", spawnID)
	}
	if err := ctx.Err(); err != nil {
		return 1, err
	}
	process, err := newExecProcess()
	if err != nil {
		return 1, err
	}
	wrapped, err := process.wrapArgv(inner)
	if err != nil {
		return 1, err
	}
	prefix := execPrefixWithStdin(ExecPrefixNonInteractiveFor(m.cfg.ContainerRuntime))
	argv := execArgv(prefix, sp.AgentID, wrapped)
	return runExecStreamCancelable(ctx, argv, stdout, stderr, len(prefix) > 0 && prefix[0] == "crictl", process)
}

// runExecStream runs argv to completion, streaming its stdout/stderr to the given writers, and returns
// the process's exit code. A non-zero exit is returned as the code with a nil error; err is reserved
// for a failure to START the process (e.g. the runtime CLI is missing). Split out from ExecStream (the
// container-resolution wrapper) so the exit-code/stream-demux logic is testable without a container.
//
// parseCrictlExit: on the runsc/CRI lane the runtime CLI is `crictl`, which — unlike `docker exec` —
// does NOT propagate the inner process's exit code as its own. It exits 1 for ANY non-zero inner
// status and reports the real code only in a stderr line "command terminated with exit code N". When
// set, we tee stderr and parse that line so `spawnctl exec` still propagates the true code.
func runExecStream(ctx context.Context, argv []string, stdout, stderr io.Writer, parseCrictlExit bool) (int, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdout = stdout
	var errTail *bytes.Buffer
	if parseCrictlExit {
		errTail = &bytes.Buffer{}
		cmd.Stderr = io.MultiWriter(stderr, errTail)
	} else {
		cmd.Stderr = stderr
	}
	if err := cmd.Start(); err != nil {
		return 1, fmt.Errorf("exec %v: %w", argv, err)
	}
	if err := cmd.Wait(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			if parseCrictlExit {
				if code, ok := parseCrictlExitCode(errTail.Bytes()); ok {
					return code, nil // crictl's real inner exit code, parsed from its stderr line
				}
			}
			return ee.ExitCode(), nil // command ran to completion with a non-zero status
		}
		return 1, fmt.Errorf("exec %v: %w", argv, err)
	}
	return 0, nil
}

// parseCrictlExitCode extracts N from crictl exec's "command terminated with exit code N" stderr line
// — the only place cri-tools surfaces a non-zero inner exit code (it always exits 1 itself). Returns
// (0, false) when the line is absent (so the caller falls back to crictl's own exit code).
func parseCrictlExitCode(stderr []byte) (int, bool) {
	const marker = "command terminated with exit code "
	i := bytes.LastIndex(stderr, []byte(marker))
	if i < 0 {
		return 0, false
	}
	rest := stderr[i+len(marker):]
	j := 0
	for j < len(rest) && rest[j] >= '0' && rest[j] <= '9' {
		j++
	}
	if j == 0 {
		return 0, false
	}
	n, err := strconv.Atoi(string(rest[:j]))
	if err != nil {
		return 0, false
	}
	return n, true
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

func (m *Manager) SpawnOwnerGeneration(id string) (string, uint64, bool) {
	return m.store.OwnerGeneration(id)
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

// StopAll tears down every spawn this Manager tracks — the DESTRUCTIVE bulk path. It is NOT the
// process-shutdown path any more: a SIGTERM'd node calls DetachAll and leaves its pods running for
// the next process to re-adopt (SE3 §4.1). Keep this for an explicit drain/destroy-everything caller.
// Returns the number of spawns it stopped.
func (m *Manager) StopAll(ctx context.Context) int {
	sps := m.store.List()
	for _, sp := range sps {
		if err := m.Stop(ctx, sp.ID); err != nil {
			log.Printf("stopAll: stop %s: %v", sp.ID, err)
		}
	}
	return len(sps)
}

// DetachAll relinquishes supervision of every spawn this Manager tracks WITHOUT touching the pods:
// the containers keep running so the next spawnlet process can re-adopt them (SE3 §4.1). It is the
// PROCESS-SHUTDOWN path (SIGTERM / `systemctl restart` — the documented upgrade path); the
// spawn-deletion path (Stop/StopAll/teardown) is unchanged and still destroys.
//
// It stops each spawn's continuous journal watchers so no snapshot is left racing process exit, and
// deliberately does NOT: call pod.Stop, run the mount finalizers, or clear the node's delta/journal
// state stores — those records are what re-adoption (sp-2tx8.3.4) reads back. The in-memory store is
// left as-is: the process is exiting, and mutating it buys nothing.
//
// Consequence to accept (spec §4.1): if this node never comes back, its pods run unsupervised. They
// are labelled, so a future spawnlet on this machine reconciles them and the CP sees the spawns as
// Unreachable — strictly better than today, where a restart destroys them unconditionally.
//
// Returns the number of spawns left running.
func (m *Manager) DetachAll() int {
	sps := m.store.List()
	for _, sp := range sps {
		// takeWatchers clears sp.journalWatchers under watchersMu; Stop() blocks until the watcher
		// goroutine exits and MUST be called outside that lock (see takeWatchers' contract).
		for _, w := range m.takeWatchers(sp) {
			w.Stop()
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
	Mounts     []MountBinding
	// BaseImageDigest is the CP-pinned base image digest for cross-node resume (spec §4).
	// Empty on fresh create (the node resolves the digest at create time via ResolveImageDigest).
	// On resume/recreate the CP threads the stored digest down so the node uses the exact base.
	BaseImageDigest string
	// RootfsSourceGeneration and RootfsArtifacts are CP-pinned migration restore inputs.
	// Normal same-node resume leaves them empty and continues to use the local DeltaImageRef.
	RootfsSourceGeneration uint64
	RootfsArtifacts        []RootfsArtifact
	// RootfsArtifactsLocalOnly means RootfsArtifacts describe an already-local fork delta image.
	// The node must launch runtime.DeltaTag(id) without restoring artifacts, and must fail if
	// that local image is absent.
	RootfsArtifactsLocalOnly bool
	// ProgressFunc is an optional callback invoked at phase boundaries during CreateWithSelection
	// so that callers (attach.go startSpawn) can relay resume progress to the CP stall detector
	// (sp-u53.7.2) and emit provisioning milestone events (sp-m859.3). stepKey identifies the
	// milestone in the catalog; empty = phase-only event with no milestone index update.
	// nil = no-op.
	ProgressFunc func(phase, detail, stepKey string)
	// Artifacts are the per-spawn create-time artifacts re-threaded on every StartSpawn (including
	// resume). Non-sensitive artifacts are materialized into the staging tmpfs at ArtifactsMountPath;
	// sensitive+inline artifacts are routed to the secrets tmpfs. Converted from proto by the node.
	Artifacts []Artifact
	// RepresignFunc mints a fresh presigned URL for a by-ref artifact whose GET failed because the
	// presign expired (sp-mwco.4.2; set by internal/node over the Attach stream — sp-mwco.4.3).
	// nil ⇒ an expired presign is Terminal instead of recovered.
	RepresignFunc RepresignFunc
	// BeforeStartAgent runs after the sidecar pod and pre-agent prep complete, immediately before the
	// untrusted agent starts. It can stage spawn-local secrets before the spawn is visible in the store.
	BeforeStartAgent func(context.Context, PreAgentContext) error
}

// progress is a nil-safe helper that calls sel.ProgressFunc when set.
func (sel AgentSelection) progress(phase, detail, stepKey string) {
	if sel.ProgressFunc != nil {
		sel.ProgressFunc(phase, detail, stepKey)
	}
}

type PreAgentContext struct {
	SpawnID      string
	Generation   uint64
	ControlURL   string
	ControlToken string
	InjectSecret func(target string, plaintext []byte) (string, error)
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

func cloneRootfsArtifacts(in []RootfsArtifact) []RootfsArtifact {
	if len(in) == 0 {
		return nil
	}
	out := make([]RootfsArtifact, len(in))
	copy(out, in)
	return out
}

type SuspendResult struct {
	MountMarkers    map[string]string
	RootfsArtifacts []RootfsArtifact
}

// SuspendProgressFunc is an optional callback invoked at phase boundaries of SnapshotForSuspend
// and FinishSuspend/teardown so that callers (attach.go) can relay progress signals upstream
// (e.g. to the CP via the Attach stream — sp-u53.7.2). nil = no-op (Stop, Delete, SuspendForMigration
// callers pass nil; only the gate/finish sequence wires this).
// snapshotJournal calls this ONCE PER JOURNALED MOUNT (not just once for all mounts) so a
// multi-mount suspend or a single large mount resets the stall timer between mounts. The markers
// parameter carries the just-completed mount's persist marker (non-nil only when a mount finishes)
// so the CP can accumulate partial markers incrementally (sp-u53.7.2 B). Byte-level intra-mount
// granularity (a journal.FinalSnapshot progress hook) is a follow-up.
type SuspendProgressFunc func(phase, detail string, markers map[string]string)

func (m *Manager) Create(ctx context.Context, id, appPath, model, name, appID string, generation uint64) (*Spawn, error) {
	return m.CreateWithSelection(ctx, id, appPath, model, name, appID, generation, AgentSelection{})
}

// CreateWithSelection is Create plus an explicit agent selection (image + runnable id + mode).
// For any selected runnable the image-entrypoint argument vector is set to [sel.RunnableID]; the image's
// dispatcher entrypoint (entrypoint.sh) resolves the actual launch (serve+adapter, tmux-wrapped
// TUI, etc.) — the node just names the runnable. No selection leaves Cmd nil (image default).
func (m *Manager) CreateWithSelection(ctx context.Context, id, appPath, model, name, appID string, generation uint64, sel AgentSelection) (*Spawn, error) {
	return m.createWithSelection(ctx, id, appPath, model, name, appID, generation, "", sel)
}

func (m *Manager) CreateAuthorizedWithSelection(ctx context.Context, id, appPath, model, name, appID string, generation uint64, ownerID string, sel AgentSelection) (*Spawn, error) {
	reservation, err := m.ReserveAuthorizedSpawn(id, ownerID, generation)
	if err != nil {
		return nil, err
	}
	defer m.ReleaseAuthorizedSpawn(reservation)
	return m.CreateReservedWithSelection(ctx, reservation, appPath, model, name, appID, sel)
}

func (m *Manager) ReserveAuthorizedSpawn(id, ownerID string, generation uint64) (*OwnerReservation, error) {
	if ownerID == "" {
		return nil, fmt.Errorf("authorized spawn owner is empty")
	}
	reservation, ok := m.store.ReserveOwner(id, ownerID, generation)
	if !ok {
		return nil, fmt.Errorf("spawn %q is already reserved or live", id)
	}
	return reservation, nil
}

func (m *Manager) ReleaseAuthorizedSpawn(reservation *OwnerReservation) {
	m.store.ReleaseOwner(reservation)
}

func (m *Manager) OwnsAuthorizedSpawn(reservation *OwnerReservation) bool {
	return m.store.OwnsReservation(reservation)
}

func (m *Manager) CreateReservedWithSelection(ctx context.Context, reservation *OwnerReservation, appPath, model, name, appID string, sel AgentSelection) (*Spawn, error) {
	if !m.store.OwnsReservation(reservation) {
		return nil, fmt.Errorf("authorized spawn reservation is not current")
	}
	return m.createWithReservation(ctx, reservation.id, appPath, model, name, appID, reservation.generation, reservation.owner, reservation, sel)
}

func (m *Manager) createWithSelection(ctx context.Context, id, appPath, model, name, appID string, generation uint64, ownerID string, sel AgentSelection) (*Spawn, error) {
	return m.createWithReservation(ctx, id, appPath, model, name, appID, generation, ownerID, nil, sel)
}

func (m *Manager) createWithReservation(ctx context.Context, id, appPath, model, name, appID string, generation uint64, ownerID string, reservation *OwnerReservation, sel AgentSelection) (*Spawn, error) {
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
		// the node passes only the runnable ID as its argument. (Replaces the old spawn-tmux +
		// agentcaps.Launch prepend.)
		agentCmd = []string{sel.RunnableID}
	}

	// Provisioning milestone: prepare-mounts (sp-m859.3). Emitted here (before manifest.Parse)
	// so it fires at the start of the mount/storage preparation phase.
	sel.progress("preparing_mounts", "preparing mounts", MilestonePrepareMounts)

	if abs, err := filepath.Abs(appPath); err == nil {
		appPath = abs
	}
	mf, err := manifest.Parse(appPath)
	if err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	mountBackendBindings, err := mountBindingsByName(mf.Storage.Mounts, sel.Mounts)
	if err != nil {
		return nil, err
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

	var mountDirs []string
	var mountFinalizers []MountFinalizer
	var journalMounts []journal.Mount
	finalizeAll := func() {
		_ = m.finalizeMountDirs(ctx, mountDirs, mountFinalizers)
	}
	rootMaterialize := m.useRootMaterializer()
	helperImage := m.cfg.AgentImage
	scratchBackend := m.scratchBackend()
	resolver := m.backendResolver
	if resolver == nil {
		resolver = storage.NewSchemeResolver(m.cfg.DataRoot)
	}

	// /app is read-only; each declared mount is a rw overlay at /app/<path>,
	// backed (slice: scratch) by a host dir seeded from /app/<seed>.
	appMountPath := appPath
	if rootMaterialize {
		appMountPath = filepath.Join(m.cfg.DataRoot, id, "app")
		if err := m.materializeRootOwned(ctx, helperImage, appPath, appMountPath, 0o777, 0o644); err != nil {
			_ = scratchBackend.Finalize(ctx, appMountPath)
			return nil, fmt.Errorf("materialize /app: %w", err)
		}
		mountDirs = append(mountDirs, appMountPath)
		mountFinalizers = append(mountFinalizers, MountFinalizer{HostDir: appMountPath, Backend: scratchBackend})
	}
	mounts := []runtime.Mount{{
		HostPath:             appMountPath,
		ContainerPath:        "/app",
		ReadOnly:             true,
		SELinuxRelabelShared: rootMaterialize,
	}}

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

	agentUID := m.agentRootUID()
	for _, mt := range mf.Storage.Mounts {
		binding := mountBackendBindings[mt.Name]
		if binding.Name == "" {
			binding.Name = mt.Name
		}
		backendURI := binding.BackendURI
		class, derr := journal.ParseDurability(mt.Durability)
		if derr != nil {
			finalizeAll()
			return nil, fmt.Errorf("mount %q durability: %w", mt.Name, derr)
		}
		if storage.IsGitHubBackendURI(backendURI) && !class.Journaled() {
			finalizeAll()
			return nil, fmt.Errorf("mount %q github backend requires a journaled durability class", mt.Name)
		}
		mountBackend, err := resolveMountBackend(resolver, binding)
		if err != nil {
			finalizeAll()
			return nil, fmt.Errorf("mount %q backend %q: %w", mt.Name, backendURI, err)
		}
		// Resume clone-skip (spec §16.7): if a journal restore will repopulate this mount, tell a
		// restore-aware backend (github) to skip the network clone — the journal is authoritative.
		_, hasPin := jrec.Manifests[mt.Name]
		applyRestoreHint(mountBackend, haveJournalRecord && class.Journaled() && hasPin)
		// A mount seeds only from an explicitly declared seed dir. With no seed, seedDir stays
		// empty (backends treat a missing seed as "empty mount") — never fall back to the whole
		// app dir, which would copy the app's own files (e.g. AGENTS.md, the manifest) into the mount.
		seedDir := ""
		if mt.Seed != "" {
			seedDir = filepath.Join(appPath, mt.Seed)
		}
		hostDir := ""
		restoreDir := ""
		preparedDir := ""
		if rootMaterialize {
			prepareName := mt.Name + stageMountNameSuffix
			preparedDir, err = mountBackend.Prepare(ctx, id, prepareName, seedDir, -1)
			if err != nil {
				finalizeAll()
				return nil, fmt.Errorf("prepare mount %q: %w", mt.Name, err)
			}
			restoreDir = preparedDir
			hostDir = filepath.Join(m.cfg.DataRoot, id, mt.Name)
			if filepath.Clean(preparedDir) == filepath.Clean(hostDir) {
				cleanupPrepared := func() { _ = mountBackend.Finalize(ctx, preparedDir) }
				cleanupPrepared()
				finalizeAll()
				return nil, fmt.Errorf("prepare mount %q: root-materialized prepared dir must differ from bind target", mt.Name)
			}
		} else {
			hostDir, err = mountBackend.Prepare(ctx, id, mt.Name, seedDir, agentUID)
			if err != nil {
				finalizeAll()
				return nil, fmt.Errorf("prepare mount %q: %w", mt.Name, err)
			}
			restoreDir = hostDir
		}
		cleanupPrepared := func() {
			if preparedDir != "" {
				_ = mountBackend.Finalize(ctx, preparedDir)
			}
		}

		// Transient-tier seam (design §1a/§3). Journaling only engages for mounts
		// that opt into a journaled durability class; ephemeral mounts (the
		// default) leave the scratch path entirely untouched.
		jm := journal.Mount{Name: mt.Name, HostDir: hostDir, Class: class}
		if m.journal != nil && jm.Class.Journaled() {
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
					// Provisioning milestone: restore-snapshot (journal path, sp-m859.3).
					// Non-fatal: a restore failure falls back to the seeded dir. Emitting
					// "running" here so a sidecar-probe failure later attributes to start-agent,
					// not restore-snapshot (catalog-order vs code-order limitation, documented).
					sel.progress("restoring_snapshot", "restoring "+mt.Name, MilestoneRestoreSnapshot)
					restore := true
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
							restore = false
						}
						cancel()
					}
					if restore {
						if rerr := m.journal.RestoreGeneration(ctx, id, jrec.Generation, mt.Name, pin, restoreDir); rerr != nil {
							log.Printf("journal restore for %s mount %s (manifest %s): %v", id, mt.Name, pin, rerr)
						} else {
							// Restore writes files owned by THIS node daemon's uid with their original
							// modes; under userns-remap that uid is outside the agent's range. In the
							// direct-bind path, NormalizeOwnership is the final owner/mode authority.
							// In the root-materialized path it makes the staging tree readable by the
							// helper; the helper then recreates the actual bind root as container-root.
							normalizeUID := agentUID
							if rootMaterialize {
								normalizeUID = -1
							}
							if nerr := storage.NormalizeOwnership(restoreDir, normalizeUID); nerr != nil {
								log.Printf("journal restore for %s mount %s: normalize ownership: %v", id, mt.Name, nerr)
							}
							log.Printf("journal: spawn=%s mount=%s restored from manifest=%s", id, mt.Name, pin)
						}
					}
				}
			}
			journalMounts = append(journalMounts, jm)
		}

		if rootMaterialize {
			if err := m.materializeRootOwned(ctx, helperImage, restoreDir, hostDir, 0o777, 0o666); err != nil {
				cleanupPrepared()
				_ = scratchBackend.Finalize(ctx, hostDir)
				finalizeAll()
				return nil, fmt.Errorf("materialize mount %q: %w", mt.Name, err)
			}
		}

		mountDirs = append(mountDirs, hostDir)
		finalizerBackend := mountBackend
		if rootMaterialize {
			mountFinalizers = append(mountFinalizers, MountFinalizer{
				HostDir:    preparedDir,
				Backend:    mountBackend,
				SyncFrom:   hostDir,
				CleanupDir: hostDir,
			})
		} else {
			mountFinalizers = append(mountFinalizers, MountFinalizer{HostDir: hostDir, Backend: finalizerBackend})
		}
		mounts = append(mounts, runtime.Mount{
			HostPath:             hostDir,
			ContainerPath:        "/app/" + mt.Path,
			SELinuxRelabelShared: rootMaterialize,
		})
	}

	// Provisioning milestone: create-pod (sp-m859.3). Emitted before secrets/artifacts/git-env
	// preparation so it fires at the start of the pod-creation phase.
	sel.progress("creating_pod", "creating pod", MilestoneCreatePod)

	// Owner-sealed secrets tmpfs (design §6): a per-spawn dir under SecretsRoot, bind-mounted into the
	// agent at SecretsMountPath. The node writes unsealed plaintext here on SecretDelivery; the agent
	// reads its credentials in place. Created empty at start (secrets arrive over the delivery protocol,
	// not at spawn time) and removed on teardown. SecretsRoot should be a tmpfs in production.
	secretsDir := m.secrets.DirFor(id)
	if err := os.MkdirAll(secretsDir, 0o700); err != nil {
		finalizeAll()
		return nil, fmt.Errorf("prepare secrets dir: %w", err)
	}
	cleanupSpawnDirs := func() {
		if serr := m.secrets.Remove(id); serr != nil {
			log.Printf("secrets dir cleanup for %s: %v", id, serr)
		}
		if aerr := m.artifacts.Remove(id); aerr != nil {
			log.Printf("artifacts dir cleanup for %s: %v", id, aerr)
		}
		if geerr := m.gitEnv.Remove(id); geerr != nil {
			log.Printf("git-env dir cleanup for %s: %v", id, geerr)
		}
		if gerr := m.githubCreds.Remove(id); gerr != nil {
			log.Printf("github credential cleanup for %s: %v", id, gerr)
		}
		// Control server cleanup (sp-n7iy.3): cancel the spawn's push loop + rejection watch and
		// purge its CA (no-op when no control server is installed).
		if m.ghControl != nil {
			m.ghControl.Stop(id)
		}
	}
	mounts = append(mounts, runtime.Mount{HostPath: secretsDir, ContainerPath: SecretsMountPath})

	// Non-sensitive artifact staging tmpfs (cross-agent installer, sp-l5sx.3): a per-spawn dir under
	// ArtifactsRoot, bind-mounted at ArtifactsMountPath. Re-applied idempotently on every create/resume
	// (artifacts are create-time-declared but durable across the spawn's life). Sensitive artifacts are
	// routed to the secrets tmpfs (0600) by Materialize, never landed here.
	if err := m.artifacts.Materialize(ctx, id, sel.Artifacts, m.secrets, sel.ProgressFunc, sel.RepresignFunc); err != nil {
		finalizeAll()
		cleanupSpawnDirs()
		// %s of the SAFE message, not %w of the raw chain: err may wrap a *FetchError whose cause is
		// a *url.Error carrying the presigned URL's X-Amz-Signature query. This return value is what
		// internal/node's attach.go stringifies into SpawnStatus.Detail, which the CP persists to the
		// DB and renders in the web UI (sp-mwco.4.2) — it must never carry the presigned URL.
		return nil, fmt.Errorf("prepare artifacts: %s", SafeErrorMessage(err))
	}
	mounts = append(mounts, runtime.Mount{HostPath: m.artifacts.DirFor(id), ContainerPath: ArtifactsMountPath, SELinuxRelabelShared: true})

	// Writable agent-owned git-env (sp-7amh, design §1.1): a per-spawn dir chowned to the agent's mapped
	// uid (mirrors storage chown) so the agent owns GIT_CONFIG_GLOBAL and can `git config --global`. A
	// SIBLING of the read-only secrets mount. Re-prepared idempotently on every create/resume.
	gitEnvDir, err := m.gitEnv.Prepare(id, m.agentRootUID())
	if err != nil {
		finalizeAll()
		cleanupSpawnDirs()
		return nil, fmt.Errorf("prepare git-env: %w", err)
	}
	mounts = append(mounts, runtime.Mount{HostPath: gitEnvDir, ContainerPath: GitEnvMountPath})

	cleanupPreStoreFailure := func(h *runtime.PodHandle, floorIP string) {
		cleanupCtx := context.WithoutCancel(ctx)
		if h != nil {
			_ = m.pod.Stop(cleanupCtx, h)
		}
		if floorIP != "" {
			if err := m.fw.Remove(cleanupCtx, firewall.Rules(floorIP, m.cfg.EgressAllowCIDRs)); err != nil {
				log.Printf("egress floor cleanup for %s (ip %s): %v", id, floorIP, err)
			}
		}
		_ = m.finalizeMountDirs(cleanupCtx, mountDirs, mountFinalizers)
		cleanupSpawnDirs()
	}

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

	// GitHub proxy wiring (sp-n7iy.5): the sidecar env the MITM proxy needs, set BEFORE StartPod (container
	// env is static once the container exists). The CA and the token are NOT here — the node PUSHES them
	// into the running sidecar after the readiness probe (sp-2tx8.9 §3.1, see PushCredentials below).
	var (
		sidecarControlEnv  []string
		sidecarControlMnts []runtime.Mount
		// gitProxyEnv holds agent env vars for the MITM proxy + CA + dummy cred wiring (sp-n7iy.5).
		// Non-nil only when ghControl != nil; nil means the spawn has no proxy wiring.
		gitProxyEnv []string
	)
	if m.ghControl != nil {
		// The sidecar binds the MITM proxy here. Constant offset from SidecarPort (inference=+0,
		// control=+1, MITM proxy=+3). (+2 was the node's inbound GetToken listener — deleted, sp-2tx8.9.5.)
		sidecarControlEnv = append(sidecarControlEnv, SidecarProxyAddrEnv+"="+proxyAddr(m.cfg.SidecarPort))
	}

	// Phase 1: sandbox + sidecar (the trusted, key-holding container).
	sidecarEnv := []string{
		"OPENROUTER_API_KEY=" + m.cfg.OpenRouterKey,
		"SIDECAR_ADDR=" + addr,
		SidecarControlTokenEnv + "=" + controlToken,
		"SIDECAR_CONTROL_ADDR=" + controlAddr,
	}
	sidecarEnv = append(sidecarEnv, sidecarControlEnv...)

	// Sidecar upstream CA trust override (sp-wwtc.3): DEV/TEST-ONLY, see ManagerConfig doc. Mount
	// the containing directory (never the file's original host directory verbatim — that may hold
	// unrelated/sensitive siblings) read-only, and point SSL_CERT_FILE at the mounted copy.
	if m.cfg.SidecarCABundleFile != "" {
		bundleName := filepath.Base(m.cfg.SidecarCABundleFile)
		sidecarControlMnts = append(sidecarControlMnts, runtime.Mount{
			HostPath:      filepath.Dir(m.cfg.SidecarCABundleFile),
			ContainerPath: SidecarCABundleMountPath,
			ReadOnly:      true,
		})
		sidecarEnv = append(sidecarEnv, SidecarCABundleFileEnv+"="+SidecarCABundleMountPath+"/"+bundleName)
	}
	h, err := m.pod.StartPod(ctx, runtime.PodSpec{
		ID:            id,
		SidecarImage:  m.cfg.SidecarImage,
		SidecarEnv:    sidecarEnv,
		SidecarMounts: sidecarControlMnts,
		Resources:     res,
		Runtime:       m.cfg.ContainerRuntime,
		Labels:        labels,
	})
	if err != nil {
		cleanupPreStoreFailure(nil, "")
		return nil, err
	}
	// Node-reachable control endpoint (pod IP + control port). Empty PodIP => unreachable URL;
	// the reconciler/node handler treats that as "no live control plane".
	controlURL := ""
	if h.PodIP != "" {
		controlURL = "http://" + net.JoinHostPort(h.PodIP, strconv.Itoa(controlPort)) + "/control/model"
	}

	// Provisioning milestone: setup-network (sp-m859.3). Emitted before the git-proxy render and the
	// egress floor, guarded so it only appears in the subset when network setup is applicable.
	if m.ghControl != nil || m.egressEnforced() {
		sel.progress("setup_network", "configuring network", MilestoneSetupNetwork)
	}

	// sp-n7iy.5: write proxy gitconfig + CA cert/bundle into git-env; build agent proxy env.
	// Runs before StartAgent; the CA comes from the node's persistent CA store.
	// Fail-closed: any error tears the pod down. Non-github spawns skip (gitProxyEnv stays nil).
	if m.ghControl != nil {
		caPEM, caErr := m.ghControl.SpawnCACert(id)
		if caErr != nil {
			cleanupPreStoreFailure(h, "")
			return nil, fmt.Errorf("spawn CA cert for %s: %w", id, caErr)
		}
		if renderErr := renderGitProxy(gitEnvDir, proxyAddr(m.cfg.SidecarPort), caPEM); renderErr != nil {
			cleanupPreStoreFailure(h, "")
			return nil, fmt.Errorf("render git proxy for %s: %w", id, renderErr)
		}
		gitProxyEnv = agentGitProxyEnv(proxyAddr(m.cfg.SidecarPort))
	}

	// Egress floor: applied after the pod IP exists, before the untrusted agent starts (fail-closed).
	var floorIP string
	if m.egressEnforced() {
		if h.PodIP == "" {
			cleanupPreStoreFailure(h, "")
			return nil, fmt.Errorf("egress floor (fail-closed): no pod IP to scope the floor")
		}
		if ferr := m.fw.Apply(ctx, firewall.Rules(h.PodIP, m.cfg.EgressAllowCIDRs)); ferr != nil {
			cleanupPreStoreFailure(h, "")
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
	deltaRef := runtime.DeltaTag(id)
	launchImage := ""
	rootfsArtifacts := cloneRootfsArtifacts(sel.RootfsArtifacts)
	if len(rootfsArtifacts) > 0 {
		var err error
		rootfsArtifacts, err = sortedRootfsArtifactChain(rootfsArtifacts)
		if err != nil {
			cleanupPreStoreFailure(h, floorIP)
			return nil, fmt.Errorf("rootfs artifact restore for %s: %w", id, err)
		}
		if err := validateRootfsArtifactPins(id, sel.RootfsSourceGeneration, baseRef, rootfsArtifacts); err != nil {
			cleanupPreStoreFailure(h, floorIP)
			return nil, err
		}
		if sel.RootfsArtifactsLocalOnly {
			existingImage, eerr := m.pod.EnsureImage(ctx, baseRef, deltaRef)
			if eerr != nil {
				cleanupPreStoreFailure(h, floorIP)
				return nil, fmt.Errorf("ensure launch image: %w", eerr)
			}
			if existingImage != deltaRef {
				cleanupPreStoreFailure(h, floorIP)
				return nil, fmt.Errorf("rootfs local-only start for %s: missing local delta image %s", id, deltaRef)
			}
			launchImage = existingImage
		} else {
			// Provisioning milestone: restore-snapshot (rootfs artifact path, sp-m859.3). Emitted
			// before the artifact restore so a cross-node resume shows the milestone running.
			// Note: this emits AFTER setup-network (catalog-order vs code-order skew, documented).
			sel.progress("restoring_rootfs", "restoring rootfs", MilestoneRestoreSnapshot)
			// Pass sel.ProgressFunc so each artifact emits a resume progress event (sp-u53.7.2):
			// a large delta being fetched+imported can exceed the stall window without resets.
			if err := m.restoreRootfsArtifacts(ctx, id, sel.RootfsSourceGeneration, baseRef, rootfsArtifacts, sel.ProgressFunc); err != nil {
				cleanupPreStoreFailure(h, floorIP)
				return nil, err
			}
			launchImage = deltaRef
		}
	}
	// Emit a resume/milestone progress event before potentially-slow image pull so the CP stall
	// detector does not fire on a cold-image node (sp-u53.7.2 C) and the provisioning step
	// advances. Byte-level intra-pull granularity is a tracked follow-up.
	sel.progress("pulling_image", "ensuring base image is available", MilestonePullImage)
	// Launch image: delta tag if already present locally (same-node resume), else base.
	if launchImage == "" {
		var eerr error
		launchImage, eerr = m.pod.EnsureImage(ctx, baseRef, deltaRef)
		if eerr != nil {
			cleanupPreStoreFailure(h, floorIP)
			return nil, fmt.Errorf("ensure launch image: %w", eerr)
		}
	}
	// Provisioning milestone: start-agent (sp-m859.3). Emitted before BeforeStartAgent so it
	// fires at the start of the agent-start phase. Note: a sidecar-probe failure below
	// attributes to start-agent (catalog-order vs code-order skew, documented).
	sel.progress("starting_agent", "starting agent", MilestoneStartAgent)

	if sel.BeforeStartAgent != nil {
		preAgent := PreAgentContext{
			SpawnID:      id,
			Generation:   generation,
			ControlURL:   controlURL,
			ControlToken: controlToken,
			InjectSecret: func(target string, plaintext []byte) (string, error) {
				return m.secrets.Write(id, target, plaintext)
			},
		}
		if err := sel.BeforeStartAgent(ctx, preAgent); err != nil {
			cleanupPreStoreFailure(h, floorIP)
			return nil, err
		}
	}

	// sp-n7iy.5: sidecar-readiness gate — probe the sidecar's control listener before the agent
	// starts, so the proxy is bound and ready when the first git/gh call lands (§2.6).
	// Gated on ghControl != nil && PodIP known; empty PodIP mirrors controlURL's empty-PodIP guard.
	// Fail-closed: probe failure tears the pod down.
	if m.ghControl != nil && h.PodIP != "" {
		if probeErr := sidecarReadyProbe(ctx, h.PodIP, m.cfg.SidecarPort+1); probeErr != nil {
			cleanupPreStoreFailure(h, floorIP)
			return nil, fmt.Errorf("sidecar readiness gate: %w", probeErr)
		}
	}

	// GitHub credential push (sp-2tx8.9 §3.1): the node DELIVERS the per-spawn MITM CA + a live
	// GitHub token into the ready sidecar BEFORE the untrusted agent starts — the agent must never
	// come up without a working proxy. FAIL-CLOSED: a failed push tears the pod down (this is the
	// successor to the old semantics, where a failed sidecar-side FetchCA made the sidecar os.Exit(1)).
	// A spawn with no GitHub link pushes nothing and returns nil. controlURL != "" mirrors the
	// readiness probe's h.PodIP != "" guard (controlURL is derived from h.PodIP above).
	if m.ghControl != nil && controlURL != "" {
		if perr := m.ghControl.PushCredentials(ctx, id, controlURL, controlToken); perr != nil {
			cleanupPreStoreFailure(h, floorIP)
			return nil, fmt.Errorf("github credential push (fail-closed): %w", perr)
		}
	}

	// Phase 2: the untrusted agent, into the existing pod.
	if err := m.pod.StartAgent(ctx, h, runtime.AgentSpec{
		Image: launchImage,
		Cmd:   agentCmd,
		Env: append([]string{
			"OPENAI_BASE_URL=http://" + addr + "/v1",
			"SPAWN_MODEL=" + model,
			"SPAWN_SESSION_TITLE=" + sessionTitle,
			"GH_CONFIG_DIR=" + SecretsMountPath + "/github/gh",
			// GIT_CONFIG_GLOBAL points at the writable agent-owned git-env dir (sp-7amh §1.1),
			// not the read-only secrets tmpfs. The three hardening vars below keep git non-interactive.
			"GIT_CONFIG_GLOBAL=" + GitEnvMountPath + "/" + GitConfigName,
			"GIT_CONFIG_NOSYSTEM=1",
			"GIT_TERMINAL_PROMPT=0",
			"GIT_ASKPASS=/bin/false",
			// SECRET_WAIT_TIMEOUT: keeps apply-artifacts.sh's --secret-wait-timeout in lockstep with
			// ApplyReportBudget (sp-mwco.2.12 ITEM A) — the shell default (apply-artifacts.sh's own
			// SECRET_WAIT_TIMEOUT="${SECRET_WAIT_TIMEOUT:-30s}") stays as a fallback for a hand-run
			// container, but the node always sets it explicitly so the two constants can't drift.
			"SECRET_WAIT_TIMEOUT=" + SecretWaitTimeout.String(),
		}, gitProxyEnv...),
		Mounts:      mounts,
		Resources:   res,
		Runtime:     m.cfg.ContainerRuntime,
		DropAllCaps: runtime.CapPolicyForUsernsMode(m.cfg.UsernsMode) == runtime.CapDropAll,
		Labels:      labels,
	}); err != nil {
		cleanupPreStoreFailure(h, floorIP)
		return nil, err
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
	if restoredDepth := maxRootfsArtifactSequence(rootfsArtifacts); restoredDepth > deltaDepth {
		deltaDepth = restoredDepth
	}

	sp := &Spawn{
		ID: id, OwnerID: ownerID, Generation: generation, SidecarID: h.SidecarID, AgentID: h.AgentID,
		MountDirs: mountDirs, MountBindings: append([]MountBinding(nil), sel.Mounts...), MountFinalizers: mountFinalizers, JournalMounts: journalMounts, MountTargets: mountTargetsOf(mounts), journalWatchers: watchers,
		FloorIP: floorIP, PodIP: h.PodIP, NetnsPath: h.NetnsPath, SandboxID: h.SandboxID,
		Status: "ready", Mode: sel.Mode, ControlToken: controlToken, ControlURL: controlURL,
		BaseImageDigest: baseDigest,
		LaunchImageRef:  launchImage, // delta tag on same-node resume, base ref on fresh create
		DeltaDepth:      deltaDepth,
		RootfsArtifacts: cloneRootfsArtifacts(rootfsArtifacts),
	}
	if ownerID != "" {
		if !m.store.CommitOwner(reservation, sp) {
			for _, watcher := range watchers {
				watcher.Stop()
			}
			cleanupPreStoreFailure(h, floorIP)
			return nil, fmt.Errorf("spawn %q ownership reservation was lost", id)
		}
	} else {
		m.store.Put(sp)
	}
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

// takeWatchers atomically takes sp.journalWatchers, sets the field to nil, and returns the
// original slice. The caller MUST call w.Stop() on the returned watchers OUTSIDE this call
// (i.e. after releasing the lock) — Stop may block until the watcher goroutine exits, and
// holding watchersMu across a blocking call risks lock-ordering issues or delays.
//
// This is the single safe path for reading-and-clearing sp.journalWatchers: both
// SnapshotForSuspend (store.Get, spawn stays live) and teardown (store.Claim, spawn removed)
// hold the same *Spawn pointer and can race without the lock.
func (m *Manager) takeWatchers(sp *Spawn) []*journal.Watcher {
	m.watchersMu.Lock()
	ws := sp.journalWatchers
	sp.journalWatchers = nil
	m.watchersMu.Unlock()
	return ws
}

// setWatchers atomically assigns ws to sp.journalWatchers. Used by the SnapshotForSuspend
// abort path to restart watchers when the journal snapshot fails and the spawn is kept live.
func (m *Manager) setWatchers(sp *Spawn, ws []*journal.Watcher) {
	m.watchersMu.Lock()
	sp.journalWatchers = ws
	m.watchersMu.Unlock()
}

// snapshotHeartbeatInterval paces the in-progress heartbeat emitted while a single mount's
// FinalSnapshot runs. It must stay well under the CP suspend stall window (defaultSuspendStallWindow,
// 30s) so a slow single-mount snapshot never trips the stall detector between the coarse per-mount
// "snapshot"/"snapshot_done" boundaries.
const snapshotHeartbeatInterval = 10 * time.Second

// finalSnapshotHeartbeat runs FinalSnapshot for one mount while emitting a wall-clock progress
// heartbeat every snapshotHeartbeatInterval. A large single mount (e.g. a github .git with many
// small objects) can take longer than the CP stall window with no per-mount boundary event in
// between; the heartbeat keeps the CP stall timer reset until byte-level Kopia progress exists.
// The heartbeat goroutine is always stopped and drained before returning (no stray events).
func (m *Manager) finalSnapshotHeartbeat(ctx context.Context, id string, gen uint64, mt journal.Mount, progress SuspendProgressFunc) (map[string]journal.ManifestID, error) {
	if progress == nil {
		return m.journal.FinalSnapshot(ctx, id, gen, []journal.Mount{mt})
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(snapshotHeartbeatInterval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				progress("snapshotting", "journaling mount "+mt.Name+" (in progress)", nil)
			}
		}
	}()
	ids, err := m.journal.FinalSnapshot(ctx, id, gen, []journal.Mount{mt})
	close(stop)
	wg.Wait()
	return ids, err
}

// snapshotJournal takes the final per-mount journal snapshot, stringifies the resulting manifest
// ids into a markers map, and persists them to journalState (for same-node resume without CP
// protocol). The caller is responsible for the `m.journal != nil && len(sp.JournalMounts) > 0`
// guard. journal.Close is intentionally NOT called here — it stays in teardown so FinishSuspend
// (snapshot=false) still closes the repo.
//
// progress (optional, nil-safe) fires a "snapshot" event before each mount's FinalSnapshot and a
// "snapshot_done" after, and finalSnapshotHeartbeat emits an in-progress heartbeat WHILE a single
// mount snapshots — so both a many-small-mounts suspend and one slow mount keep the CP stall
// detector's timer reset (sp-u53.7.2 AC: large-but-advancing snapshot must not false-time-out).
// Byte-level intra-mount granularity (a journal.FinalSnapshot progress hook on the Kopia scan path)
// remains a finer follow-up, but the wall-clock heartbeat already covers the stall window.
func (m *Manager) snapshotJournal(ctx context.Context, sp *Spawn, progress SuspendProgressFunc) (map[string]string, error) {
	id := sp.ID
	// Snapshot each journaled mount individually so progress fires per mount. Calling
	// FinalSnapshot with a single-element slice is semantically equivalent to the batch
	// call: each mount has its own serialQueue and the suspend barrier drains per mount.
	allIDs := make(map[string]journal.ManifestID, len(sp.JournalMounts))
	for _, mt := range sp.JournalMounts {
		if progress != nil {
			// Pre-start signal: resets the CP stall timer before the potentially slow FinalSnapshot.
			progress("snapshot", "journaling mount "+mt.Name, nil)
		}
		ids, err := m.finalSnapshotHeartbeat(ctx, id, sp.Generation, mt, progress)
		if err != nil {
			return nil, err
		}
		for k, v := range ids {
			allIDs[k] = v
			log.Printf("journal: spawn=%s gen=%d mount=%s final manifest=%s", id, sp.Generation, k, v)
		}
		// Post-mount signal: carry the completed mount's marker so the CP can accumulate
		// partial markers incrementally (sp-u53.7.2 B). A mid-snapshot stall then persists
		// markers of already-completed mounts rather than losing them all.
		if progress != nil && len(ids) > 0 {
			mountMarkers := make(map[string]string, len(ids))
			for k, v := range ids {
				mountMarkers[string(k)] = string(v)
			}
			progress("snapshot_done", "journaled mount "+mt.Name, mountMarkers)
		}
	}
	markers := make(map[string]string, len(allIDs))
	for mount, mid := range allIDs {
		markers[mount] = string(mid)
	}
	if m.journalState != nil {
		rec := journalRecord{Generation: sp.Generation, Manifests: allIDs}
		if serr := m.journalState.Save(id, rec); serr != nil {
			log.Printf("journal state save for %s: %v", id, serr)
		}
	}
	return markers, nil
}

// restoreRootfsArtifacts fetches and imports each CP-pinned rootfs artifact (delta) into the local
// image store. progress (optional, nil-safe) is called ONCE PER ARTIFACT so the CP resume stall
// detector can reset its timer between artifacts — a single large delta can exceed the stall window
// (sp-u53.7.2). Byte-level intra-artifact progress is a follow-up.
func validateRootfsArtifactPins(id string, sourceGeneration uint64, baseRef string, artifacts []RootfsArtifact) error {
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
	}
	return nil
}

func (m *Manager) rootfsArtifactsForMigrationGeneration(ctx context.Context, sp *Spawn) ([]RootfsArtifact, error) {
	artifacts, err := sortedPortableRootfsArtifacts("rootfs artifact migration", sp.ID, sp.DeltaDepth-1, sp.RootfsArtifacts)
	if err != nil {
		return nil, err
	}
	if len(artifacts) == 0 {
		return nil, nil
	}
	out := make([]RootfsArtifact, 0, len(artifacts))
	for _, art := range artifacts {
		if art.ArtifactID == "" {
			return nil, fmt.Errorf("rootfs artifact migration for %s: inherited artifact has empty artifact id", sp.ID)
		}
		sourceGen := art.Generation
		if sourceGen == 0 || sourceGen == sp.Generation {
			art.Generation = sp.Generation
			out = append(out, art)
			continue
		}
		var payload bytes.Buffer
		desc, err := m.journal.GetArtifact(ctx, sp.ID, sourceGen, art.ArtifactID, &payload)
		if err != nil {
			return nil, fmt.Errorf("rootfs artifact migration for %s: get inherited artifact %s: %w", sp.ID, art.ArtifactID, err)
		}
		desc = forkRootfsCopyDescriptor(desc, art, sp.BaseImageDigest, m.cfg.NodeID, m.rootfsProducerRuntime())
		stored, err := m.journal.PutArtifact(ctx, sp.ID, sp.Generation, desc, bytes.NewReader(payload.Bytes()))
		if err != nil {
			return nil, fmt.Errorf("rootfs artifact migration for %s: put inherited artifact %s: %w", sp.ID, art.ArtifactID, err)
		}
		out = append(out, rootfsArtifactFromJournal(stored))
	}
	return out, nil
}

func (m *Manager) restoreRootfsArtifacts(ctx context.Context, id string, sourceGeneration uint64, baseRef string, artifacts []RootfsArtifact, progress func(phase, detail, stepKey string)) error {
	if m.journal == nil {
		return fmt.Errorf("rootfs artifact restore for %s: no journaler configured", id)
	}
	var err error
	artifacts, err = sortedRootfsArtifactChain(artifacts)
	if err != nil {
		return fmt.Errorf("rootfs artifact restore for %s: %w", id, err)
	}
	if err := validateRootfsArtifactPins(id, sourceGeneration, baseRef, artifacts); err != nil {
		return err
	}
	importBaseRef := baseRef
	for i, art := range artifacts {
		// Emit per-artifact progress so the CP stall timer resets between artifacts.
		if progress != nil {
			progress("restore_rootfs", fmt.Sprintf("restoring rootfs artifact %d/%d (id=%s)", i+1, len(artifacts), art.ArtifactID), MilestoneRestoreSnapshot)
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
		if _, err := m.pod.ImportDelta(ctx, id, importBaseRef, bytes.NewReader(payload.Bytes())); err != nil {
			return fmt.Errorf("rootfs artifact restore for %s: import artifact %s: %w", id, art.ArtifactID, err)
		}
		importBaseRef = runtime.DeltaTag(id)
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

// Ping checks the container runtime is reachable. Used by /readyz.
func (m *Manager) Ping(ctx context.Context) error {
	return m.pod.Ping(ctx)
}

func (m *Manager) Stop(ctx context.Context, id string) error {
	// Claim atomically removes the spawn from the store so a concurrent quota-watchdog
	// Stop or CP-driven Delete cannot race into a double-teardown.
	sp, ok := m.store.Claim(id)
	if !ok {
		return fmt.Errorf("unknown spawn %s", id)
	}
	// snapshot=true: best-effort final journal snapshot, never blocks teardown (fail-closed is suspend-gate-only).
	// progress=nil: Stop is a destroy path — no stall-detector relay.
	_, _ = m.teardown(ctx, sp, false, false, false, true, nil)
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
	// progress=nil: Suspend is the legacy single-step path (no SnapshotForSuspend gate); no caller to relay to.
	// The scrub is a live-container exec (see scrubForCapture) and this path never pauses, so it runs
	// here, before teardown — teardown itself no longer scrubs and no longer unpauses.
	m.scrubForCapture(ctx, sp)
	res, err := m.teardown(ctx, sp, true, false, false, true, nil)
	return res.MountMarkers, err
}

func (m *Manager) SuspendForMigration(ctx context.Context, id string, captureRootfsArtifact bool) (SuspendResult, error) {
	sp, ok := m.store.Claim(id)
	if !ok {
		return SuspendResult{}, fmt.Errorf("unknown spawn %s", id)
	}
	// snapshot=true: best-effort final journal snapshot, never blocks teardown (fail-closed is suspend-gate-only).
	// progress=nil: SuspendForMigration is the migration path; no CP stall-detector relay needed.
	// Non-gate path: the agent is live and never paused, so scrub here (teardown no longer does).
	m.scrubForCapture(ctx, sp)
	return m.teardown(ctx, sp, true, false, captureRootfsArtifact, true, nil)
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
//
// progress (optional, nil-safe) is called at phase boundaries so the caller can relay progress
// signals upstream (sp-u53.7.2). Byte-level intra-snapshot progress is a documented follow-up.
func (m *Manager) SnapshotForSuspend(ctx context.Context, id string, progress SuspendProgressFunc) (SuspendResult, error) {
	sp, ok := m.store.Get(id)
	if !ok {
		return SuspendResult{}, fmt.Errorf("unknown spawn %s", id)
	}
	// Cleanup/abort must run even if the caller's ctx is already cancelled.
	ctx = context.WithoutCancel(ctx)

	// Stop continuous journal watchers so no background RequestSnapshot races the snapshot below.
	// takeWatchers atomically takes the slice under watchersMu — guards against a concurrent
	// Stop/teardown that holds the same *Spawn via store.Claim (data race on journalWatchers,
	// sp-csks). w.Stop is called outside the lock; it may block until the goroutine exits.
	for _, w := range m.takeWatchers(sp) {
		w.Stop()
	}

	h := &runtime.PodHandle{
		PodIP:     sp.PodIP,
		AgentID:   sp.AgentID,
		NetnsPath: sp.NetnsPath,
		SidecarID: sp.SidecarID,
		SandboxID: sp.SandboxID,
	}
	// Layer-hygiene scrub — BEFORE the pause, while the agent is still live and an exec can enter it
	// (spec §4.1). After this point nothing may unpause the agent: everything that lands in the suspend
	// artifact (the journal/mount snapshot below AND the rootfs delta captured by FinishSuspend, across
	// the CP gate) is captured from this one frozen instant.
	if progress != nil {
		progress("gate", "scrubbing delta paths", nil)
	}
	m.scrubForCapture(ctx, sp)

	if progress != nil {
		progress("gate", "pausing agent", nil)
	}
	if perr := m.pod.Pause(ctx, h); perr != nil {
		// Non-fatal (spec §3): snapshot the live tree. Roast-M17 is best-effort.
		log.Printf("suspend gate: pause %s: %v (non-fatal; snapshotting live tree)", id, perr)
	}

	// snapshotJournal emits progress PER MOUNT (not a single batch event here): each mount
	// call resets the CP stall timer so a large-mount suspend never false-times-out (sp-u53.7.2).
	result := SuspendResult{MountMarkers: map[string]string{}}
	if m.journal != nil && len(sp.JournalMounts) > 0 {
		markers, err := m.snapshotJournal(ctx, sp, progress)
		if err != nil {
			// Abort/restore: unpause so the agent can keep running, restart watchers.
			if uerr := m.pod.Unpause(ctx, h); uerr != nil {
				log.Printf("suspend gate abort: unpause %s: %v", id, uerr)
			}
			// setWatchers stores the restarted slice under watchersMu so a concurrent
			// Stop/teardown (which calls takeWatchers) cannot race the assignment.
			m.setWatchers(sp, m.startJournalWatchers(id, sp.Generation, sp.JournalMounts))
			return SuspendResult{}, fmt.Errorf("suspend gate: journal final snapshot for %s: %w", id, err)
		}
		result.MountMarkers = markers
	}
	// Success: agent left paused, watchers stopped, markers persisted by snapshotJournal.
	// Spawn stays in store until FinishSuspend claims and tears it down.
	return result, nil
}

// FinishSuspend completes the suspend teardown started by SnapshotForSuspend (spec §4): it claims the
// spawn from the store, captures the rootfs delta ON THE STILL-PAUSED CONTAINER — the gate's Pause is
// never released, so the rootfs and the journal/mount snapshot come from the same frozen instant — then
// stops the pod, removes the egress floor, finalizes mount dirs, and closes the journal repo. The journal
// snapshot was already taken by SnapshotForSuspend, so FinishSuspend passes snapshot=false to teardown.
//
// The returned SuspendResult carries RootfsArtifacts (when captureRootfsArtifact=true and
// DeltaCapture is enabled). MountMarkers is intentionally empty — the node already holds them
// from the SnapshotForSuspend call and does not need them re-returned here.
//
// progress (optional, nil-safe) is called at phase boundaries inside teardown so the caller
// can relay progress signals upstream (sp-u53.7.2).
func (m *Manager) FinishSuspend(ctx context.Context, id string, captureRootfsArtifact bool, progress SuspendProgressFunc) (SuspendResult, error) {
	sp, ok := m.store.Claim(id)
	if !ok {
		return SuspendResult{}, fmt.Errorf("unknown spawn %s", id)
	}
	// capture=true: rootfs CaptureDelta on the paused container (non-fatal as always).
	// gc=false: delta image preserved for same-node restart-resume.
	// snapshot=false: SnapshotForSuspend already took the final journal snapshot.
	return m.teardown(ctx, sp, true, false, captureRootfsArtifact, false, progress)
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
	// progress=nil: Delete is a destroy path — no stall-detector relay.
	_, _ = m.teardown(ctx, sp, false, true, false, true, nil)
	return nil
}

// teardown is the shared Stop/Suspend/Delete body: stop the pod, remove the egress floor, run the
// journal suspend barrier (final snapshot + node-local pin save), finalize the scratch dirs, and
// drop the spawn from the in-mem store. It returns the per-mount persist markers from the final
// snapshot (empty when journaling is off / the spawn has no journaled mounts) so Suspend can hand
// them to the CP; Stop and Delete discard them.
//
//   - capture=true (Suspend path): trigger the rootfs delta capture BEFORE pod.Stop. On the gate path the
//     container is PAUSED and stays that way (see the capture block); on the legacy Suspend/
//     SuspendForMigration paths it is live. teardown never scrubs and never unpauses.
//   - gc=true (Delete path): release the delta image after pod.Stop and purge durable state files.
//     (Stop and Suspend both have gc=false — the delta image must survive for same-node restart-resume.)
//   - snapshot=true: take a final journal snapshot + persist node-local state (best-effort, non-fatal).
//     false when SnapshotForSuspend (the gate) already did it — FinishSuspend calls with snapshot=false.
//   - progress (optional, nil-safe): called at phase boundaries so callers can relay signals upstream.
func (m *Manager) teardown(ctx context.Context, sp *Spawn, capture, gc, captureRootfsArtifact, snapshot bool, progress SuspendProgressFunc) (SuspendResult, error) {
	id := sp.ID
	result := SuspendResult{MountMarkers: map[string]string{}}
	var resultErr error
	// Teardown must complete even if the caller's ctx is already cancelled (e.g. the CP connection
	// dropped mid-startup and the readiness probe failed): detach so firewall + mount cleanup run.
	ctx = context.WithoutCancel(ctx)
	// Stop the continuous journal watchers FIRST so no background RequestSnapshot
	// races the suspend barrier below (the serial queue would drop a post-suspend
	// request anyway, but stopping here also reclaims the watcher goroutines).
	// takeWatchers atomically takes the slice under watchersMu, guarding against a
	// concurrent SnapshotForSuspend that holds the same *Spawn via store.Get and may
	// be writing (nil-out or restart) sp.journalWatchers (data race, sp-csks).
	for _, w := range m.takeWatchers(sp) {
		w.Stop()
	}

	// Delta capture (spec §2/§4): commit the agent container's writable layer to a local image tag
	// BEFORE pod.Stop (which removes the container). Non-fatal: a capture failure is logged and the
	// teardown continues normally — the next resume falls back to the base image (cold-ish start).
	// Orthogonal to the journal block below (journal handles data mounts; delta handles rootfs).
	if capture && m.cfg.DeltaCapture && sp.AgentID != "" {
		h := &runtime.PodHandle{
			SpawnID:   sp.ID,
			AgentID:   sp.AgentID,
			SidecarID: sp.SidecarID,
			// Use the launch image (delta on resume, base on fresh create) as the layer-count
			// reference for the moby#47065 guard — NOT the original base — so chained captures
			// correctly detect a zero-layer commit on the 2nd+ suspend (spec §3 validation).
			BaseImageRef: sp.LaunchImageRef,
		}
		// NO UNPAUSE HERE — DELIBERATELY (sp-2tx8.2.1, spec §4.1/§4.5). On the gate path the agent is
		// PAUSED (SnapshotForSuspend froze it for the journal/mount snapshot and left it frozen), and the
		// rootfs delta below MUST be captured from that same frozen instant: unpause it and any agent
		// process still running (build, LSP, git, editor autosave) writes into the rootfs but not into the
		// already-taken mount snapshot — a torn artifact. Both lanes capture a paused agent: Docker commits
		// one (Pause:false on an already-frozen container), and containerd's CreateDiff is byte-identical
		// across RUNNING/PAUSED/STOPPED (spike-proven; the CRI lane's own resume happens AFTER its diff).
		//
		// The scrub therefore does NOT live here any more: it is an exec, an exec cannot enter a paused
		// container, and that is exactly how the unpause got here in the first place. It now runs in
		// SnapshotForSuspend before the Pause (gate path) and in Suspend/SuspendForMigration before this
		// call (non-gate paths, never paused). If you need an exec in this path, put it BEFORE the Pause —
		// do NOT reintroduce an Unpause here.
		if progress != nil {
			progress("capture", "committing rootfs delta", nil)
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
					inherited, err := m.rootfsArtifactsForMigrationGeneration(ctx, sp)
					if err != nil {
						resultErr = err
					} else {
						result.RootfsArtifacts = inherited
						if progress != nil {
							progress("export", "exporting rootfs delta artifact", nil)
						}
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
				}
				if resultErr != nil {
					log.Printf("%v", resultErr)
				}
			}
		}
	}

	if progress != nil {
		progress("stop", "stopping pod", nil)
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
			// Pass progress so the stall timer resets per mount (sp-u53.7.2).
			if markers, serr := m.snapshotJournal(ctx, sp, progress); serr != nil {
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

	if progress != nil {
		progress("finalize", "finalizing mount dirs", nil)
	}
	if ferr := m.finalizeMountDirs(ctx, sp.MountDirs, sp.MountFinalizers); ferr != nil {
		log.Printf("mount finalize for %s: %v", id, ferr)
		resultErr = errors.Join(resultErr, fmt.Errorf("finalize mounts for %s: %w", id, ferr))
	}
	// Owner-sealed secret plaintext must not outlive the episode (design §6 never-persist): drop the
	// per-spawn secrets dir. Best-effort — a leftover dir is reseeded empty on the next Create.
	if serr := m.secrets.Remove(id); serr != nil {
		log.Printf("secrets dir cleanup for %s: %v", id, serr)
	}
	if aerr := m.artifacts.Remove(id); aerr != nil {
		log.Printf("artifacts dir cleanup for %s: %v", id, aerr)
	}
	if geerr := m.gitEnv.Remove(id); geerr != nil {
		log.Printf("git-env dir cleanup for %s: %v", id, geerr)
	}
	if gerr := m.githubCreds.Remove(id); gerr != nil {
		log.Printf("github credential cleanup for %s: %v", id, gerr)
	}
	// Control server cleanup (sp-n7iy.3): cancel the spawn's push loop + rejection watch and purge its CA.
	if m.ghControl != nil {
		m.ghControl.Stop(id)
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

func (m *Manager) CleanupSpawnTransient(spawnID string) {
	if err := m.secrets.Remove(spawnID); err != nil {
		log.Printf("secrets dir cleanup for %s: %v", spawnID, err)
	}
	if err := m.artifacts.Remove(spawnID); err != nil {
		log.Printf("artifacts dir cleanup for %s: %v", spawnID, err)
	}
	if err := m.gitEnv.Remove(spawnID); err != nil {
		log.Printf("git-env dir cleanup for %s: %v", spawnID, err)
	}
	if err := m.githubCreds.Remove(spawnID); err != nil {
		log.Printf("github credential cleanup for %s: %v", spawnID, err)
	}
	if m.ghControl != nil {
		m.ghControl.Stop(spawnID)
	}
}

// RenderGitHubAgentCredential renders the agent-facing exact-repo GitHub helper/config into the
// agent-visible secrets tmpfs. The root itself is journal-excluded by construction.
func (m *Manager) RenderGitHubAgentCredential(spawnID string, req githubcred.RenderRequest) (githubcred.Rendered, error) {
	req.RootInsideContainer = SecretsMountPath
	return githubcred.Render(m.secrets.DirFor(spawnID), req)
}

// RenderGitHubIdentity seeds the [user] commit identity into the spawn's agent-owned git-env global
// gitconfig (design §1.2), so agent commits carry the linked GitHub author. Best-effort: the caller
// (mint-at-provision) logs any error and MUST NOT fail provisioning.
func (m *Manager) RenderGitHubIdentity(spawnID, login string, userID int64) error {
	id := githubcred.ResolveIdentity(login, userID)
	return githubcred.RenderIdentity(filepath.Join(m.gitEnv.DirFor(spawnID), GitConfigName), id)
}

func (m *Manager) MountBindings(spawnID string) ([]MountBinding, bool) {
	sp, ok := m.store.Get(spawnID)
	if !ok {
		return nil, false
	}
	return append([]MountBinding(nil), sp.MountBindings...), true
}

// newControlToken returns a 256-bit random hex string used as the sidecar control-endpoint
// bearer token (one per pod). Mirrors the crypto/rand+hex pattern in server.go's newID.
func newControlToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
