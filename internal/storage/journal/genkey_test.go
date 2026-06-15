package journal

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// fakeAdmin is an in-memory bucketKeyAdmin for hermetic GenerationKeyManager
// tests: it records bucket creation, key mint/delete, and grants so a test can
// assert the per-generation mint/revoke contract without a live Garage.
type fakeAdmin struct {
	mu        sync.Mutex
	buckets   map[string]string // alias -> id
	keys      map[string]string // accessKeyID -> name
	grants    map[string]bool   // bucketID + "|" + accessKeyID
	ensureN   int
	nextKeyID int
}

func newFakeAdmin() *fakeAdmin {
	return &fakeAdmin{buckets: map[string]string{}, keys: map[string]string{}, grants: map[string]bool{}}
}

func (f *fakeAdmin) EnsureBucket(_ context.Context, alias string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureN++
	if id, ok := f.buckets[alias]; ok {
		return id, nil
	}
	id := "bucket-" + alias
	f.buckets[alias] = id
	return id, nil
}

func (f *fakeAdmin) CreateKey(_ context.Context, name string) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextKeyID++
	ak := fmt.Sprintf("GK%08d", f.nextKeyID)
	f.keys[ak] = name
	return ak, "secret-" + ak, nil
}

func (f *fakeAdmin) AllowKeyOnBucket(_ context.Context, bucketID, accessKeyID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.grants[bucketID+"|"+accessKeyID] = true
	return nil
}

func (f *fakeAdmin) DeleteKey(_ context.Context, accessKeyID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.keys, accessKeyID)
	return nil
}

func (f *fakeAdmin) keyExists(ak string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.keys[ak]
	return ok
}

// TestGenerationKeyManagerMintRevoke proves the core M1 contract: each generation
// gets a fresh, distinct, bucket-granted key on the spawn's single (lazily
// created) bucket, and revoking the superseded generation deletes only its key.
func TestGenerationKeyManagerMintRevoke(t *testing.T) {
	ctx := context.Background()
	admin := newFakeAdmin()
	g, err := NewGenerationKeyManager(GenerationKeyConfig{Admin: admin, S3Endpoint: "127.0.0.1:3900", DisableTLS: true})
	if err != nil {
		t.Fatal(err)
	}
	const spawnID = "sp-AbC123"

	cfg1, err := g.Mint(ctx, spawnID, 1)
	if err != nil {
		t.Fatalf("mint gen1: %v", err)
	}
	cfg2, err := g.Mint(ctx, spawnID, 2)
	if err != nil {
		t.Fatalf("mint gen2: %v", err)
	}

	// Same (lowercased) bucket across generations, but distinct keys.
	wantBucket := "spawnery-spawn-sp-abc123"
	if cfg1.Bucket != wantBucket || cfg2.Bucket != wantBucket {
		t.Fatalf("buckets = %q, %q; want %q (bucket-per-spawn, lowercased)", cfg1.Bucket, cfg2.Bucket, wantBucket)
	}
	if cfg1.AccessKeyID == cfg2.AccessKeyID {
		t.Fatalf("each generation must get a FRESH key; got the same %q", cfg1.AccessKeyID)
	}
	if admin.ensureN != 2 || len(admin.buckets) != 1 {
		t.Fatalf("expected one bucket ensured per mint over a single bucket; ensureN=%d buckets=%d", admin.ensureN, len(admin.buckets))
	}
	if !admin.grants["bucket-"+wantBucket+"|"+cfg1.AccessKeyID] || !admin.grants["bucket-"+wantBucket+"|"+cfg2.AccessKeyID] {
		t.Fatal("both generation keys must be granted on the bucket")
	}

	// Recreate fence: keep gen2, revoke everything prior.
	if err := g.RevokeSuperseded(ctx, spawnID, 2); err != nil {
		t.Fatalf("revoke superseded: %v", err)
	}
	if admin.keyExists(cfg1.AccessKeyID) {
		t.Fatal("superseded gen1 key must be revoked (deleted)")
	}
	if !admin.keyExists(cfg2.AccessKeyID) {
		t.Fatal("surviving gen2 key must NOT be revoked")
	}

	// Idempotent: revoking an already-gone generation is a no-op.
	if err := g.RevokeGeneration(ctx, spawnID, 1); err != nil {
		t.Fatalf("re-revoke gen1 should be a no-op, got %v", err)
	}
}

func TestGenerationKeyManagerBackendForReusesRecordedGeneration(t *testing.T) {
	ctx := context.Background()
	admin := newFakeAdmin()
	g, err := NewGenerationKeyManager(GenerationKeyConfig{Admin: admin, S3Endpoint: "127.0.0.1:3900", DisableTLS: true})
	if err != nil {
		t.Fatal(err)
	}

	b1, err := g.BackendFor(ctx, "sp-reopen", 4)
	if err != nil {
		t.Fatalf("backend first open: %v", err)
	}
	b2, err := g.BackendFor(ctx, "sp-reopen", 4)
	if err != nil {
		t.Fatalf("backend reopen: %v", err)
	}

	s31, ok := b1.(*S3Backend)
	if !ok {
		t.Fatalf("first backend type = %T, want *S3Backend", b1)
	}
	s32, ok := b2.(*S3Backend)
	if !ok {
		t.Fatalf("second backend type = %T, want *S3Backend", b2)
	}
	if s31.cfg.AccessKeyID != s32.cfg.AccessKeyID {
		t.Fatalf("reopening same generation minted a different key: %q then %q", s31.cfg.AccessKeyID, s32.cfg.AccessKeyID)
	}
	if admin.nextKeyID != 1 {
		t.Fatalf("same (spawn,generation) should mint once, minted %d keys", admin.nextKeyID)
	}
	if h := g.HoldExistingGeneration("sp-reopen", 4, "fork test"); h == nil {
		t.Fatal("reused generation key must remain recorded for fork holds")
	} else {
		h.Release()
	}
}

func TestGenerationHoldDefersRevokeUntilRelease(t *testing.T) {
	ctx := context.Background()
	admin := newFakeAdmin()
	g, err := NewGenerationKeyManager(GenerationKeyConfig{Admin: admin, S3Endpoint: "127.0.0.1:3900", DisableTLS: true})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := g.Mint(ctx, "sp-held", 7)
	if err != nil {
		t.Fatalf("mint held generation: %v", err)
	}
	hold := g.HoldGeneration("sp-held", 7, "fork")

	if err := g.RevokeGeneration(ctx, "sp-held", 7); err != nil {
		t.Fatalf("revoke held generation: %v", err)
	}
	if !admin.keyExists(cfg.AccessKeyID) {
		t.Fatal("held generation key must remain valid until the fork hold releases")
	}

	hold.Release()
	if admin.keyExists(cfg.AccessKeyID) {
		t.Fatal("revoke requested during hold must apply when the hold releases")
	}
	if _, ok := g.lookupKey("sp-held", 7); ok {
		t.Fatal("released deferred revoke must forget the generation key")
	}
}

// TestGarageAdminHTTP exercises the real GarageAdmin HTTP client against an
// httptest server mimicking the Garage v1 admin API — so the request/response
// wiring (paths, bodies, 404-idempotent delete) is covered without a live Garage.
func TestGarageAdminHTTP(t *testing.T) {
	var deleted []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/key":
			fmt.Fprint(w, `{"accessKeyId":"GKabc","secretAccessKey":"sek"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/bucket":
			fmt.Fprint(w, `{"id":"bk1"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/bucket":
			// No bucket yet -> 404 so EnsureBucket falls through to create.
			w.WriteHeader(http.StatusNotFound)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/bucket/allow":
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/key":
			id := r.URL.Query().Get("id")
			deleted = append(deleted, id)
			if id == "GKgone" {
				w.WriteHeader(http.StatusNotFound) // exercise idempotent revoke
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	ctx := context.Background()
	admin, err := NewGarageAdmin(srv.URL, "tok", srv.Client())
	if err != nil {
		t.Fatal(err)
	}

	ak, sk, err := admin.CreateKey(ctx, "k")
	if err != nil || ak != "GKabc" || sk != "sek" {
		t.Fatalf("CreateKey = %q,%q,%v", ak, sk, err)
	}
	id, err := admin.EnsureBucket(ctx, "b")
	if err != nil || id != "bk1" {
		t.Fatalf("EnsureBucket = %q,%v", id, err)
	}
	if err := admin.AllowKeyOnBucket(ctx, id, ak); err != nil {
		t.Fatalf("AllowKeyOnBucket: %v", err)
	}
	if err := admin.DeleteKey(ctx, ak); err != nil {
		t.Fatalf("DeleteKey: %v", err)
	}
	// A 404 delete is treated as success (idempotent revoke).
	if err := admin.DeleteKey(ctx, "GKgone"); err != nil {
		t.Fatalf("DeleteKey(absent) should be nil, got %v", err)
	}
	if len(deleted) != 2 {
		t.Fatalf("expected 2 delete calls, got %v", deleted)
	}
}
