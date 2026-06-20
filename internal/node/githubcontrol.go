package node

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/subtle"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	sidecarv1 "spawnery/gen/sidecar/v1"
	"spawnery/internal/spawnlet"
)

// caPair holds the PEM-encoded certificate and private key for a per-spawn CA.
type caPair struct {
	certPEM []byte
	keyPEM  []byte
}

// githubControlServer implements spawnlet.GitHubControlServer. It holds the per-spawn ECDSA-P256
// CA store and drives the lane-aware HTTP control server (UDS or TCP) for GetToken + GetSpawnCA.
type githubControlServer struct {
	refresher *githubRefresher

	mu        sync.Mutex
	cas       map[string]caPair      // spawnID -> CA pair
	listeners map[string]net.Listener // spawnID -> active listener
}

// newGitHubControlServer creates a githubControlServer backed by the given refresher.
func newGitHubControlServer(r *githubRefresher) *githubControlServer {
	return &githubControlServer{
		refresher: r,
		cas:       make(map[string]caPair),
		listeners: make(map[string]net.Listener),
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

// caForLocked returns (or generates) the CA pair for spawnID. Caller must hold s.mu.
func (s *githubControlServer) caForLocked(spawnID string) (caPair, error) {
	if p, ok := s.cas[spawnID]; ok {
		return p, nil
	}
	p, err := generateCA(spawnID)
	if err != nil {
		return caPair{}, fmt.Errorf("generate per-spawn CA for %s: %w", spawnID, err)
	}
	s.cas[spawnID] = p
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

// Serve binds the control HTTP server on the transport described by t. For "unix" it creates
// the socket and chmods it 0666; for "tcp" it wraps each handler with bearer + source-IP auth.
// If a prior listener exists for t.SpawnID it is closed first (idempotent re-Serve). A Serve
// error tears down the listener before returning so the caller can fail-close the pod.
func (s *githubControlServer) Serve(t spawnlet.ControlTransport) error {
	mux := http.NewServeMux()

	if t.Network == "tcp" {
		// TCP lane: wrap each handler with auth.
		mux.Handle("/control/gettoken", s.tcpAuthMiddleware(t, http.HandlerFunc(s.handleGetToken)))
		mux.Handle("/control/spawnca", s.tcpAuthMiddleware(t, http.HandlerFunc(s.handleGetSpawnCA)))
	} else {
		// UDS lane: filesystem scope is the auth boundary.
		mux.HandleFunc("/control/gettoken", s.handleGetToken)
		mux.HandleFunc("/control/spawnca", s.handleGetSpawnCA)
	}

	ln, err := net.Listen(t.Network, t.Address)
	if err != nil {
		return fmt.Errorf("github control server %s %s: listen: %w", t.Network, t.Address, err)
	}

	if t.Network == "unix" {
		// Make the socket world-connectable so the userns-remapped sidecar (different uid) can
		// connect. The parent directory is 0711 (node uid), so only the sidecar-on-the-mount-point
		// and the node process itself can reach the socket.
		if err := os.Chmod(t.Address, 0o666); err != nil {
			_ = ln.Close()
			return fmt.Errorf("github control server chmod socket %s: %w", t.Address, err)
		}
	}

	// Register the listener before starting the goroutine so Stop can find and close it.
	s.mu.Lock()
	// Close any prior listener for this spawn (idempotent re-Serve).
	if prev := s.listeners[t.SpawnID]; prev != nil {
		_ = prev.Close()
	}
	s.listeners[t.SpawnID] = ln
	s.mu.Unlock()

	srv := &http.Server{Handler: mux}
	go func() {
		if err := srv.Serve(ln); err != nil && !isListenerClosed(err) {
			log.Printf("github control server for spawn %s: %v", t.SpawnID, err)
		}
	}()
	return nil
}

// Stop closes the spawn's listener and purges its CA. Also calls Forget on the refresher so the
// proactive-refresh entry is removed (callers that previously called Forget directly may switch to
// Stop-only to avoid the double-Forget; Forget is idempotent).
func (s *githubControlServer) Stop(spawnID string) {
	s.mu.Lock()
	if ln := s.listeners[spawnID]; ln != nil {
		_ = ln.Close()
		delete(s.listeners, spawnID)
	}
	delete(s.cas, spawnID)
	s.mu.Unlock()

	if s.refresher != nil {
		s.refresher.Forget(spawnID)
	}
}

// tcpAuthMiddleware wraps h with bearer-token + source-pod-IP verification for the TCP lane.
func (s *githubControlServer) tcpAuthMiddleware(t spawnlet.ControlTransport, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Bearer token check (constant-time to resist timing attacks).
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), []byte(t.Bearer)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		// Source pod IP check.
		sourceHost, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil || sourceHost != t.PodIP {
			http.Error(w, "forbidden: unexpected source IP", http.StatusForbidden)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// handleGetToken decodes a GetTokenRequest, calls GetToken on the refresher, maps typed errors
// to HTTP status codes, and writes a protojson GetTokenResponse.
func (s *githubControlServer) handleGetToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}

	var req sidecarv1.GetTokenRequest
	if err := protojson.Unmarshal(body, &req); err != nil {
		http.Error(w, "decode request: "+err.Error(), http.StatusBadRequest)
		return
	}

	tok, exp, err := s.refresher.GetToken(r.Context(), req.GetSpawnId(), req.GetMinRemainingSeconds())
	if err != nil {
		status, msg := statusForGetToken(err)
		http.Error(w, msg, status)
		return
	}

	resp := &sidecarv1.GetTokenResponse{
		Token:               tok,
		AccessExpiresAtUnix: exp,
	}
	out, merr := protojson.Marshal(resp)
	if merr != nil {
		http.Error(w, "marshal response: "+merr.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

// handleGetSpawnCA decodes a GetSpawnCARequest and returns a protojson SpawnCADelivery with both
// the public cert and the private key (the key is needed by the sidecar for JIT leaf-cert signing).
func (s *githubControlServer) handleGetSpawnCA(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}

	var req sidecarv1.GetSpawnCARequest
	if err := protojson.Unmarshal(body, &req); err != nil {
		http.Error(w, "decode request: "+err.Error(), http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	pair, err := s.caForLocked(req.GetSpawnId())
	s.mu.Unlock()
	if err != nil {
		http.Error(w, "CA generation failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	resp := &sidecarv1.SpawnCADelivery{
		CaCertPem: pair.certPEM,
		CaKeyPem:  pair.keyPEM,
	}
	out, merr := protojson.Marshal(resp)
	if merr != nil {
		http.Error(w, "marshal response: "+merr.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

// statusForGetToken maps typed GetToken errors to HTTP status codes and short diagnostic bodies.
// Returns a distinct, non-retrying status for each typed failure so the sidecar proxy (and by
// extension the agent's git/gh) fails fast with a comprehensible error rather than looping.
func statusForGetToken(err error) (int, string) {
	switch {
	case isNotLinkedOrNotFound(err):
		return http.StatusForbidden, "no github link for this spawn (not linked or link not yet delivered)"
	case isMintRateLimited(err):
		return http.StatusTooManyRequests, "github token mint rate-limited; try again shortly"
	default:
		return http.StatusBadGateway, "upstream github auth service error: " + err.Error()
	}
}

func isNotLinkedOrNotFound(err error) bool {
	return errors.Is(err, ErrGitHubNotLinked) || errors.Is(err, ErrGitHubRelinkRequired)
}

func isMintRateLimited(err error) bool {
	return errors.Is(err, ErrGitHubMintRateLimited)
}

// isListenerClosed reports whether err is the expected "use of closed network connection" error
// returned when we close the listener from Stop (not a real server error).
func isListenerClosed(err error) bool {
	return err != nil && strings.Contains(err.Error(), "use of closed network connection")
}

// Compile-time assertion: *githubControlServer must implement spawnlet.GitHubControlServer.
var _ spawnlet.GitHubControlServer = (*githubControlServer)(nil)
