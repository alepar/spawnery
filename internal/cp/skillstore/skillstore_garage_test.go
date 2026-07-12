//go:build garage_e2e

package skillstore_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"testing"
	"time"

	"spawnery/internal/cp/skillstore"
)

// TestGarageSkillStore_PutIfAbsentAndPresign exercises PutIfAbsent + PresignedGet + presigned GET
// against a live dev Garage instance (started via `just garage`).
//
// Requires environment:
//
//	JOURNAL_S3_ENDPOINT    — S3 endpoint host:port (e.g. 127.0.0.1:3900)
//	JOURNAL_S3_REGION      — region (default "garage")
//	JOURNAL_S3_DISABLE_TLS — "true" for plain HTTP
//
// Access credentials are taken from the standard JOURNAL_* env vars used by the spawnlet.
//
// The spawnery-skills bucket MUST be pre-provisioned out-of-band (the CP's journal key is
// Forbidden for MakeBucket — spike S1 finding). The test FAILs, never t.Skips, when Garage is down.
func TestGarageSkillStore_PutIfAbsentAndPresign(t *testing.T) {
	endpoint := os.Getenv("JOURNAL_S3_ENDPOINT")
	if endpoint == "" {
		t.Fatalf("JOURNAL_S3_ENDPOINT is required; start dev Garage with `just garage` and source deploy/garage/dev-creds.env")
	}
	accessKey := os.Getenv("JOURNAL_S3_ACCESS_KEY")
	secretKey := os.Getenv("JOURNAL_S3_SECRET_KEY")
	if accessKey == "" || secretKey == "" {
		t.Fatalf("JOURNAL_S3_ACCESS_KEY and JOURNAL_S3_SECRET_KEY are required; source deploy/garage/dev-creds.env")
	}
	region := os.Getenv("JOURNAL_S3_REGION")
	if region == "" {
		region = "garage"
	}
	disableTLS := os.Getenv("JOURNAL_S3_DISABLE_TLS") == "true"

	cfg := skillstore.Config{
		Endpoint:        endpoint,
		NodeEndpoint:    endpoint, // same host in dev (S2 cross-netns is task .7)
		AccessKeyID:     accessKey,
		SecretAccessKey: secretKey,
		Region:          region,
		DisableTLS:      disableTLS,
		Bucket:          "spawnery-skills",
	}

	store, err := skillstore.New(cfg)
	if err != nil {
		t.Fatalf("construct store: %v (Garage endpoint: %s)", err, endpoint)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Build a tiny synthetic object
	content := []byte("fake-tar-content-for-e2e-test")
	h := sha256.Sum256(content)
	sha := hex.EncodeToString(h[:])

	tags := map[string]string{
		"source":     "github.com/test/repo",
		"owner":      "e2e-test",
		"catalog_id": "test-catalog-id",
	}

	// First put: should upload
	if err := store.PutIfAbsent(ctx, sha, content, tags); err != nil {
		t.Fatalf("PutIfAbsent (first): %v", err)
	}

	// Second put: should be a no-op (StatObject guard fires)
	if err := store.PutIfAbsent(ctx, sha, content, tags); err != nil {
		t.Fatalf("PutIfAbsent (second, idempotent): %v", err)
	}

	// StatObject: a present sha reports nil (sp-mwco.4.4 HEAD-before-presign gate).
	if err := store.StatObject(ctx, sha); err != nil {
		t.Fatalf("StatObject (present): %v", err)
	}

	// StatObject: a random, never-written sha reports ErrObjectMissing (definitively absent,
	// not a transport/config fault).
	randomSha := randomHexSha(t)
	err = store.StatObject(ctx, randomSha)
	if err == nil {
		t.Fatalf("StatObject (random sha %s): want ErrObjectMissing, got nil", randomSha)
	}
	if !errors.Is(err, skillstore.ErrObjectMissing) {
		t.Fatalf("StatObject (random sha %s): want errors.Is(..., ErrObjectMissing), got %v", randomSha, err)
	}

	// Get: a present sha returns the exact bytes stored.
	got, err := store.Get(ctx, sha)
	if err != nil {
		t.Fatalf("Get (present): %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("Get (present) content mismatch: got %d bytes, want %d", len(got), len(content))
	}

	// Get: a random, never-written sha reports ErrObjectMissing.
	_, err = store.Get(ctx, randomSha)
	if err == nil {
		t.Fatalf("Get (random sha %s): want ErrObjectMissing, got nil", randomSha)
	}
	if !errors.Is(err, skillstore.ErrObjectMissing) {
		t.Fatalf("Get (random sha %s): want errors.Is(..., ErrObjectMissing), got %v", randomSha, err)
	}

	// Presign and GET
	presignedURL, err := store.PresignedGet(ctx, sha)
	if err != nil {
		t.Fatalf("PresignedGet: %v", err)
	}
	if presignedURL == "" {
		t.Fatal("PresignedGet returned empty URL")
	}

	resp, err := http.Get(presignedURL) //nolint:noctx
	if err != nil {
		t.Fatalf("GET presigned URL: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET presigned URL returned HTTP %d (expected 200)", resp.StatusCode)
	}

	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		t.Fatalf("read presigned response: %v", err)
	}
	if !bytes.Equal(buf.Bytes(), content) {
		t.Fatalf("presigned GET content mismatch: got %d bytes, want %d", buf.Len(), len(content))
	}

	// Verify sha256 of retrieved content
	retrieved := sha256.Sum256(buf.Bytes())
	if hex.EncodeToString(retrieved[:]) != sha {
		t.Fatalf("sha256 mismatch: stored %s, retrieved %s", sha, hex.EncodeToString(retrieved[:]))
	}

	t.Logf("PutIfAbsent + PresignedGet + GET round-trip OK (sha=%s url=%s)", sha, presignedURL)
}

// randomHexSha returns a random 64-hex-char string in the same shape as a sha256hex key, for
// use as a sha guaranteed never to have been PutIfAbsent'd.
func randomHexSha(t *testing.T) string {
	t.Helper()
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	return hex.EncodeToString(b[:])
}
