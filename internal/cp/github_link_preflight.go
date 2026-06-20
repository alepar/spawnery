package cp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"spawnery/internal/cp/store"
)

// gitHubLinkStatus is the result returned by the AS /internal/github/link-status endpoint.
type gitHubLinkStatus int

const (
	// gitHubLinkStatusActive means the account has a live, non-expired GitHub link.
	gitHubLinkStatusActive gitHubLinkStatus = iota
	// gitHubLinkStatusRelinkRequired means the link exists but the refresh chain is broken.
	gitHubLinkStatusRelinkRequired
	// gitHubLinkStatusNone means the account has no GitHub link.
	gitHubLinkStatusNone
)

// asLinkChecker verifies whether a CP owner account has a live GitHub link on the AS.
// A nil asLinkChecker means no checker is configured and the preflight is skipped (back-compat
// for non-github lanes and hermetic tests without an AS).
type asLinkChecker interface {
	CheckLinkStatus(ctx context.Context, accountID string) (gitHubLinkStatus, error)
}

// httpASLinkChecker is the production implementation: POSTs to the AS link-status endpoint.
type httpASLinkChecker struct {
	asURL  string
	secret string
	client *http.Client
}

type asLinkStatusReq struct {
	AccountID string `json:"account_id"`
}

type asLinkStatusResp struct {
	Status string `json:"status"`
}

func (c *httpASLinkChecker) CheckLinkStatus(ctx context.Context, accountID string) (gitHubLinkStatus, error) {
	body, err := json.Marshal(asLinkStatusReq{AccountID: accountID})
	if err != nil {
		return 0, fmt.Errorf("marshal link-status request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.asURL+"/internal/github/link-status", bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("build link-status request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Spawnery-AS-Secret", c.secret)

	resp, err := c.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("AS link-status request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("AS link-status returned HTTP %d", resp.StatusCode)
	}

	var out asLinkStatusResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, fmt.Errorf("decode AS link-status response: %w", err)
	}

	switch out.Status {
	case "active":
		return gitHubLinkStatusActive, nil
	case "relink_required":
		return gitHubLinkStatusRelinkRequired, nil
	case "none":
		return gitHubLinkStatusNone, nil
	default:
		return 0, fmt.Errorf("unexpected AS link-status %q", out.Status)
	}
}

// newHTTPASLinkChecker constructs an httpASLinkChecker. Pass nil client to use http.DefaultClient.
func newHTTPASLinkChecker(asURL, secret string, client *http.Client) asLinkChecker {
	if client == nil {
		client = http.DefaultClient
	}
	return &httpASLinkChecker{asURL: asURL, secret: secret, client: client}
}

// mountsHaveGitHubBackend reports whether any mount in the slice has a github: backend URI.
// Used by the CreateSpawn preflight to decide whether a link-status check is needed.
func mountsHaveGitHubBackend(mounts []store.Mount) bool {
	for _, m := range mounts {
		if strings.HasPrefix(m.BackendURI, "github:") {
			return true
		}
	}
	return false
}
