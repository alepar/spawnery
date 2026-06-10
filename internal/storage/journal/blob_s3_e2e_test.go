//go:build garage_e2e

// Live Garage/S3 round-trip for the S3Backend. Build-tagged `garage_e2e` so the
// hermetic `go test ./...` never compiles or runs it. It is additionally GATED
// on GARAGE_S3_ENDPOINT being set, so even `go test -tags garage_e2e ./...`
// SKIPS (not fails) when no Garage is reachable.
//
// Bring a dev Garage up with `just garage` (deploy/garage), then:
//
//	GARAGE_S3_ENDPOINT=127.0.0.1:3900 \
//	GARAGE_ADMIN_ENDPOINT=http://127.0.0.1:3903 \
//	GARAGE_ADMIN_TOKEN=$(grep admin_token deploy/garage/garage.toml | cut -d'"' -f2) \
//	go test -tags garage_e2e -run TestS3BackendRoundTripGarage -v ./internal/storage/journal/
//
// The test mints a fresh bucket + access key via Garage's admin API (mirroring
// the design's bucket-per-spawn + per-bucket key, §3), opens a Kopia repo on the
// S3 backend, snapshots a temp dir, restores the pinned manifest, and asserts the
// round-trip.
package journal

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// garageAdmin is a minimal client for the Garage v1 admin API
// (https://garagehq.deuxfleurs.fr/documentation/reference-manual/admin-api/).
type garageAdmin struct {
	endpoint string // e.g. http://127.0.0.1:3903
	token    string
	hc       *http.Client
}

func (g *garageAdmin) do(ctx context.Context, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(g.endpoint, "/")+path, rdr)
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

// createKey mints a fresh access key, returning (accessKeyID, secretAccessKey).
func (g *garageAdmin) createKey(ctx context.Context, name string) (string, string, error) {
	var out struct {
		AccessKeyID     string `json:"accessKeyId"`
		SecretAccessKey string `json:"secretAccessKey"`
	}
	if err := g.do(ctx, http.MethodPost, "/v1/key", map[string]string{"name": name}, &out); err != nil {
		return "", "", err
	}
	if out.AccessKeyID == "" || out.SecretAccessKey == "" {
		return "", "", fmt.Errorf("garage createKey: empty credentials in response")
	}
	return out.AccessKeyID, out.SecretAccessKey, nil
}

// createBucket creates a bucket with a global alias, returning its id.
func (g *garageAdmin) createBucket(ctx context.Context, alias string) (string, error) {
	var out struct {
		ID string `json:"id"`
	}
	if err := g.do(ctx, http.MethodPost, "/v1/bucket", map[string]string{"globalAlias": alias}, &out); err != nil {
		return "", err
	}
	if out.ID == "" {
		return "", fmt.Errorf("garage createBucket: empty bucket id in response")
	}
	return out.ID, nil
}

// allowKeyOnBucket grants the key read/write/owner on the bucket.
func (g *garageAdmin) allowKeyOnBucket(ctx context.Context, bucketID, accessKeyID string) error {
	body := map[string]any{
		"bucketId":    bucketID,
		"accessKeyId": accessKeyID,
		"permissions": map[string]bool{"read": true, "write": true, "owner": true},
	}
	return g.do(ctx, http.MethodPost, "/v1/bucket/allow", body, nil)
}

// TestS3BackendRoundTripGarage opens a Kopia repo on a live Garage bucket and
// asserts a snapshot/restore round-trip through the S3Backend.
func TestS3BackendRoundTripGarage(t *testing.T) {
	s3Endpoint := os.Getenv("GARAGE_S3_ENDPOINT")
	if s3Endpoint == "" {
		t.Skip("GARAGE_S3_ENDPOINT not set — bring up `just garage` and set GARAGE_S3_ENDPOINT/GARAGE_ADMIN_ENDPOINT/GARAGE_ADMIN_TOKEN to run")
	}
	adminEndpoint := os.Getenv("GARAGE_ADMIN_ENDPOINT")
	adminToken := os.Getenv("GARAGE_ADMIN_TOKEN")
	if adminEndpoint == "" || adminToken == "" {
		t.Fatal("GARAGE_ADMIN_ENDPOINT and GARAGE_ADMIN_TOKEN must be set alongside GARAGE_S3_ENDPOINT")
	}
	region := os.Getenv("GARAGE_REGION")
	if region == "" {
		region = "garage"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// --- mint a fresh bucket + per-bucket key via the admin API (design §3) ---
	admin := &garageAdmin{endpoint: adminEndpoint, token: adminToken, hc: &http.Client{Timeout: 15 * time.Second}}
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	bucket := "sp-journal-e2e-" + suffix
	accessKeyID, secretKey, err := admin.createKey(ctx, "sp-journal-e2e-key-"+suffix)
	if err != nil {
		t.Fatalf("create access key: %v", err)
	}
	bucketID, err := admin.createBucket(ctx, bucket)
	if err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	if err := admin.allowKeyOnBucket(ctx, bucketID, accessKeyID); err != nil {
		t.Fatalf("grant key on bucket: %v", err)
	}

	// --- build the S3 backend + a node-local-custody Manager ---
	backend, err := NewS3Backend(S3Config{
		Endpoint:        s3Endpoint,
		Bucket:          bucket,
		AccessKeyID:     accessKeyID,
		SecretAccessKey: secretKey,
		Region:          region,
		DisableTLS:      true, // dev Garage speaks plain HTTP
	})
	if err != nil {
		t.Fatalf("new s3 backend: %v", err)
	}

	root := t.TempDir()
	keyfile := filepath.Join(root, "node.key")
	if err := GenerateNodeKeyfile(keyfile); err != nil {
		t.Fatal(err)
	}
	custody, err := NewNodeLocalCustody(keyfile, filepath.Join(root, "seals"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(Config{
		RepoRoot: filepath.Join(root, "repos"),
		Backend:  backend,
		Custody:  custody,
	})
	if err != nil {
		t.Fatal(err)
	}

	const spawnID = "spawn-garage-e2e"

	// --- snapshot a temp dir ---
	src := t.TempDir()
	writeFile(t, src, "hello.txt", "garage-hello")
	writeFile(t, src, "sub/nested.txt", "garage-nested")
	mt := Mount{Name: "work", HostDir: src, Class: NodeLocal}

	ids, err := m.FinalSnapshot(ctx, spawnID, 1, []Mount{mt})
	if err != nil {
		t.Fatalf("final snapshot to garage: %v", err)
	}
	id := ids["work"]
	if id == "" {
		t.Fatal("garage snapshot manifest id is empty")
	}

	// Drop in-memory state so the restore reopens the repo from S3 cold.
	if err := m.Close(ctx, spawnID); err != nil {
		t.Fatal(err)
	}

	// --- restore the pinned manifest into a fresh dir + assert round-trip ---
	dst := t.TempDir()
	if err := m.Restore(ctx, spawnID, "work", id, dst); err != nil {
		t.Fatalf("restore from garage: %v", err)
	}
	if got := readFile(t, dst, "hello.txt"); got != "garage-hello" {
		t.Fatalf("restored hello.txt = %q, want garage-hello", got)
	}
	if got := readFile(t, dst, "sub/nested.txt"); got != "garage-nested" {
		t.Fatalf("restored nested = %q, want garage-nested", got)
	}

	// --- crash-fallback resolver finds the same manifest ---
	got, err := m.LatestForGeneration(ctx, spawnID, "work", 1)
	if err != nil || got != id {
		t.Fatalf("LatestForGeneration(1) = %s, %v; want %s", got, err, id)
	}
}
