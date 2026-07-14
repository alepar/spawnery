package node

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"log"
	"math/big"
	"sync"
	"time"

	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/spawnlet"
)

// caPair holds the PEM-encoded certificate and private key for a per-spawn CA.
type caPair struct {
	certPEM []byte
	keyPEM  []byte
}

// spawnControlLookup resolves a spawn's sidecar control endpoint (its /control/model URL and the
// SIDECAR_CONTROL_TOKEN bearer). Injected by node.Run over the Manager's spawn store: the async push
// paths (rotation, re-adopt) run outside CreateWithSelection and have no other way to find the pod.
type spawnControlLookup func(spawnID string) (controlURL, controlToken string, ok bool)

// githubControlServer implements spawnlet.GitHubControlServer. It holds the per-spawn ECDSA-P256 CA
// store, PUSHES the CA + GitHub token into the sidecar (sp-2tx8.9 §3.1 — see githubpush.go), and HOLDS a
// rejection long-poll per github spawn (§3.2 — see githubevents.go).
//
// It has NO inbound listener. The node used to SERVE /control/gettoken + /control/spawnca to the pod;
// sp-2tx8.9 inverted the channel (a node can dial into a pod, but cannot bind the pod's IP), and
// sp-2tx8.9.5 deleted the server. Do not re-add one: see internal/node/no_inbound_listener_test.go.
type githubControlServer struct {
	refresher *githubRefresher
	cas       caStore // on-disk CA persistence (sp-2tx8.3.5); zero value = memory-only

	mu    sync.Mutex
	cache map[string]caPair // spawnID -> CA pair (memoizes cas.Load / generateCA)

	// --- push plane (sp-2tx8.9.3) ---
	doer   httpDoer           // POSTs /control/github; set by node.Run
	lookup spawnControlLookup // resolves a spawn's control endpoint; set by node.Run

	reporter     func(spawnID string, st nodev1.GitHubCredentialStatus) // per-CP-connection; nil until installed
	lastStatus   map[string]nodev1.GitHubCredentialStatus               // sticky condition per spawn
	pushedExpiry map[string]int64                                       // expiry of the last SUCCESSFULLY pushed token (unix; 0 = unknown)
	pushes       map[string]*pushHandle                                 // in-flight async push loop per spawn

	// Timing knobs (fields, not package vars, so parallel tests never race). Defaults in the constructor.
	now                func() time.Time
	pushBackoffBase    time.Duration
	pushBackoffMax     time.Duration
	pushFallbackWindow time.Duration

	// --- watch plane (sp-2tx8.9.4) ---
	// eventsDoer holds the long-poll against the sidecar's GET /control/github/events. It is a SEPARATE
	// client from doer: doer has a 5s timeout (controlPostTimeout) and the long-poll blocks ~60s. nil
	// disables rejection detection entirely (the default; node.Run sets it).
	eventsDoer httpDoer
	// baseCtx bounds the watcher goroutines. It is the NODE PROCESS's ctx, not a request's: PushCredentials
	// runs on the create path, whose ctx dies with CreateWithSelection, and the watch must outlive it (and
	// every CP reconnect). Set by node.Run; context.Background() by default.
	baseCtx context.Context

	watches map[string]*watchHandle // in-flight events long-poll per spawn (githubevents.go)

	eventsBackoffBase time.Duration // re-dial backoff after a FAILED poll
	eventsBackoffMax  time.Duration
	eventsPollTimeout time.Duration // our own bound on one long-poll (> the sidecar's 60s)
	rejectCooldown    time.Duration // min interval between two FORCED re-mints for one spawn
}

// Default push timings. The retry loop gives up when the last successfully pushed token expires
// (spec §4: "if still undeliverable when the token expires, report STALE"); when no token was ever
// pushed — or its expiry is unknown — pushFallbackWindow bounds the loop instead.
const (
	defaultPushBackoffBase    = 5 * time.Second
	defaultPushBackoffMax     = 60 * time.Second
	defaultPushFallbackWindow = 15 * time.Minute
)

// Default watch timings (sp-2tx8.9.4). eventsPollTimeout exceeds the sidecar's own ~60s long-poll bound
// (internal/sidecar/githubstate.go: defaultEventsTimeout) so a healthy poll is ended by the SIDECAR's
// 204, not by us. rejectCooldown bounds forced re-mints: GetToken(force=true) bypasses the refresher's
// minMintInterval floor, so an agent hammering a dead token must not become a mint storm against the AS.
const (
	defaultEventsBackoffBase = 5 * time.Second
	defaultEventsBackoffMax  = 60 * time.Second
	defaultEventsPollTimeout = 90 * time.Second
	defaultRejectCooldown    = 60 * time.Second
)

// newGitHubControlServer creates a githubControlServer backed by the given refresher and the given
// on-disk CA store. store's zero value (caStore{}) makes the CA memory-only, as before this bead —
// it will not survive a restart, but nothing errors.
func newGitHubControlServer(r *githubRefresher, store caStore) *githubControlServer {
	return &githubControlServer{
		refresher:          r,
		cas:                store,
		cache:              make(map[string]caPair),
		lastStatus:         make(map[string]nodev1.GitHubCredentialStatus),
		pushedExpiry:       make(map[string]int64),
		pushes:             make(map[string]*pushHandle),
		now:                time.Now,
		pushBackoffBase:    defaultPushBackoffBase,
		pushBackoffMax:     defaultPushBackoffMax,
		pushFallbackWindow: defaultPushFallbackWindow,
		baseCtx:            context.Background(),
		watches:            make(map[string]*watchHandle),
		eventsBackoffBase:  defaultEventsBackoffBase,
		eventsBackoffMax:   defaultEventsBackoffMax,
		eventsPollTimeout:  defaultEventsPollTimeout,
		rejectCooldown:     defaultRejectCooldown,
	}
}

// SpawnCACert returns the PEM-encoded public certificate for the per-spawn CA, generating it
// lazily the first time it is called. The same CA is returned on every call for the same spawnID
// (sidecar app-restart re-delivery semantics, §2.5). Returns an error if key generation fails.
func (s *githubControlServer) SpawnCACert(spawnID string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pair, err := s.caForLocked(spawnID)
	if err != nil {
		return nil, err
	}
	return pair.certPEM, nil
}

// caForLocked returns the CA pair for spawnID: a memory hit first, then a disk load (a restarted
// node reunited with a still-running spawn — logged at Info, this is the whole point of the disk
// store), and only then a fresh generation (logged at Warn — a legacy pod that predates persistence,
// or a spawn whose CA was never minted; see D2 in the bead's design). A fresh generation is
// persisted before it is memoized: an unpersisted CA defeats the point of this store, so a Save
// failure is returned rather than swallowed. Caller must hold s.mu.
func (s *githubControlServer) caForLocked(spawnID string) (caPair, error) {
	if p, ok := s.cache[spawnID]; ok {
		return p, nil
	}
	if p, ok, err := s.cas.Load(spawnID); err != nil {
		return caPair{}, fmt.Errorf("load persisted CA for %s: %w", spawnID, err)
	} else if ok {
		log.Printf("github control: loaded persisted CA for spawn %s", spawnID)
		s.cache[spawnID] = p
		return p, nil
	}
	p, err := generateCA(spawnID)
	if err != nil {
		return caPair{}, fmt.Errorf("generate per-spawn CA for %s: %w", spawnID, err)
	}
	if err := s.cas.Save(spawnID, p); err != nil {
		return caPair{}, fmt.Errorf("persist per-spawn CA for %s: %w", spawnID, err)
	}
	log.Printf("github control: generated a new CA for spawn %s (no persisted CA found)", spawnID)
	s.cache[spawnID] = p
	return p, nil
}

// generateCA creates a new ECDSA-P256 self-signed CA certificate for spawnID.
func generateCA(spawnID string) (caPair, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return caPair{}, fmt.Errorf("generate ECDSA P-256 key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return caPair{}, fmt.Errorf("generate serial: %w", err)
	}

	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName: "spawnery-spawn-CA " + spawnID,
		},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.Add(30 * 24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return caPair{}, fmt.Errorf("create certificate: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return caPair{}, fmt.Errorf("marshal EC private key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	return caPair{certPEM: certPEM, keyPEM: keyPEM}, nil
}

// Stop cancels the spawn's in-flight push loop and rejection watch, and purges its CA (memory + disk) —
// since Stop means the spawn itself is gone (stop/suspend/delete; see the call sites in
// cleanupSpawnDirs/teardown/CleanupSpawnTransient), not merely that this node process is exiting.
// A graceful restart does NOT call Stop (see DetachAll) — that asymmetry is what lets the CA survive
// a spawnlet restart while still dying with the spawn. Also calls Forget on the refresher so the
// proactive-refresh entry is removed (callers that previously called Forget directly may switch to
// Stop-only to avoid the double-Forget; Forget is idempotent), and stops the spawn's
// `/control/github/events` long-poll (sp-2tx8.9.4) — rejection detection dies with the spawn.
func (s *githubControlServer) Stop(spawnID string) {
	s.mu.Lock()
	delete(s.cache, spawnID)
	if h := s.pushes[spawnID]; h != nil {
		h.cancel()
		delete(s.pushes, spawnID)
	}
	if w := s.watches[spawnID]; w != nil {
		w.cancel()
		delete(s.watches, spawnID)
	}
	delete(s.lastStatus, spawnID)
	delete(s.pushedExpiry, spawnID)
	s.mu.Unlock()

	if err := s.cas.Remove(spawnID); err != nil {
		log.Printf("github control: remove persisted CA for spawn %s: %v", spawnID, err)
	}

	if s.refresher != nil {
		s.refresher.Forget(spawnID)
	}
}

// Compile-time assertion: *githubControlServer must implement spawnlet.GitHubControlServer.
var _ spawnlet.GitHubControlServer = (*githubControlServer)(nil)
