//go:build garage_e2e

// Live Garage per-generation key mint/revoke fence (design §3, roast M1).
// Build-tagged `garage_e2e` so the hermetic `go test ./...` never compiles it,
// and additionally GATED on GARAGE_S3_ENDPOINT so `go test -tags garage_e2e`
// SKIPS when no Garage is reachable.
//
// Bring up `just garage`, then:
//
//	GARAGE_S3_ENDPOINT=127.0.0.1:3900 \
//	GARAGE_ADMIN_ENDPOINT=http://127.0.0.1:3903 \
//	GARAGE_ADMIN_TOKEN=$(grep admin_token deploy/garage/garage.toml | cut -d'"' -f2) \
//	go test -tags garage_e2e -run TestGenerationKeyFenceGarage -v ./internal/storage/journal/
//
// Flow: mint gen-1 key -> write an object (ok) -> mint gen-2 key (fresh) -> revoke
// the superseded gen-1 key -> assert the gen-1 key can no longer write OR delete,
// while the surviving gen-2 key still can. Raw S3 ops go through minio-go (the
// same client Kopia's s3 backend uses) so the test exercises the access fence
// directly, independent of the Kopia repo layer.
package journal

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

func s3Client(t *testing.T, cfg S3Config) *minio.Client {
	t.Helper()
	cl, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		Secure: !cfg.DisableTLS,
		Region: cfg.Region,
	})
	if err != nil {
		t.Fatalf("minio client: %v", err)
	}
	return cl
}

func putObject(ctx context.Context, cl *minio.Client, bucket, key, data string) error {
	_, err := cl.PutObject(ctx, bucket, key, bytes.NewReader([]byte(data)), int64(len(data)), minio.PutObjectOptions{})
	return err
}

// TestGenerationKeyFenceGarage proves the M1 fence end-to-end against live Garage.
func TestGenerationKeyFenceGarage(t *testing.T) {
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

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	admin, err := NewGarageAdmin(adminEndpoint, adminToken, &http.Client{Timeout: 15 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	g, err := NewGenerationKeyManager(GenerationKeyConfig{
		Admin:        admin,
		S3Endpoint:   s3Endpoint,
		Region:       region,
		DisableTLS:   true,
		BucketPrefix: "sp-genfence-e2e-",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Unique spawn id so the bucket is fresh per run.
	spawnID := "k" + time.Now().Format("20060102150405")
	bucket := g.BucketFor(spawnID)
	const probe = "genfence/probe"

	// --- gen 1: mint + write ---
	cfg1, err := g.Mint(ctx, spawnID, 1)
	if err != nil {
		t.Fatalf("mint gen1: %v", err)
	}
	cl1 := s3Client(t, cfg1)
	if err := putObject(ctx, cl1, bucket, probe, "gen1-data"); err != nil {
		t.Fatalf("gen1 key must be able to write: %v", err)
	}

	// --- gen 2: a FRESH key on the same bucket ---
	cfg2, err := g.Mint(ctx, spawnID, 2)
	if err != nil {
		t.Fatalf("mint gen2: %v", err)
	}
	if cfg2.AccessKeyID == cfg1.AccessKeyID {
		t.Fatal("gen2 must get a fresh access key distinct from gen1")
	}
	cl2 := s3Client(t, cfg2)
	if err := putObject(ctx, cl2, bucket, "genfence/probe2", "gen2-data"); err != nil {
		t.Fatalf("gen2 key must be able to write: %v", err)
	}

	// --- revoke the superseded gen-1 key (keep gen2) ---
	if err := g.RevokeSuperseded(ctx, spawnID, 2); err != nil {
		t.Fatalf("revoke superseded gen1: %v", err)
	}

	// --- the revoked gen-1 key can no longer WRITE or DELETE ---
	if err := expectDenied(ctx, func() error { return putObject(ctx, cl1, bucket, "genfence/after-revoke", "nope") }); err != nil {
		t.Fatalf("revoked gen1 key must be DENIED writes: %v", err)
	}
	if err := expectDenied(ctx, func() error { return cl1.RemoveObject(ctx, bucket, probe, minio.RemoveObjectOptions{}) }); err != nil {
		t.Fatalf("revoked gen1 key must be DENIED deletes (the real fence — Garage has no object-lock): %v", err)
	}

	// --- the surviving gen-2 key still works (writes + deletes) ---
	if err := putObject(ctx, cl2, bucket, "genfence/probe3", "still-good"); err != nil {
		t.Fatalf("surviving gen2 key must still write: %v", err)
	}
	if err := cl2.RemoveObject(ctx, bucket, probe, minio.RemoveObjectOptions{}); err != nil {
		t.Fatalf("surviving gen2 key must still delete: %v", err)
	}
}

// expectDenied returns nil once op() returns an auth/forbidden error (the key is
// fenced), or an error if op keeps succeeding past the poll window (revocation
// may take a moment to propagate in Garage).
func expectDenied(ctx context.Context, op func() error) error {
	deadline := time.Now().Add(20 * time.Second)
	for {
		err := op()
		if err != nil && isDenied(err) {
			return nil
		}
		if time.Now().After(deadline) {
			if err == nil {
				return errDenied("operation still permitted after key revocation")
			}
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

type errDenied string

func (e errDenied) Error() string { return string(e) }

func isDenied(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "forbidden") ||
		strings.Contains(s, "access denied") ||
		strings.Contains(s, "accessdenied") ||
		strings.Contains(s, "invalidaccesskey") ||
		strings.Contains(s, "signaturedoesnotmatch") ||
		strings.Contains(s, "403")
}
