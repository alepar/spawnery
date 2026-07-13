//go:build garage_e2e

package skillstore_test

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"spawnery/internal/cp/skillstore"
)

// TestPresignExpirySpike records the exact HTTP status + parsed S3 error <Code>/<Message> that a live
// dev Garage returns for (1) an EXPIRED presigned GET, (2) a presigned GET with a tampered
// X-Amz-Signature, and (3) a presigned GET for a key that does not exist. sp-mwco.4.2 Phase 0: this
// is the gating spike — the error-taxonomy classifier (artifacts.go classifyS3Error) triggers on
// these recorded Code/Message values, NOT the HTTP status, because expiry (400 InvalidRequest) and a
// persistent signature/config fault (403 AccessDenied) must be distinguishable to avoid burning the
// retry budget on a config fault or silently retrying forever on a genuinely expired presign.
//
// It builds a minio client DIRECTLY (not skillstore.PresignedGet, whose TTL is the fixed 30-minute
// production PresignTTL const) so it can mint a 1-second-TTL presign for case (1).
//
// Requires a live dev Garage (`just garage`) with its S3 creds sourced:
//
//	set -a; . deploy/garage/dev-creds.env; set +a
//	CGO_ENABLED=1 go test -tags garage_e2e -run TestPresignExpirySpike ./internal/cp/skillstore/ -v
//
// FAILs (t.Fatalf), never t.Skips, when Garage is unreachable — the garage_e2e tag is the opt-in.
func TestPresignExpirySpike(t *testing.T) {
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

	client, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: !disableTLS,
		Region: region,
	})
	if err != nil {
		t.Fatalf("construct minio client: %v (Garage endpoint: %s)", err, endpoint)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	bucket := skillstore.DefaultBucket
	key := skillstore.ObjectKey("spike0000000000000000000000000000000000000000000000000000000e2e")
	content := []byte("presign-expiry-spike-fixture")

	if _, err := client.PutObject(ctx, bucket, key, strings.NewReader(string(content)), int64(len(content)), minio.PutObjectOptions{ContentType: "application/zstd"}); err != nil {
		t.Fatalf("PutObject fixture: %v", err)
	}

	get := func(t *testing.T, label, rawURL string) (int, string) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			t.Fatalf("%s: build request: %v", label, err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: GET: %v", label, err)
		}
		defer resp.Body.Close() //nolint:errcheck
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("%s: read body: %v", label, err)
		}
		t.Logf("%s: HTTP %d body=%s", label, resp.StatusCode, string(body))
		return resp.StatusCode, string(body)
	}

	// Case 1: expired presign (1s TTL, sleep past it).
	expURL, err := client.PresignedGetObject(ctx, bucket, key, 1*time.Second, nil)
	if err != nil {
		t.Fatalf("presign (expiry case): %v", err)
	}
	time.Sleep(2 * time.Second)
	get(t, "expired", expURL.String())

	// Case 2: fresh presign, tampered X-Amz-Signature.
	sigURL, err := client.PresignedGetObject(ctx, bucket, key, 5*time.Minute, nil)
	if err != nil {
		t.Fatalf("presign (signature case): %v", err)
	}
	tampered := tamperSignature(t, sigURL.String())
	get(t, "tampered-signature", tampered)

	// Case 3: presign a key that does not exist.
	missingURL, err := client.PresignedGetObject(ctx, bucket, skillstore.ObjectKey("spike-does-not-exist"), 5*time.Minute, nil)
	if err != nil {
		t.Fatalf("presign (missing-key case): %v", err)
	}
	get(t, "missing-key", missingURL.String())
}

// tamperSignature flips the last character of the X-Amz-Signature query param so the request fails
// signature verification while remaining a well-formed presigned URL.
func tamperSignature(t *testing.T, rawURL string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse presigned URL: %v", err)
	}
	q := u.Query()
	sig := q.Get("X-Amz-Signature")
	if sig == "" {
		t.Fatalf("presigned URL has no X-Amz-Signature query param: %s", rawURL)
	}
	last := sig[len(sig)-1:]
	flip := "0"
	if last == "0" {
		flip = "1"
	}
	q.Set("X-Amz-Signature", sig[:len(sig)-1]+flip)
	u.RawQuery = q.Encode()
	return u.String()
}
