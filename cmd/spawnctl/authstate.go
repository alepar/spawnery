package main

// authstate.go — spawnctl auth state: session keypair, token storage, single-flight refresh.
//
// State file: <configDir>/auth.json (0600, atomic rename on write).
// Lock file:  <configDir>/auth.json.lock (advisory flock for cross-process single-flight).
//
// Refresh contract (frozen, must match AS pop.go):
//   message = []byte("spawnery/refresh-pop/v1") || sha256(refresh_token_bytes) || be64(ts) || nonce
//   digest  = sha256(message)
//   sig     = ECDSA P-256, P1363 raw 64-byte r||s

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	mathrand "math/rand"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"google.golang.org/protobuf/proto"

	authv1 "spawnery/gen/auth/v1"
	clientpkg "spawnery/internal/client"
)

const (
	authStateFile    = "auth.json"
	authLockFile     = "auth.json.lock"
	refreshPoPDomain = "spawnery/refresh-pop/v1" // frozen per AS idp.go:47
	// refreshWindow: proactive refresh when this close to expiry (plus jitter).
	refreshWindow = 2 * time.Minute
	// accessTokenTTL mirrors the AS's 15-min TTL; used to set access_expires_at at mint time.
	accessTokenTTLClient = 15 * time.Minute
)

// authState is the JSON-serialised state stored at <configDir>/auth.json.
type authState struct {
	ASURL              string `json:"as_url"`
	AccountID          string `json:"account_id,omitempty"`
	Handle             string `json:"handle,omitempty"`
	CPAccessToken      string `json:"cp_access_token"`
	NodeAccessToken    string `json:"node_access_token"`
	AccessExpiresAt    int64  `json:"access_expires_at"` // unix seconds
	RefreshToken       string `json:"refresh_token"`
	SessionKeyPKCS8PEM string `json:"session_key_pkcs8_pem"`
}

func authStatePath(dir string) string { return filepath.Join(dir, authStateFile) }
func authLockPath(dir string) string  { return filepath.Join(dir, authLockFile) }

// loadState reads auth.json; returns (nil, nil) when the file does not exist.
func loadState(dir string) (*authState, error) {
	b, err := os.ReadFile(authStatePath(dir))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var s authState
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("parse auth state: %w", err)
	}
	return &s, nil
}

// saveState atomically writes auth.json at 0600 via temp+rename.
func saveState(dir string, s *authState) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	tmp := authStatePath(dir) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, authStatePath(dir))
}

// withFileLock acquires the advisory LOCK_EX on the lock file, runs fn, then unlocks.
// Creates the lock file (and the directory) if absent.
func withFileLock(dir string, fn func() error) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("auth lock: mkdir: %w", err)
	}
	f, err := os.OpenFile(authLockPath(dir), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("auth lock: open: %w", err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("auth lock: flock: %w", err)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:errcheck
	return fn()
}

// generateSessionKey generates a fresh ECDSA P-256 session keypair.
func generateSessionKey() (*ecdsa.PrivateKey, error) {
	return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
}

// sessionPubkeySPKIB64 returns the DER SPKI public key encoded as base64 standard (AS accepts both).
func sessionPubkeySPKIB64(key *ecdsa.PrivateKey) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(der), nil
}

// marshalSessionKey serialises an ECDSA P-256 private key to PKCS#8 PEM.
func marshalSessionKey(key *ecdsa.PrivateKey) (string, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), nil
}

// parseSessionKey parses a PKCS#8 PEM-encoded ECDSA P-256 private key.
func parseSessionKey(pemStr string) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, errors.New("authstate: no PEM block in session_key_pkcs8_pem")
	}
	raw, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("authstate: parse PKCS8: %w", err)
	}
	ec, ok := raw.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("authstate: session key is not ECDSA")
	}
	if ec.Curve != elliptic.P256() {
		return nil, errors.New("authstate: session key is not P-256")
	}
	return ec, nil
}

func parseSessionTokenBody(wire string) (*authv1.SessionTokenBody, error) {
	raw, err := base64.RawURLEncoding.DecodeString(wire)
	if err != nil {
		return nil, fmt.Errorf("decode token envelope: %w", err)
	}
	var artifact authv1.SignedAuthArtifact
	if err := proto.Unmarshal(raw, &artifact); err != nil || artifact.GetArtifactType() != "session-token" {
		return nil, errors.New("invalid session-token envelope")
	}
	var body authv1.SessionTokenBody
	if err := proto.Unmarshal(artifact.GetPayload(), &body); err != nil {
		return nil, fmt.Errorf("decode session-token body: %w", err)
	}
	return &body, nil
}

func validateCredentialPair(cpWire, nodeWire string, key *ecdsa.PrivateKey) (int64, error) {
	if cpWire == "" || nodeWire == "" || key == nil {
		return 0, errors.New("credential pair and session key are required")
	}
	cpBody, err := parseSessionTokenBody(cpWire)
	if err != nil {
		return 0, fmt.Errorf("cp access token: %w", err)
	}
	nodeBody, err := parseSessionTokenBody(nodeWire)
	if err != nil {
		return 0, fmt.Errorf("node access token: %w", err)
	}
	spki, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return 0, fmt.Errorf("marshal session key: %w", err)
	}
	keyHash := sha256.Sum256(spki)
	if cpBody.GetAudience() != "cp" || nodeBody.GetAudience() != "node" ||
		cpBody.GetAccountId() == "" || cpBody.GetFamilyId() == "" ||
		cpBody.GetTokenId() == "" || nodeBody.GetTokenId() == "" || cpBody.GetTokenId() == nodeBody.GetTokenId() ||
		cpBody.GetAccountId() != nodeBody.GetAccountId() || cpBody.GetFamilyId() != nodeBody.GetFamilyId() ||
		cpBody.GetKeyId() != nodeBody.GetKeyId() || cpBody.GetIssuedAt() != nodeBody.GetIssuedAt() ||
		cpBody.GetExpiresAt() != nodeBody.GetExpiresAt() || cpBody.GetExpiresAt() <= cpBody.GetIssuedAt() ||
		!bytes.Equal(cpBody.GetSessionKeyHash(), nodeBody.GetSessionKeyHash()) ||
		!bytes.Equal(cpBody.GetSessionKeyHash(), keyHash[:]) {
		return 0, errors.New("access token pair does not describe one key-bound login session")
	}
	return cpBody.GetExpiresAt(), nil
}

// signRefreshPoP builds and signs the PoP proof for /refresh per the frozen wire contract.
// Returns (X-PoP-Timestamp, X-PoP-Nonce, X-PoP-Sig) header values.
func signRefreshPoP(rawRefresh string, key *ecdsa.PrivateKey) (tsStr, nonceB64, sigB64 string, err error) {
	ts := time.Now().Unix()
	nonce := make([]byte, 16)
	if _, err = rand.Read(nonce); err != nil {
		return "", "", "", fmt.Errorf("signRefreshPoP: random nonce: %w", err)
	}

	tokenHash := sha256.Sum256([]byte(rawRefresh))
	var tsBytes [8]byte
	binary.BigEndian.PutUint64(tsBytes[:], uint64(ts))

	domain := []byte(refreshPoPDomain)
	msg := make([]byte, 0, len(domain)+32+8+len(nonce))
	msg = append(msg, domain...)
	msg = append(msg, tokenHash[:]...)
	msg = append(msg, tsBytes[:]...)
	msg = append(msg, nonce...)
	digest := sha256.Sum256(msg)

	r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		return "", "", "", fmt.Errorf("signRefreshPoP: ecdsa.Sign: %w", err)
	}
	// P1363: left-pad r and s to 32 bytes each, concatenate.
	sig := make([]byte, 64)
	rB, sB := r.Bytes(), s.Bytes()
	copy(sig[32-len(rB):32], rB)
	copy(sig[64-len(sB):64], sB)

	enc := base64.RawURLEncoding.EncodeToString
	return fmt.Sprintf("%d", ts), enc(nonce), enc(sig), nil
}

// doRefresh calls /refresh with the stored token + PoP proof, rotates state.
// Must be called with the advisory file lock held (caller's responsibility).
// On token_revoked/401, wipes state and returns an error prompting re-login.
func doRefresh(ctx context.Context, dir string, s *authState, httpClient *http.Client) error {
	key, err := parseSessionKey(s.SessionKeyPKCS8PEM)
	if err != nil {
		return invalidateLostAuthState(ctx, dir, s, httpClient, err)
	}

	tsStr, nonceB64, sigB64, err := signRefreshPoP(s.RefreshToken, key)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.ASURL+"/refresh", nil)
	if err != nil {
		return fmt.Errorf("doRefresh: build request: %w", err)
	}
	req.AddCookie(&http.Cookie{Name: "refresh_token", Value: s.RefreshToken})
	req.Header.Set("X-PoP-Timestamp", tsStr)
	req.Header.Set("X-PoP-Nonce", nonceB64)
	req.Header.Set("X-PoP-Sig", sigB64)

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("doRefresh: request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		var body struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&body)
		// Wipe local state — token is revoked or expired on the AS.
		_ = os.Remove(authStatePath(dir))
		return fmt.Errorf("session expired (%s): please run 'spawnctl login' again", body.Error)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("doRefresh: unexpected status %d", resp.StatusCode)
	}

	var body struct {
		CPAccessToken   string `json:"cp_access_token"`
		NodeAccessToken string `json:"node_access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return fmt.Errorf("doRefresh: decode response: %w", err)
	}
	if body.CPAccessToken == "" || body.NodeAccessToken == "" {
		return fmt.Errorf("doRefresh: incomplete access token pair in response")
	}

	// New refresh token arrives in Set-Cookie (the AS rotates it).
	newRefresh := ""
	for _, c := range resp.Cookies() {
		if c.Name == "refresh_token" && c.Value != "" {
			newRefresh = c.Value
		}
	}
	if newRefresh == "" {
		return errors.New("doRefresh: missing rotated refresh token")
	}
	expiresAt, err := validateCredentialPair(body.CPAccessToken, body.NodeAccessToken, key)
	if err != nil {
		return fmt.Errorf("doRefresh: %w", err)
	}
	next := *s
	next.CPAccessToken = body.CPAccessToken
	next.NodeAccessToken = body.NodeAccessToken
	next.RefreshToken = newRefresh
	next.AccessExpiresAt = expiresAt

	if err := saveState(dir, &next); err != nil {
		return err
	}
	*s = next
	return nil
}

// cpTokenSource provides the bearer token for CP calls with transparent refresh.
// Precedence (highest first):
//  1. staticToken non-empty → return it as-is (dev-token / env / -token flag)
//  2. auth.json present → AS-backed with single-flight refresh
//  3. (callers fall back to "dev-token" before constructing this; see buildTokenSource)
type cpTokenSource struct {
	dir         string
	httpClient  *http.Client
	staticToken string // non-empty ⇒ skip AS, return as-is
	mu          sync.Mutex
}

// Token returns a valid bearer token, proactively refreshing if within the refresh window.
func (ts *cpTokenSource) Token(ctx context.Context) (string, error) {
	if ts.staticToken != "" {
		return ts.staticToken, nil
	}
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.tokenLocked(ctx)
}

// tokenLocked is called with ts.mu held. It acquires the file lock and refreshes if needed.
func (ts *cpTokenSource) tokenLocked(ctx context.Context) (string, error) {
	var result string
	err := withFileLock(ts.dir, func() error {
		s, err := loadState(ts.dir)
		if err != nil {
			return err
		}
		if s == nil || s.CPAccessToken == "" {
			return fmt.Errorf("not logged in — run 'spawnctl login' first")
		}

		// Proactive refresh: threshold = expiry minus refreshWindow minus random jitter (0–30s).
		jitterSec := mathrand.Int63n(30)
		threshold := s.AccessExpiresAt - int64(refreshWindow.Seconds()) - jitterSec
		if time.Now().Unix() >= threshold {
			if err := doRefresh(ctx, ts.dir, s, ts.httpClient); err != nil {
				// Refresh failed — still return the (possibly expired) token and let the server
				// produce a 401 so the caller can decide (transparent retry via OnUnauthenticated).
				result = s.CPAccessToken
				return nil //nolint:nilerr
			}
		}
		result = s.CPAccessToken
		return nil
	})
	return result, err
}

// OnUnauthenticated forces an immediate refresh after receiving a 401/CodeUnauthenticated.
func (ts *cpTokenSource) OnUnauthenticated(ctx context.Context) error {
	if ts.staticToken != "" {
		return nil // static tokens cannot be refreshed
	}
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return withFileLock(ts.dir, func() error {
		s, err := loadState(ts.dir)
		if err != nil {
			return err
		}
		if s == nil {
			return fmt.Errorf("not logged in")
		}
		return doRefresh(ctx, ts.dir, s, ts.httpClient)
	})
}

// NodeCredentials returns the current aud=node token and the persistent login signer. Static CP
// tokens intentionally cannot authorize a node operation.
func (ts *cpTokenSource) NodeCredentials(ctx context.Context) (clientpkg.NodeCredentials, error) {
	if ts.staticToken != "" {
		return clientpkg.NodeCredentials{}, errors.New("node authorization requires 'spawnctl login'")
	}
	ts.mu.Lock()
	defer ts.mu.Unlock()
	var result clientpkg.NodeCredentials
	err := withFileLock(ts.dir, func() error {
		s, err := loadState(ts.dir)
		if err != nil || s == nil {
			return errors.New("not logged in - run 'spawnctl login' first")
		}
		key, err := parseSessionKey(s.SessionKeyPKCS8PEM)
		if err != nil {
			return ts.invalidateLostKey(ctx, s, err)
		}
		if _, err := validateCredentialPair(s.CPAccessToken, s.NodeAccessToken, key); err != nil {
			return ts.invalidateLostKey(ctx, s, err)
		}
		signer, err := clientpkg.NewECDSASessionSigner(key)
		if err != nil {
			return ts.invalidateLostKey(ctx, s, err)
		}
		if _, err := signer.SignP1363("spawnery/session-key-check/v1", nil); err != nil {
			return ts.invalidateLostKey(ctx, s, err)
		}
		result = clientpkg.NodeCredentials{AccessToken: s.NodeAccessToken, Signer: signer}
		return nil
	})
	return result, err
}

func (ts *cpTokenSource) invalidateLostKey(ctx context.Context, s *authState, cause error) error {
	return invalidateLostAuthState(ctx, ts.dir, s, ts.httpClient, cause)
}

func invalidateLostAuthState(ctx context.Context, dir string, s *authState, httpClient *http.Client, cause error) error {
	client := httpClient
	if client == nil {
		client = http.DefaultClient
	}
	if s.ASURL != "" && s.RefreshToken != "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.ASURL+"/logout", nil)
		if err == nil {
			req.AddCookie(&http.Cookie{Name: "logout_session", Value: s.RefreshToken})
			if resp, err := client.Do(req); err == nil {
				resp.Body.Close()
			}
		}
	}
	if err := os.Remove(authStatePath(dir)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("session key unusable (%v), and clearing auth state: %w", cause, err)
	}
	return fmt.Errorf("session key unusable: %v; please run 'spawnctl login' again", cause)
}

// buildTokenSource constructs a cpTokenSource from flag/env/state with the defined precedence.
// tokenFlag is the value of the -token flag; envToken is from SPAWNERY_TOKEN/CP_DEV_TOKEN.
// When neither is explicitly set (flag at default "dev-token", no env), it falls back to
// the auth.json state; if no state either, it uses the default "dev-token" for dev mode.
func buildTokenSource(dir, tokenFlag string, httpClient *http.Client) *cpTokenSource {
	// 1. Environment override.
	envToken := os.Getenv("SPAWNERY_TOKEN")
	if envToken == "" {
		envToken = os.Getenv("CP_DEV_TOKEN")
	}
	if envToken != "" {
		return &cpTokenSource{staticToken: envToken, httpClient: httpClient}
	}

	// 2. Explicit -token flag (non-default).
	if tokenFlag != "" && tokenFlag != "dev-token" {
		return &cpTokenSource{staticToken: tokenFlag, httpClient: httpClient}
	}

	// 3. Auth state (auth.json exists and has tokens).
	s, err := loadState(dir)
	if err == nil && s != nil && s.CPAccessToken != "" && s.NodeAccessToken != "" {
		return &cpTokenSource{dir: dir, httpClient: httpClient}
	}

	// 4. Default dev-token fallback.
	return &cpTokenSource{staticToken: "dev-token", httpClient: httpClient}
}
