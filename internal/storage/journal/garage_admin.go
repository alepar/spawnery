package journal

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// GarageAdmin is a minimal client for the Garage v1 admin API
// (https://garagehq.deuxfleurs.fr/documentation/reference-manual/admin-api/),
// used to implement the per-generation access key fencing (design §3, roast M1):
// each spawn generation gets a FRESH access key on the spawn's bucket, and the
// superseded generation's key is revoked on suspend/recreate/migrate. Garage has
// no object-lock and no IAM/prefix policies, so a per-bucket access key minted +
// revoked per generation is the mechanism that fences a partitioned/zombie node's
// DELETE capability — not just its writes.
//
// Endpoints used (mirrors deploy/garage/bootstrap.sh):
//   - POST   /v1/key            mint an access key
//   - DELETE /v1/key?id=...     revoke (delete) an access key
//   - POST   /v1/bucket         create a bucket (global alias)
//   - GET    /v1/bucket?globalAlias=...   look up a bucket id
//   - POST   /v1/bucket/allow   grant a key permissions on a bucket
//   - POST   /v1/bucket/deny    remove a key's permissions on a bucket
type GarageAdmin struct {
	endpoint string // e.g. http://127.0.0.1:3903
	token    string
	hc       *http.Client
}

// NewGarageAdmin builds an admin client. endpoint is the admin API base URL
// (scheme included); token is the bearer admin_token from garage.toml. hc may be
// nil (a 15s-timeout client is used).
func NewGarageAdmin(endpoint, token string, hc *http.Client) (*GarageAdmin, error) {
	if endpoint == "" {
		return nil, fmt.Errorf("garage admin: endpoint is required")
	}
	if token == "" {
		return nil, fmt.Errorf("garage admin: token is required")
	}
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	return &GarageAdmin{endpoint: strings.TrimSuffix(endpoint, "/"), token: token, hc: hc}, nil
}

// do issues a request, decoding a 2xx JSON body into out (when non-nil). A
// non-2xx status becomes an error carrying the response body.
func (g *GarageAdmin) do(ctx context.Context, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, g.endpoint+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+g.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("garage admin %s %s: %d: %s", method, path, resp.StatusCode, raw)
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("garage admin %s %s: decode %q: %w", method, path, raw, err)
		}
	}
	return nil
}

// CreateKey mints a fresh access key, returning (accessKeyID, secretAccessKey).
func (g *GarageAdmin) CreateKey(ctx context.Context, name string) (string, string, error) {
	var out struct {
		AccessKeyID     string `json:"accessKeyId"`
		SecretAccessKey string `json:"secretAccessKey"`
	}
	if err := g.do(ctx, http.MethodPost, "/v1/key", map[string]string{"name": name}, &out); err != nil {
		return "", "", err
	}
	if out.AccessKeyID == "" || out.SecretAccessKey == "" {
		return "", "", fmt.Errorf("garage admin: CreateKey returned empty credentials")
	}
	return out.AccessKeyID, out.SecretAccessKey, nil
}

// DeleteKey revokes (deletes) an access key entirely — the per-generation fence
// on supersede/delete (design §3). A delete of an already-absent key is treated
// as success (idempotent revoke).
func (g *GarageAdmin) DeleteKey(ctx context.Context, accessKeyID string) error {
	err := g.do(ctx, http.MethodDelete, "/v1/key?id="+url.QueryEscape(accessKeyID), nil, nil)
	if err != nil && strings.Contains(err.Error(), ": 404:") {
		return nil
	}
	return err
}

// EnsureBucket returns the id of the bucket with the given global alias, creating
// it if absent (design §6 lazy bucket mint). Idempotent.
func (g *GarageAdmin) EnsureBucket(ctx context.Context, alias string) (string, error) {
	if id, err := g.bucketID(ctx, alias); err == nil && id != "" {
		return id, nil
	}
	var out struct {
		ID string `json:"id"`
	}
	err := g.do(ctx, http.MethodPost, "/v1/bucket", map[string]string{"globalAlias": alias}, &out)
	if err != nil {
		// A concurrent create may have won the race; fall back to a lookup.
		if id, gerr := g.bucketID(ctx, alias); gerr == nil && id != "" {
			return id, nil
		}
		return "", err
	}
	if out.ID == "" {
		return "", fmt.Errorf("garage admin: EnsureBucket returned empty bucket id")
	}
	return out.ID, nil
}

func (g *GarageAdmin) bucketID(ctx context.Context, alias string) (string, error) {
	var out struct {
		ID string `json:"id"`
	}
	if err := g.do(ctx, http.MethodGet, "/v1/bucket?globalAlias="+url.QueryEscape(alias), nil, &out); err != nil {
		return "", err
	}
	return out.ID, nil
}

// AllowKeyOnBucket grants the key read/write/owner on the bucket.
func (g *GarageAdmin) AllowKeyOnBucket(ctx context.Context, bucketID, accessKeyID string) error {
	return g.do(ctx, http.MethodPost, "/v1/bucket/allow", map[string]any{
		"bucketId":    bucketID,
		"accessKeyId": accessKeyID,
		"permissions": map[string]bool{"read": true, "write": true, "owner": true},
	}, nil)
}

// DenyKeyOnBucket removes the key's read/write/owner on the bucket — a softer
// fence than DeleteKey (the key keeps existing but loses this bucket). Offered
// for completeness; the per-generation path uses DeleteKey.
func (g *GarageAdmin) DenyKeyOnBucket(ctx context.Context, bucketID, accessKeyID string) error {
	return g.do(ctx, http.MethodPost, "/v1/bucket/deny", map[string]any{
		"bucketId":    bucketID,
		"accessKeyId": accessKeyID,
		"permissions": map[string]bool{"read": true, "write": true, "owner": true},
	}, nil)
}
