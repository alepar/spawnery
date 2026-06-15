package subkey

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"spawnery/internal/pki"
)

const maxNodeRevocationResponseBytes = 1 << 20

type ASRevocationChecker struct {
	url    string
	client *http.Client
	ttl    time.Duration
	now    func() time.Time

	mu      sync.Mutex
	expires time.Time
	revoked map[string]struct{}
}

func NewASRevocationChecker(url string, client *http.Client, ttl time.Duration) *ASRevocationChecker {
	if client == nil {
		client = http.DefaultClient
	}
	return &ASRevocationChecker{
		url:    url,
		client: client,
		ttl:    ttl,
		now:    time.Now,
	}
}

func (c *ASRevocationChecker) IsRevoked(id pki.Identity) (bool, error) {
	if strings.TrimSpace(c.url) == "" {
		return false, errors.New("subkey: AS node revocation URL is empty")
	}

	c.mu.Lock()
	if c.ttl > 0 && c.revoked != nil && c.now().Before(c.expires) {
		_, ok := c.revoked[id.NodeID]
		c.mu.Unlock()
		return ok, nil
	}
	c.mu.Unlock()

	revoked, err := c.fetch()
	if err != nil {
		return false, err
	}

	c.mu.Lock()
	if c.ttl > 0 {
		c.revoked = revoked
		c.expires = c.now().Add(c.ttl)
	}
	_, ok := revoked[id.NodeID]
	c.mu.Unlock()
	return ok, nil
}

func (c *ASRevocationChecker) fetch() (map[string]struct{}, error) {
	req, err := http.NewRequest(http.MethodGet, c.url, nil)
	if err != nil {
		return nil, fmt.Errorf("subkey: build node revocation request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("subkey: fetch node revocations: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("subkey: fetch node revocations: AS returned %d", resp.StatusCode)
	}

	var body struct {
		RevokedNodeIDs *[]string `json:"revoked_node_ids"`
	}
	dec := json.NewDecoder(io.LimitReader(resp.Body, maxNodeRevocationResponseBytes))
	if err := dec.Decode(&body); err != nil {
		return nil, fmt.Errorf("subkey: malformed node revocation response: %w", err)
	}
	if body.RevokedNodeIDs == nil {
		return nil, errors.New("subkey: malformed node revocation response: revoked_node_ids missing or null")
	}

	out := make(map[string]struct{}, len(*body.RevokedNodeIDs))
	for _, nodeID := range *body.RevokedNodeIDs {
		out[nodeID] = struct{}{}
	}
	return out, nil
}
