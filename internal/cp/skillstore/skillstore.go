// Package skillstore provides content-addressed storage for skill tarballs in a Garage S3 bucket.
//
// The skills bucket (spawnery-skills) is assumed PRE-PROVISIONED out-of-band.
// DO NOT call MakeBucket — the CP's journal key is Forbidden for bucket creation (spike S1 finding).
//
// Object key scheme: skills/<sha256hex>.tar.zst (global content-addressed dedup).
// PutIfAbsent guards with a StatObject check; if the object already exists it is a no-op.
// StatObject is also exposed on the interface directly — the CP-side HEAD-before-presign gate
// (internal/cp/artifacts.go, sp-mwco.4.4) uses it to fail a start early with a definitive
// "object missing" signal (ErrObjectMissing), distinct from a transport/config fault.
// Incomplete multipart uploads (minio-go auto-MPU above ~16 MiB) are cleaned via
// RemoveIncompleteUpload on PutObject error to prevent Garage MPU part leaks (§4.13).
package skillstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"sync"
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

// ErrObjectMissing is the sentinel StatObject returns when the object is DEFINITIVELY absent
// (HTTP 404 with S3 code NoSuchKey/NotFound). Any other StatObject error is a transport/auth/
// config fault, not a missing-object signal — callers MUST use errors.Is to distinguish them.
var ErrObjectMissing = errors.New("skillstore: object missing")

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
	// StatObject checks whether skills/<sha256hex>.tar.zst exists.
	// Returns nil if present; an error satisfying errors.Is(err, ErrObjectMissing) if
	// DEFINITIVELY absent; any other error indicates a transport/auth/config fault.
	StatObject(ctx context.Context, sha256hex string) error
	// Get downloads and returns the full compressed bytes stored under skills/<sha256hex>.tar.zst
	// (a GetBundleDiff SKILL.md body extraction, sp-mwco.1.7 §4.9 — the diff-token gate needs the
	// member content, not just its presence). Returns an error satisfying
	// errors.Is(err, ErrObjectMissing) if DEFINITIVELY absent; any other error indicates a
	// transport/auth/config fault.
	Get(ctx context.Context, sha256hex string) ([]byte, error)
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
	return ObjectKey(sha256hex)
}

// ObjectKey returns the Garage object key for the given sha256hex.
// Use this wherever a key must be constructed outside the skillstore package
// to keep the format consistent (and avoid drift with the internal objectKey method).
func ObjectKey(sha256hex string) string {
	return objectPrefix + sha256hex + ".tar.zst"
}

// PutIfAbsent puts compressed bytes under skills/<sha256hex>.tar.zst.
// Guards with StatObject; no-ops if the object already exists.
// Calls RemoveIncompleteUpload on PutObject error to avoid Garage MPU part leaks.
func (s *garageSkillStore) PutIfAbsent(ctx context.Context, sha256hex string, compressed []byte, tags map[string]string) error {
	key := s.objectKey(sha256hex)

	// Guard: if the object already exists, skip (cross-user dedup).
	if err := s.statKey(ctx, key); err != nil {
		if !errors.Is(err, ErrObjectMissing) {
			return err
		}
		// ErrObjectMissing — fall through to PutObject.
	} else {
		// Object exists — no-op
		return nil
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

	_, err := s.client.PutObject(ctx, s.bucket, key, bytes.NewReader(compressed), int64(len(compressed)), opts)
	if err != nil {
		// Best-effort cleanup of any dangling incomplete MPU parts.
		// minio-go auto-MPU fires at ~16 MiB; a ~50 MiB incompressible tar will trigger it.
		// Garage does not auto-abort stale MPU parts, so we must do it here.
		_ = s.client.RemoveIncompleteUpload(ctx, s.bucket, key)
		return fmt.Errorf("skillstore: put object %q: %w", key, err)
	}
	return nil
}

// StatObject checks whether skills/<sha256hex>.tar.zst exists.
// Returns nil if present; ErrObjectMissing (wrapped) if DEFINITIVELY absent; any other error
// (including NoSuchBucket, a config fault masquerading as a 404) is a transport/auth/config fault.
func (s *garageSkillStore) StatObject(ctx context.Context, sha256hex string) error {
	return s.statKey(ctx, s.objectKey(sha256hex))
}

// statKey is the shared classify-and-stat helper used by both StatObject and PutIfAbsent's
// existence guard. NoSuchBucket is also an HTTP 404 in S3 semantics but indicates a config
// fault (wrong/missing bucket), NOT a missing object — it must NOT be classified as
// ErrObjectMissing.
func (s *garageSkillStore) statKey(ctx context.Context, key string) error {
	_, err := s.client.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{})
	if err == nil {
		return nil
	}
	resp := minio.ToErrorResponse(err)
	if resp.Code == "NoSuchBucket" {
		return fmt.Errorf("skillstore: stat object %q: %w", key, err)
	}
	if resp.Code == "NoSuchKey" || resp.StatusCode == 404 {
		return fmt.Errorf("%w: %q: %v", ErrObjectMissing, key, err)
	}
	return fmt.Errorf("skillstore: stat object %q: %w", key, err)
}

// Get downloads and returns the full compressed bytes stored under skills/<sha256hex>.tar.zst.
// minio-go's GetObject is lazy — a 404 (or any other error) surfaces on the first Read, not on
// the GetObject call itself — so the ErrObjectMissing classification happens on the Read error,
// mirroring statKey's NoSuchKey/404 check.
func (s *garageSkillStore) Get(ctx context.Context, sha256hex string) ([]byte, error) {
	key := s.objectKey(sha256hex)
	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("skillstore: get object %q: %w", key, err)
	}
	defer obj.Close()

	data, err := io.ReadAll(obj)
	if err != nil {
		resp := minio.ToErrorResponse(err)
		if resp.Code == "NoSuchKey" || resp.StatusCode == 404 {
			return nil, fmt.Errorf("%w: %q: %v", ErrObjectMissing, key, err)
		}
		return nil, fmt.Errorf("skillstore: read object %q: %w", key, err)
	}
	return data, nil
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

// FakeSkillStore is an in-memory SkillStore for hermetic unit tests. Safe for concurrent use
// (the sp-mwco.4.4 HEAD-before-presign gate stats distinct shas in parallel).
type FakeSkillStore struct {
	// StatHook, if set, is called by StatObject BEFORE the presence check and its error (if
	// non-nil) is returned as-is — i.e. NOT wrapped in ErrObjectMissing, simulating a
	// transport/timeout fault. Tests use it to inject brownouts, count concurrent in-flight
	// calls, or block on ctx.Done() to simulate a hang past the caller's deadline.
	StatHook func(ctx context.Context, sha256hex string) error

	mu      sync.Mutex
	objects map[string][]byte
	tags    map[string]map[string]string
	calls   []string // records PutIfAbsent/PresignedGet/StatObject calls
}

// NewFakeSkillStore returns a ready FakeSkillStore.
func NewFakeSkillStore() *FakeSkillStore {
	return &FakeSkillStore{
		objects: make(map[string][]byte),
		tags:    make(map[string]map[string]string),
	}
}

func (f *FakeSkillStore) PutIfAbsent(_ context.Context, sha256hex string, compressed []byte, tags map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "put:"+sha256hex)
	if _, ok := f.objects[sha256hex]; ok {
		return nil // no-op, already exists
	}
	f.objects[sha256hex] = compressed
	f.tags[sha256hex] = tags
	return nil
}

func (f *FakeSkillStore) PresignedGet(_ context.Context, sha256hex string) (string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, "presign:"+sha256hex)
	_, ok := f.objects[sha256hex]
	f.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("fake: object %q not found", sha256hex)
	}
	return "https://fake-garage/skills/" + sha256hex + ".tar.zst?sig=fake", nil
}

// StatObject reports presence of sha256hex. If StatHook is set, it is called first and any
// non-nil error is returned verbatim (a transport fault, NOT ErrObjectMissing). Otherwise
// returns nil if present, or ErrObjectMissing (wrapped) if absent.
func (f *FakeSkillStore) StatObject(ctx context.Context, sha256hex string) error {
	f.mu.Lock()
	f.calls = append(f.calls, "stat:"+sha256hex)
	hook := f.StatHook
	_, ok := f.objects[sha256hex]
	f.mu.Unlock()

	if hook != nil {
		if err := hook(ctx, sha256hex); err != nil {
			return err
		}
	}
	if !ok {
		return fmt.Errorf("%w: %q", ErrObjectMissing, sha256hex)
	}
	return nil
}

// Get returns a copy of the bytes recorded for sha256hex, or ErrObjectMissing (wrapped) if absent.
func (f *FakeSkillStore) Get(_ context.Context, sha256hex string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "get:"+sha256hex)
	data, ok := f.objects[sha256hex]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrObjectMissing, sha256hex)
	}
	out := make([]byte, len(data))
	copy(out, data)
	return out, nil
}

// Delete removes sha256hex from the fake store, simulating an object having been lost from
// Garage — used by tests exercising a missing-object code path (e.g. GetBundleDiff's
// body_unavailable).
func (f *FakeSkillStore) Delete(sha256hex string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.objects, sha256hex)
	delete(f.tags, sha256hex)
}

// Has reports whether the fake store contains an object for sha256hex.
func (f *FakeSkillStore) Has(sha256hex string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.objects[sha256hex]
	return ok
}

// Tags returns the tags recorded for sha256hex.
func (f *FakeSkillStore) Tags(sha256hex string) map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tags[sha256hex]
}

// Calls returns a snapshot of the recorded PutIfAbsent/PresignedGet/StatObject calls, in order.
func (f *FakeSkillStore) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// Remove deletes the object for sha256hex, simulating a lost Garage object.
func (f *FakeSkillStore) Remove(sha256hex string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.objects, sha256hex)
	delete(f.tags, sha256hex)
}
