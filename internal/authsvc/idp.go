package authsvc

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"

	authv1 "spawnery/gen/auth/v1"
	"spawnery/internal/authsvc/store"
	"spawnery/internal/authsvc/token"
)

// Identity-core named constants (auth-identity design §2/§3).
const (
	accessTokenTTL     = 15 * time.Minute    // §3 access token TTL
	refreshSliding     = 30 * 24 * time.Hour // §3 sliding refresh window
	familyMaxAge       = 90 * 24 * time.Hour // [AM6] absolute family max age
	replayGrace        = 45 * time.Second    // [AM3] idempotent-replay grace window
	popSkew            = 90 * time.Second    // [AM5] PoP timestamp tolerance
	oauthStateTTL      = 10 * time.Minute    // [AM8] authorize->callback state lifetime
	userCodeTTL        = 15 * time.Minute    // [AM7] device-grant user_code lifetime
	devicePollInterval = 5 * time.Second     // RFC 8628 minimum poll interval
	defaultMaxFamilies = 20                  // §3 concurrent-family cap per account
)

// refreshPoPDomain prefixes the bytes a client session key signs to authorize a /refresh
// [AM5]. FROZEN: A3 (spawnctl) and A5 (SPA, WebCrypto) reproduce this byte layout exactly —
// message = domain || sha256(refresh_token_string_bytes) || be64(timestamp) || nonce(16B);
// signature = ECDSA P-256 over SHA-256(message), P1363 raw 64-byte r||s (WebCrypto-native).
const refreshPoPDomain = "spawnery/refresh-pop/v1"

// ErrRefreshRejected indicates GitHub rejected the presented refresh token (already-used / expired /
// revoked). For a single-use rotation this means the chain is provably broken and a relink is
// required. Callers test with errors.Is(err, ErrRefreshRejected).
var ErrRefreshRejected = errors.New("github refresh token rejected")

// IdPConfig wires the identity core into a Service.
type IdPConfig struct {
	Store  store.Store
	GitHub GitHubProvider
	// GitHubRedirectURI is the AS's own /oauth/callback URL as registered at GitHub.
	GitHubRedirectURI string
	// SPAOrigin is the single allowed credentialed-CORS origin [AM2].
	SPAOrigin string
	// RedirectURIs are the exact-match registered client redirect URIs; loopback (http +
	// 127.0.0.1/[::1] + any port + exact path) is the only relaxation (RFC 8252 §7.3).
	RedirectURIs []string
	// VerificationURI is what the device grant tells the user to open (the SPA's confirm page).
	VerificationURI string

	Signer     *token.SigningCredential // current certified session-token + revocation-feed signer
	NextSigner *token.SigningCredential // optional validated one-shot rotation target

	RegistrationEnabled bool // §6 kill switch
	MaxFamilies         int  // 0 => defaultMaxFamilies

	RateLimits RateLimitConfig

	Now func() time.Time
}

// IdP implements the AS identity surface: OAuth code flow, token minting, refresh families,
// device grants, and the revocation feed.
type IdP struct {
	cfg     IdPConfig
	store   store.Store
	github  GitHubProvider
	now     func() time.Time
	signers signerManager

	limits *rateLimiters
}

// NewIdP validates config and builds the identity core.
func NewIdP(cfg IdPConfig) (*IdP, error) {
	if cfg.Store == nil || cfg.GitHub == nil || cfg.Signer == nil {
		return nil, errors.New("authsvc: IdPConfig requires Store, GitHub, Signer")
	}
	if cfg.MaxFamilies <= 0 {
		cfg.MaxFamilies = defaultMaxFamilies
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &IdP{
		cfg:     cfg,
		store:   cfg.Store,
		github:  cfg.GitHub,
		now:     cfg.Now,
		signers: signerManager{current: cfg.Signer, next: cfg.NextSigner},
		limits:  newRateLimiters(cfg.RateLimits),
	}, nil
}

type signerManager struct {
	mu        sync.RWMutex
	current   *token.SigningCredential
	next      *token.SigningCredential
	activated bool
}

func (m *signerManager) withCurrent(sign func(*token.SigningCredential) (string, error)) (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return sign(m.current)
}

func (m *signerManager) sign(artifactType string, payload []byte) (string, error) {
	return m.withCurrent(func(signer *token.SigningCredential) (string, error) {
		return signer.Sign(artifactType, payload)
	})
}

// ActivateNextSigner atomically switches all session and revocation issuance to the configured
// next credential. The transition is one-shot and intentionally has no HTTP exposure.
func (i *IdP) ActivateNextSigner() error {
	i.signers.mu.Lock()
	defer i.signers.mu.Unlock()
	if i.signers.activated {
		return errors.New("authsvc: next signer already activated")
	}
	if i.signers.next == nil {
		return errors.New("authsvc: no next signer configured")
	}
	i.signers.current = i.signers.next
	i.signers.next = nil
	i.signers.activated = true
	return nil
}

// GitHubUser is what the provider's GET /user yields. Sub is the immutable numeric id — the
// subject; Login is display-only [AM9].
type GitHubUser struct {
	Sub   int64
	Login string
}

type GitHubUserToken struct {
	AccessToken          string
	AccessExpiresAtUnix  int64
	RefreshToken         string
	RefreshExpiresAtUnix int64
	TokenType            string
}

// GitHubProvider abstracts the AS<->GitHub confidential-client leg; the production client and
// the dev/test fake share it (same AS code path either way).
type GitHubProvider interface {
	AuthorizeURL(state, challenge, redirectURI string) string
	Exchange(ctx context.Context, code, verifier, redirectURI string) (accessToken string, err error)
	FetchUser(ctx context.Context, accessToken string) (GitHubUser, error)
	RefreshUserAccessToken(ctx context.Context, refreshToken string) (GitHubUserToken, error)
}

// githubClient is the real provider over GitHub's web + API base URLs (overridable so the fake
// plugs in via config).
type githubClient struct {
	webURL, apiURL         string
	clientID, clientSecret string
	httpClient             *http.Client
}

// NewGitHubProvider returns the production GitHub client. webURL hosts /login/oauth/*, apiURL
// hosts /user (github.com / api.github.com in production; the fake's URL in dev/tests).
func NewGitHubProvider(webURL, apiURL, clientID, clientSecret string) GitHubProvider {
	return &githubClient{
		webURL: webURL, apiURL: apiURL,
		clientID: clientID, clientSecret: clientSecret,
		httpClient: &http.Client{Timeout: 15 * time.Second},
	}
}

func (g *githubClient) AuthorizeURL(state, challenge, redirectURI string) string {
	q := url.Values{
		"client_id":             {g.clientID},
		"redirect_uri":          {redirectURI},
		"state":                 {state},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"scope":                 {"read:user"},
	}
	return g.webURL + "/login/oauth/authorize?" + q.Encode()
}

func (g *githubClient) Exchange(ctx context.Context, code, verifier, redirectURI string) (string, error) {
	form := url.Values{
		"client_id":     {g.clientID},
		"client_secret": {g.clientSecret},
		"code":          {code},
		"code_verifier": {verifier},
		"redirect_uri":  {redirectURI},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.webURL+"/login/oauth/access_token",
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := g.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK || out.Error != "" || out.AccessToken == "" {
		return "", fmt.Errorf("github exchange failed: status %d error %q", resp.StatusCode, out.Error)
	}
	return out.AccessToken, nil
}

func (g *githubClient) RefreshUserAccessToken(ctx context.Context, refreshToken string) (GitHubUserToken, error) {
	form := url.Values{
		"client_id":     {g.clientID},
		"client_secret": {g.clientSecret},
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.webURL+"/login/oauth/access_token",
		strings.NewReader(form.Encode()))
	if err != nil {
		return GitHubUserToken{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := g.httpClient.Do(req)
	if err != nil {
		return GitHubUserToken{}, err
	}
	defer resp.Body.Close()
	var out struct {
		AccessToken           string `json:"access_token"`
		ExpiresIn             int64  `json:"expires_in"`
		RefreshToken          string `json:"refresh_token"`
		RefreshTokenExpiresIn int64  `json:"refresh_token_expires_in"`
		TokenType             string `json:"token_type"`
		Error                 string `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return GitHubUserToken{}, err
	}
	if out.Error == "bad_refresh_token" || out.Error == "invalid_grant" {
		return GitHubUserToken{}, fmt.Errorf("github refresh rejected (%s): %w", out.Error, ErrRefreshRejected)
	}
	if resp.StatusCode != http.StatusOK || out.Error != "" || out.AccessToken == "" || out.RefreshToken == "" {
		return GitHubUserToken{}, fmt.Errorf("github refresh failed: status %d error %q", resp.StatusCode, out.Error)
	}
	now := time.Now()
	if out.TokenType == "" {
		out.TokenType = "bearer"
	}
	return GitHubUserToken{
		AccessToken:          out.AccessToken,
		AccessExpiresAtUnix:  now.Add(time.Duration(out.ExpiresIn) * time.Second).Unix(),
		RefreshToken:         out.RefreshToken,
		RefreshExpiresAtUnix: now.Add(time.Duration(out.RefreshTokenExpiresIn) * time.Second).Unix(),
		TokenType:            out.TokenType,
	}, nil
}

func (g *githubClient) FetchUser(ctx context.Context, accessToken string) (GitHubUser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.apiURL+"/user", nil)
	if err != nil {
		return GitHubUser{}, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := g.httpClient.Do(req)
	if err != nil {
		return GitHubUser{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return GitHubUser{}, fmt.Errorf("github /user: status %d", resp.StatusCode)
	}
	var u struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&u); err != nil {
		return GitHubUser{}, err
	}
	if u.ID == 0 {
		return GitHubUser{}, errors.New("github /user: missing numeric id")
	}
	return GitHubUser{Sub: u.ID, Login: u.Login}, nil
}

// --- shared helpers ---

type accessPair struct {
	CPWire, NodeWire       string
	CPTokenID, NodeTokenID string
	IssuedAt, ExpiresAt    int64
}

// mintAccessPair mints the inseparable CP/node access pair under one signer read lock.
func (i *IdP) mintAccessPair(u store.User, familyID string, spkiDER []byte, now time.Time) (accessPair, error) {
	pair := accessPair{
		CPTokenID:   uuid.NewString(),
		NodeTokenID: uuid.NewString(),
		IssuedAt:    now.Unix(),
		ExpiresAt:   now.Add(accessTokenTTL).Unix(),
	}
	sessionKeyHash := token.SessionKeyHash(spkiDER)
	i.signers.mu.RLock()
	defer i.signers.mu.RUnlock()
	signer := i.signers.current
	sign := func(tokenID, audience string) (string, error) {
		body := &authv1.SessionTokenBody{
			AccountId:      u.AccountID,
			Handle:         u.Handle,
			TokenId:        tokenID,
			Audience:       audience,
			IssuedAt:       pair.IssuedAt,
			ExpiresAt:      pair.ExpiresAt,
			SessionKeyHash: sessionKeyHash,
			KeyId:          hex.EncodeToString(signer.KeyID[:]),
			FamilyId:       familyID,
		}
		payload, err := proto.Marshal(body)
		if err != nil {
			return "", err
		}
		return signer.Sign(token.ArtifactTypeSession, payload)
	}
	var err error
	pair.CPWire, err = sign(pair.CPTokenID, token.AudienceCP)
	if err != nil {
		return accessPair{}, err
	}
	pair.NodeWire, err = sign(pair.NodeTokenID, token.AudienceNode)
	if err != nil {
		return accessPair{}, err
	}
	return pair, nil
}

// appendRevocation records a family revocation on the signed feed (logout, theft detection,
// cap eviction, account disable) [AM10]. Call inside the same tx as the revoke.
func appendRevocation(ctx context.Context, tx store.Store, accountID, familyID string, tokenIDs []string, now time.Time) error {
	ids, err := json.Marshal(tokenIDs)
	if err != nil {
		return err
	}
	_, err = tx.Revocations().Append(ctx, store.RevocationEvent{
		AccountID: accountID,
		FamilyID:  familyID,
		TokenIDs:  string(ids),
		RevokedAt: now.Unix(),
	})
	return err
}

// parseSessionSPKI parses and validates a client session pubkey: base64(std or rawurl) of DER
// SPKI, MUST be ECDSA P-256 — the one client key algorithm system-wide [AM11].
func parseSessionSPKI(b64 string) ([]byte, *ecdsa.PublicKey, error) {
	der, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		if der, err = base64.RawURLEncoding.DecodeString(b64); err != nil {
			return nil, nil, errors.New("session_pubkey_spki: bad base64")
		}
	}
	pub, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return nil, nil, errors.New("session_pubkey_spki: bad SPKI")
	}
	ec, ok := pub.(*ecdsa.PublicKey)
	if !ok || ec.Curve != elliptic.P256() {
		return nil, nil, errors.New("session_pubkey_spki: must be ECDSA P-256")
	}
	return der, ec, nil
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// randOpaque returns a fresh 32-byte random opaque value (refresh tokens, codes, flow ids).
func randOpaque() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand failure is unrecoverable
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// writeError emits the AS's structured machine-readable error shape.
func writeError(w http.ResponseWriter, status int, code, desc string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "error_description": desc})
}
