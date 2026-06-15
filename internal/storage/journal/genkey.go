package journal

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// bucketKeyAdmin is the subset of GarageAdmin the GenerationKeyManager needs,
// extracted so the manager can be unit-tested hermetically with a fake admin
// (no live Garage). GarageAdmin satisfies it.
type bucketKeyAdmin interface {
	EnsureBucket(ctx context.Context, alias string) (bucketID string, err error)
	CreateKey(ctx context.Context, name string) (accessKeyID, secretAccessKey string, err error)
	AllowKeyOnBucket(ctx context.Context, bucketID, accessKeyID string) error
	DeleteKey(ctx context.Context, accessKeyID string) error
}

// GenerationKeyManager implements the per-generation Garage key fencing (design
// §3, roast M1): each spawn generation gets a FRESH access key, scoped to the
// spawn's own bucket (bucket-per-spawn, lazily created — design §6), and the
// superseded generation's key is revoked (deleted) on suspend/recreate/migrate.
// Because Garage has no object-lock, deleting the prior key is what fences a
// partitioned/zombie node from DELETING blobs under a stale credential.
//
// It is a node-side component: the node receives the spawn's mount bindings and
// mints/revokes the per-generation key as generations turn over. The minted
// credentials are returned as an S3Config / BlobBackend the Kopia repo opens
// with.
type GenerationKeyManager struct {
	admin      bucketKeyAdmin
	s3Endpoint string
	region     string
	disableTLS bool
	bucketPfx  string

	mu    sync.Mutex
	keys  map[string]map[uint64]string // spawnID -> gen -> accessKeyID (for revoke)
	holds map[string]map[uint64]int    // spawnID -> gen -> active fork-point holds
}

// GenerationKeyConfig configures a GenerationKeyManager.
type GenerationKeyConfig struct {
	// Admin is the Garage admin client (a *GarageAdmin). Required.
	Admin bucketKeyAdmin
	// S3Endpoint is the S3 host[:port] (no scheme) the minted credentials address
	// (e.g. "127.0.0.1:3900"). Required.
	S3Endpoint string
	// Region is the S3 region label (Garage default "garage"). Optional.
	Region string
	// DisableTLS speaks plain HTTP (dev Garage). Never set in production.
	DisableTLS bool
	// BucketPrefix is prepended to the per-spawn bucket name. Default
	// "spawnery-spawn-". Bucket names must be DNS-like (lowercase, 3-63 chars);
	// the spawn id is lowercased and appended.
	BucketPrefix string
}

// NewGenerationKeyManager validates cfg and returns the manager.
func NewGenerationKeyManager(cfg GenerationKeyConfig) (*GenerationKeyManager, error) {
	if cfg.Admin == nil {
		return nil, fmt.Errorf("genkey: Admin is required")
	}
	if cfg.S3Endpoint == "" {
		return nil, fmt.Errorf("genkey: S3Endpoint is required")
	}
	pfx := cfg.BucketPrefix
	if pfx == "" {
		pfx = "spawnery-spawn-"
	}
	return &GenerationKeyManager{
		admin:      cfg.Admin,
		s3Endpoint: cfg.S3Endpoint,
		region:     cfg.Region,
		disableTLS: cfg.DisableTLS,
		bucketPfx:  pfx,
		keys:       map[string]map[uint64]string{},
		holds:      map[string]map[uint64]int{},
	}, nil
}

// BucketFor is the per-spawn bucket name (design §3 bucket-per-spawn).
func (g *GenerationKeyManager) BucketFor(spawnID string) string {
	return g.bucketPfx + strings.ToLower(spawnID)
}

// Mint lazily ensures the spawn's bucket exists, mints a FRESH access key for
// this generation, grants it on the bucket, and returns the S3Config the repo
// opens with. Recorded internally so the key can be revoked when superseded.
func (g *GenerationKeyManager) Mint(ctx context.Context, spawnID string, gen uint64) (S3Config, error) {
	bucket := g.BucketFor(spawnID)
	bucketID, err := g.admin.EnsureBucket(ctx, bucket)
	if err != nil {
		return S3Config{}, fmt.Errorf("genkey: ensure bucket %q: %w", bucket, err)
	}
	keyName := fmt.Sprintf("%s-gen%d", bucket, gen)
	ak, sk, err := g.admin.CreateKey(ctx, keyName)
	if err != nil {
		return S3Config{}, fmt.Errorf("genkey: create key %q: %w", keyName, err)
	}
	if err := g.admin.AllowKeyOnBucket(ctx, bucketID, ak); err != nil {
		// Best-effort cleanup of the just-minted key so it isn't orphaned.
		_ = g.admin.DeleteKey(ctx, ak)
		return S3Config{}, fmt.Errorf("genkey: allow key on bucket %q: %w", bucket, err)
	}
	g.record(spawnID, gen, ak)
	return S3Config{
		Endpoint:        g.s3Endpoint,
		Bucket:          bucket,
		AccessKeyID:     ak,
		SecretAccessKey: sk,
		Region:          g.region,
		DisableTLS:      g.disableTLS,
	}, nil
}

// BackendFor mints this generation's key (Mint) and returns a ready S3Backend —
// the slot-in seam for the journal Manager / cmd/spawnlet wiring.
func (g *GenerationKeyManager) BackendFor(ctx context.Context, spawnID string, gen uint64) (BlobBackend, error) {
	cfg, err := g.Mint(ctx, spawnID, gen)
	if err != nil {
		return nil, err
	}
	return NewS3Backend(cfg)
}

// RevokeGeneration deletes the recorded access key for (spawnID, gen) — the
// supersede/teardown fence. A gen with no recorded key (already revoked, or
// minted by another node) is a no-op.
func (g *GenerationKeyManager) RevokeGeneration(ctx context.Context, spawnID string, gen uint64) error {
	if g.held(spawnID, gen) {
		return nil
	}
	ak := g.lookup(spawnID, gen)
	if ak == "" {
		return nil
	}
	if err := g.admin.DeleteKey(ctx, ak); err != nil {
		return fmt.Errorf("genkey: revoke %s gen %d: %w", spawnID, gen, err)
	}
	g.forget(spawnID, gen)
	return nil
}

// RevokeSuperseded revokes every recorded generation key for the spawn EXCEPT
// keepGen — the recreate/migrate fence where exactly one generation survives.
func (g *GenerationKeyManager) RevokeSuperseded(ctx context.Context, spawnID string, keepGen uint64) error {
	for _, gen := range g.generations(spawnID) {
		if gen == keepGen {
			continue
		}
		if err := g.RevokeGeneration(ctx, spawnID, gen); err != nil {
			return err
		}
	}
	return nil
}

type GenerationHold struct {
	once    sync.Once
	release func()
}

func (h *GenerationHold) Release() {
	if h != nil {
		h.once.Do(h.release)
	}
}

func (g *GenerationKeyManager) HoldGeneration(spawnID string, gen uint64, reason string) *GenerationHold {
	_ = reason
	g.mu.Lock()
	g.addHoldLocked(spawnID, gen)
	g.mu.Unlock()

	return &GenerationHold{release: g.releaseHoldFunc(spawnID, gen)}
}

func (g *GenerationKeyManager) HoldExistingGeneration(spawnID string, gen uint64, reason string) *GenerationHold {
	_ = reason
	g.mu.Lock()
	if g.keys[spawnID][gen] == "" {
		g.mu.Unlock()
		return nil
	}
	g.addHoldLocked(spawnID, gen)
	g.mu.Unlock()

	return &GenerationHold{release: g.releaseHoldFunc(spawnID, gen)}
}

func (g *GenerationKeyManager) addHoldLocked(spawnID string, gen uint64) {
	if g.holds[spawnID] == nil {
		g.holds[spawnID] = map[uint64]int{}
	}
	g.holds[spawnID][gen]++
}

func (g *GenerationKeyManager) releaseHoldFunc(spawnID string, gen uint64) func() {
	return func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		if g.holds[spawnID] == nil {
			return
		}
		if g.holds[spawnID][gen] > 1 {
			g.holds[spawnID][gen]--
			return
		}
		delete(g.holds[spawnID], gen)
		if len(g.holds[spawnID]) == 0 {
			delete(g.holds, spawnID)
		}
	}
}

func (g *GenerationKeyManager) held(spawnID string, gen uint64) bool {
	return g.holdCount(spawnID, gen) > 0
}

func (g *GenerationKeyManager) holdCount(spawnID string, gen uint64) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.holds[spawnID][gen]
}

func (g *GenerationKeyManager) record(spawnID string, gen uint64, accessKeyID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.keys[spawnID] == nil {
		g.keys[spawnID] = map[uint64]string{}
	}
	g.keys[spawnID][gen] = accessKeyID
}

func (g *GenerationKeyManager) lookup(spawnID string, gen uint64) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.keys[spawnID][gen]
}

func (g *GenerationKeyManager) forget(spawnID string, gen uint64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.keys[spawnID] != nil {
		delete(g.keys[spawnID], gen)
		if len(g.keys[spawnID]) == 0 {
			delete(g.keys, spawnID)
		}
	}
}

func (g *GenerationKeyManager) generations(spawnID string) []uint64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]uint64, 0, len(g.keys[spawnID]))
	for gen := range g.keys[spawnID] {
		out = append(out, gen)
	}
	return out
}
