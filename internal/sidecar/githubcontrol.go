package sidecar

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	sidecarv1 "spawnery/gen/sidecar/v1"
)

// minRemainingSeconds is the buffer before expiry at which the proxy refreshes the token.
// A small buffer (5 min) rather than "near full headroom" prevents a near-per-request refresh
// while still guaranteeing the token cannot expire mid-stream for normal operations (§2.4 roast r4).
const minRemainingSeconds = 300

// ControlConfig holds the parameters needed to reach the node's credential server.
// Either UDS (network="unix", address=socket path) or TCP (network="tcp", address=host:port,
// optional bearer for TCP-lane auth).
type ControlConfig struct {
	Network string // "unix" or "tcp"
	Address string // socket path (unix) or host:port (tcp)
	Bearer  string // per-spawn bearer (TCP lane only; empty for UDS)
	SpawnID string // used in GetTokenRequest / GetSpawnCARequest
}

// ControlTokenError is a typed, non-retrying error returned when the node refuses a GetToken
// request with a permanent failure code (403/429/5xx). The proxy forwards a short diagnostic
// body to the agent's git/gh so it fails fast rather than looping.
type ControlTokenError struct {
	StatusCode int
	Body       string
}

func (e *ControlTokenError) Error() string {
	return fmt.Sprintf("GetToken: HTTP %d: %s", e.StatusCode, e.Body)
}

// githubControl is the sidecar-side client for the node's credential control server.
// It speaks protojson over HTTP (POST /control/gettoken, /control/spawnca).
type githubControl struct {
	cfg    ControlConfig
	client *http.Client

	nowFn func() time.Time // injectable for tests; defaults to time.Now

	mu           sync.Mutex
	cachedToken  string
	cachedExpiry int64 // Unix timestamp
}

// newGitHubControl constructs a control client. The HTTP transport is dialed over the network
// and address in cfg: "unix" dials a UDS socket; "tcp" dials a plain TCP connection and adds
// a Bearer header when cfg.Bearer is set.
func newGitHubControl(cfg ControlConfig) *githubControl {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, cfg.Network, cfg.Address)
		},
	}
	return &githubControl{
		cfg:   cfg,
		client: &http.Client{Transport: transport},
		nowFn: time.Now,
	}
}

// Token returns a cached GitHub access token if it has at least minRemainingSeconds of life left,
// or fetches a fresh one otherwise.
func (c *githubControl) Token(ctx context.Context) (string, error) {
	c.mu.Lock()
	now := c.nowFn()
	if c.cachedToken != "" && c.cachedExpiry-now.Unix() >= minRemainingSeconds {
		tok := c.cachedToken
		c.mu.Unlock()
		return tok, nil
	}
	c.mu.Unlock()

	// Fetch outside the lock so concurrent callers in different goroutines don't all block on
	// the same slow mint; only the first one's result wins (re-check under lock on return).
	tok, exp, err := c.FetchToken(ctx)
	if err != nil {
		return "", err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.cachedToken = tok
	c.cachedExpiry = exp
	return tok, nil
}

// FetchToken fetches a fresh token from the node, bypassing the cache. It always sends
// MinRemainingSeconds=minRemainingSeconds so the node mints a fresh token when needed.
func (c *githubControl) FetchToken(ctx context.Context) (token string, expiresAtUnix int64, _ error) {
	req := &sidecarv1.GetTokenRequest{
		SpawnId:             c.cfg.SpawnID,
		MinRemainingSeconds: minRemainingSeconds,
	}
	body, err := protojson.Marshal(req)
	if err != nil {
		return "", 0, fmt.Errorf("FetchToken: marshal request: %w", err)
	}

	resp, err := c.post(ctx, "/control/gettoken", body)
	if err != nil {
		return "", 0, err
	}

	var result sidecarv1.GetTokenResponse
	if err := protojson.Unmarshal(resp, &result); err != nil {
		return "", 0, fmt.Errorf("FetchToken: unmarshal response: %w", err)
	}
	return result.GetToken(), result.GetAccessExpiresAtUnix(), nil
}

// FetchCA fetches the per-spawn CA cert and private key from the node.
func (c *githubControl) FetchCA(ctx context.Context) (*sidecarv1.SpawnCADelivery, error) {
	req := &sidecarv1.GetSpawnCARequest{SpawnId: c.cfg.SpawnID}
	body, err := protojson.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("FetchCA: marshal request: %w", err)
	}

	resp, err := c.post(ctx, "/control/spawnca", body)
	if err != nil {
		return nil, err
	}

	var result sidecarv1.SpawnCADelivery
	if err := protojson.Unmarshal(resp, &result); err != nil {
		return nil, fmt.Errorf("FetchCA: unmarshal response: %w", err)
	}
	return &result, nil
}

// post sends a POST request to the control server at path with body, and returns the response
// body. On non-2xx it reads the response body and returns a *ControlTokenError.
func (c *githubControl) post(ctx context.Context, path string, body []byte) ([]byte, error) {
	// The URL host is ignored by the custom dialer; we use a placeholder.
	url := "http://control" + path
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("control POST %s: new request: %w", path, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.cfg.Network == "tcp" && c.cfg.Bearer != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.cfg.Bearer)
	}

	resp, err := c.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("control POST %s: %w", path, err)
	}
	defer resp.Body.Close() //nolint:errcheck

	respBody := make([]byte, 0, 256)
	buf := make([]byte, 512)
	total := 0
	for total < 4096 { // cap: short diagnostic bodies only
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			respBody = append(respBody, buf[:n]...)
			total += n
		}
		if readErr != nil {
			break
		}
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &ControlTokenError{
			StatusCode: resp.StatusCode,
			Body:       string(respBody),
		}
	}
	return respBody, nil
}
