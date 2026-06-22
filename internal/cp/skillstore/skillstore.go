// Package skillstore provides content-addressed storage for skill tarballs in a Garage S3 bucket.
//
// The skills bucket (spawnery-skills) is assumed PRE-PROVISIONED out-of-band.
// DO NOT call MakeBucket — the CP's journal key is Forbidden for bucket creation (spike S1 finding).
//
// Object key scheme: skills/<sha256hex>.tar.zst (global content-addressed dedup).
// PutIfAbsent guards with a StatObject check; if the object already exists it is a no-op.
// Incomplete multipart uploads (minio-go auto-MPU above ~16 MiB) are cleaned via
// RemoveIncompleteUpload on PutObject error to prevent Garage MPU part leaks (§4.13).
package skillstore

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

const (
	// DefaultBucket is the skills bucket name.
	DefaultBucket = "spawnery-skills"
	// PresignTTL is the presigned URL lifetime — well above cold-node startup latency.
	PresignTTL = 30 * time.Minute
	// objectPrefix is the key prefix for all skill objects.
	objectPrefix = "skills/"
)

// SkillStore is the interface for content-addressed skill object storage.
// Implementations must be safe for concurrent use.
type SkillStore interface {
	// PutIfAbsent stores compressed under key skills/<sha256hex>.tar.zst.
	// If the object already exists (StatObject hit), it is a no-op (cross-user dedup).
	// On PutObject error, it attempts to cancel any incomplete multipart upload.
	PutIfAbsent(ctx context.Context, sha256hex string, compressed []byte, tags map[string]string) error
	// PresignedGet returns a time-limited GET URL for the given sha256hex key.
	// The URL is presigned against the node-reachable endpoint.
	PresignedGet(ctx context.Context, sha256hex string) (string, error)
}

// Config holds the parameters for constructing a garageSkillStore.
type Config struct {
	// Endpoint is the S3 host:port (no scheme) for CP-internal access (PutObject/StatObject).
	Endpoint string
	// NodeEndpoint is the S3 host:port for presigned GET URLs served to nodes.
	// May differ from Endpoint when CP and nodes are in different network namespaces.
	// Defaults to Endpoint when empty.
	NodeEndpoint string
	// AccessKeyID and SecretAccessKey are the S3 credentials.
	AccessKeyID     string
	SecretAccessKey string
	// Region is the S3 region label (Garage default "garage").
	Region string
	// DisableTLS uses plain HTTP (dev Garage). Never set in production.
	DisableTLS bool
	// Bucket is the skills bucket name (default DefaultBucket).
	Bucket string
}

// garageSkillStore is the Garage-backed SkillStore.
type garageSkillStore struct {
	client     *minio.Client
	nodeClient *minio.Client
	bucket     string
}

// New constructs a garageSkillStore from Config.
// The bucket is assumed to already exist — this constructor does NOT call MakeBucket.
func New(cfg Config) (SkillStore, error) {
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("skillstore: S3 endpoint is required")
	}
	if cfg.Bucket == "" {
		cfg.Bucket = DefaultBucket
	}
	nodeEndpoint := cfg.NodeEndpoint
	if nodeEndpoint == "" {
		nodeEndpoint = cfg.Endpoint
	}

	client, err := minio.New(stripScheme(cfg.Endpoint), &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		Secure: !cfg.DisableTLS,
		Region: cfg.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("skillstore: build S3 client: %w", err)
	}

	nodeClient, err := minio.New(stripScheme(nodeEndpoint), &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		Secure: !cfg.DisableTLS,
		Region: cfg.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("skillstore: build node S3 client: %w", err)
	}

	return &garageSkillStore{
		client:     client,
		nodeClient: nodeClient,
		bucket:     cfg.Bucket,
	}, nil
}

func (s *garageSkillStore) objectKey(sha256hex string) string {
	return objectPrefix + sha256hex + ".tar.zst"
}

// PutIfAbsent puts compressed bytes under skills/<sha256hex>.tar.zst.
// Guards with StatObject; no-ops if the object already exists.
// Calls RemoveIncompleteUpload on PutObject error to avoid Garage MPU part leaks.
func (s *garageSkillStore) PutIfAbsent(ctx context.Context, sha256hex string, compressed []byte, tags map[string]string) error {
	key := s.objectKey(sha256hex)

	// Guard: if the object already exists, skip (cross-user dedup).
	_, err := s.client.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{})
	if err == nil {
		// Object exists — no-op
		return nil
	}
	resp := minio.ToErrorResponse(err)
	if resp.Code != "NoSuchKey" && resp.StatusCode != 404 {
		return fmt.Errorf("skillstore: stat object %q: %w", key, err)
	}

	// Convert tags map to URL-encoded string
	tagStr := ""
	if len(tags) > 0 {
		vals := url.Values{}
		for k, v := range tags {
			vals.Set(k, v)
		}
		tagStr = vals.Encode()
	}

	opts := minio.PutObjectOptions{
		ContentType: "application/zstd",
	}
	if tagStr != "" {
		opts.UserTags = tags
	}

	_, err = s.client.PutObject(ctx, s.bucket, key, bytes.NewReader(compressed), int64(len(compressed)), opts)
	if err != nil {
		// Best-effort cleanup of any dangling incomplete MPU parts.
		// minio-go auto-MPU fires at ~16 MiB; a ~50 MiB incompressible tar will trigger it.
		// Garage does not auto-abort stale MPU parts, so we must do it here.
		_ = s.client.RemoveIncompleteUpload(ctx, s.bucket, key)
		return fmt.Errorf("skillstore: put object %q: %w", key, err)
	}
	return nil
}

// PresignedGet returns a time-limited GET URL for skills/<sha256hex>.tar.zst,
// signed against the node-reachable endpoint.
func (s *garageSkillStore) PresignedGet(ctx context.Context, sha256hex string) (string, error) {
	key := s.objectKey(sha256hex)
	u, err := s.nodeClient.PresignedGetObject(ctx, s.bucket, key, PresignTTL, nil)
	if err != nil {
		return "", fmt.Errorf("skillstore: presign %q: %w", key, err)
	}
	return u.String(), nil
}

// stripScheme removes http:// or https:// prefix from an endpoint string,
// as minio.New expects host:port without a scheme.
func stripScheme(endpoint string) string {
	endpoint = strings.TrimPrefix(endpoint, "https://")
	endpoint = strings.TrimPrefix(endpoint, "http://")
	return endpoint
}

// --- Fake (in-memory) SkillStore for tests ---

// FakeSkillStore is an in-memory SkillStore for hermetic unit tests.
type FakeSkillStore struct {
	objects map[string][]byte
	tags    map[string]map[string]string
	Calls   []string // records PutIfAbsent/PresignedGet calls
}

// NewFakeSkillStore returns a ready FakeSkillStore.
func NewFakeSkillStore() *FakeSkillStore {
	return &FakeSkillStore{
		objects: make(map[string][]byte),
		tags:    make(map[string]map[string]string),
	}
}

func (f *FakeSkillStore) PutIfAbsent(_ context.Context, sha256hex string, compressed []byte, tags map[string]string) error {
	f.Calls = append(f.Calls, "put:"+sha256hex)
	if _, ok := f.objects[sha256hex]; ok {
		return nil // no-op, already exists
	}
	f.objects[sha256hex] = compressed
	f.tags[sha256hex] = tags
	return nil
}

func (f *FakeSkillStore) PresignedGet(_ context.Context, sha256hex string) (string, error) {
	f.Calls = append(f.Calls, "presign:"+sha256hex)
	if _, ok := f.objects[sha256hex]; !ok {
		return "", fmt.Errorf("fake: object %q not found", sha256hex)
	}
	return "https://fake-garage/skills/" + sha256hex + ".tar.zst?sig=fake", nil
}

// Has reports whether the fake store contains an object for sha256hex.
func (f *FakeSkillStore) Has(sha256hex string) bool {
	_, ok := f.objects[sha256hex]
	return ok
}

// Tags returns the tags recorded for sha256hex.
func (f *FakeSkillStore) Tags(sha256hex string) map[string]string {
	return f.tags[sha256hex]
}
