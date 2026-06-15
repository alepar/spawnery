package journal

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/blob/filesystem"
)

// BlobBackend opens the Kopia blob storage for a spawn's repo. It is the seam
// that keeps the journaler store-agnostic: tests (and the phase-① node-local
// tier) use FilesystemBackend; a Garage/S3 backend slots in behind this
// interface later (design §1, T.6) WITHOUT touching the snapshot/restore code.
type BlobBackend interface {
	// Open returns the blob.Storage backing spawnID's repo. create=true permits
	// initializing the underlying location (e.g. mkdir for filesystem, lazy
	// bucket mint for S3).
	Open(ctx context.Context, spawnID string, create bool) (blob.Storage, error)
}

// GenerationBackendProvider returns the blob backend for one spawn generation.
// Production S3 implements this with GenerationKeyManager so normal snapshots,
// fork seeds, and restores open the repo with the generation-scoped key.
type GenerationBackendProvider interface {
	BackendFor(ctx context.Context, spawnID string, gen uint64) (BlobBackend, error)
}

// FilesystemBackend is a Kopia filesystem-backed blob store rooted at Root,
// one sub-directory per spawn. This is the hermetic-test + node-local-disk
// backend: no network. (The bounded-local-disk spool of design §6 also rides
// this backend.)
type FilesystemBackend struct {
	Root string
}

// Open implements BlobBackend.
func (f *FilesystemBackend) Open(ctx context.Context, spawnID string, create bool) (blob.Storage, error) {
	dir := filepath.Join(f.Root, spawnID, "repo")
	if create {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("journal: mkdir repo dir: %w", err)
		}
	}
	st, err := filesystem.New(ctx, &filesystem.Options{Path: dir}, create)
	if err != nil {
		return nil, fmt.Errorf("journal: open filesystem blob storage: %w", err)
	}
	return st, nil
}

// S3Backend (blob_s3.go) is the Garage/S3 BlobBackend, selectable via NewBackend
// alongside FilesystemBackend. It implements this same Open(ctx, spawnID, create)
// contract, so the snapshot/restore/maintenance code in repo.go is unchanged.
//
// The bucket-per-spawn + per-generation access-key mint/revoke fence (design §3
// roast M1) + lazy bucket mint (design §6) is implemented by GenerationKeyManager
// (genkey.go) over GarageAdmin (garage_admin.go): production S3 Manager wiring
// uses its BackendFor(spawnID, gen) on the actual snapshot/restore data path.
