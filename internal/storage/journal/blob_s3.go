package journal

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/blob/s3"
)

// S3Config configures an S3Backend against an S3-class object store. The dev +
// self-hosted target is Garage (design §1, T.6): a single binary speaking the
// S3 API with **path-style** addressing (Garage has no virtual-host bucket
// routing by default). minio-go (kopia's S3 client) auto-selects path-style for
// a custom, non-AWS Endpoint, so no explicit toggle is required.
type S3Config struct {
	// Endpoint is the S3 host[:port] WITHOUT scheme (e.g. "127.0.0.1:3900" for a
	// local Garage). Required.
	Endpoint string
	// Bucket is the bucket holding one spawn repo. Production S3 journaling gets
	// this from GenerationKeyManager's bucket-per-spawn mint path; static bucket
	// configuration remains useful for direct backend tests and local modes.
	Bucket string
	// AccessKeyID / SecretAccessKey authenticate to the store. Production Garage
	// credentials are minted per generation via GenerationKeyManager. Required.
	AccessKeyID     string
	SecretAccessKey string
	// Region is the S3 region label. Garage's default is "garage"; AWS-style
	// stores need their real region. Optional (Garage tolerates empty).
	Region string
	// Prefix is an optional object-key prefix prepended ahead of the per-spawn
	// segment — lets several nodes/tenants share one bucket without colliding.
	Prefix string
	// DisableTLS speaks plain HTTP instead of HTTPS — for a local dev Garage on
	// http://. Never set in production.
	DisableTLS bool
}

// S3Backend is a Garage/S3-backed BlobBackend (design §1, T.6). In production
// the journal Manager receives S3Backend instances from GenerationKeyManager for
// each (spawn,generation), so repo opens use generation-scoped credentials.
type S3Backend struct {
	cfg S3Config
}

// NewS3Backend validates cfg and returns the backend. Endpoint, Bucket and both
// credential fields are required.
func NewS3Backend(cfg S3Config) (*S3Backend, error) {
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("journal s3: Endpoint is required")
	}
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("journal s3: Bucket is required")
	}
	if cfg.AccessKeyID == "" || cfg.SecretAccessKey == "" {
		return nil, fmt.Errorf("journal s3: AccessKeyID and SecretAccessKey are required")
	}
	// minio-go rejects an Endpoint carrying a scheme; strip a stray one defensively.
	cfg.Endpoint = strings.TrimPrefix(strings.TrimPrefix(cfg.Endpoint, "https://"), "http://")
	return &S3Backend{cfg: cfg}, nil
}

// prefixFor is the object-key prefix isolating spawnID's repo within the bucket.
// It mirrors FilesystemBackend's "<spawnID>/repo" sub-directory layout. Kopia
// treats Prefix as a literal string prepended to every blob name, so the trailing
// slash makes it a clean directory boundary.
func (b *S3Backend) prefixFor(spawnID string) string {
	return path.Join(b.cfg.Prefix, spawnID, "repo") + "/"
}

// Open implements BlobBackend. create is accepted for interface parity but the
// S3 store needs no per-spawn initialization step here (the bucket must already
// exist — minted out of band by deploy/garage / the phase-② lazy mint); Kopia's
// repo.Initialize writes the format blob on first use.
func (b *S3Backend) Open(ctx context.Context, spawnID string, create bool) (blob.Storage, error) {
	_ = create
	st, err := s3.New(ctx, &s3.Options{
		Endpoint:        b.cfg.Endpoint,
		BucketName:      b.cfg.Bucket,
		Prefix:          b.prefixFor(spawnID),
		AccessKeyID:     b.cfg.AccessKeyID,
		SecretAccessKey: b.cfg.SecretAccessKey,
		Region:          b.cfg.Region,
		DoNotUseTLS:     b.cfg.DisableTLS,
	}, create)
	if err != nil {
		return nil, fmt.Errorf("journal: open s3 blob storage (bucket %q): %w", b.cfg.Bucket, err)
	}
	return st, nil
}

// BackendKind selects a BlobBackend implementation in NewBackend.
type BackendKind string

const (
	// BackendFilesystem is the hermetic-test + node-local-disk backend.
	BackendFilesystem BackendKind = "filesystem"
	// BackendS3 is the Garage/S3 object-store backend.
	BackendS3 BackendKind = "s3"
)

// BackendConfig is the union config used to select + build a BlobBackend, so a
// caller (spawnlet wiring) can pick the backend from configuration without
// importing the concrete types. An empty Kind defaults to filesystem.
type BackendConfig struct {
	Kind BackendKind
	// FilesystemRoot is the FilesystemBackend root (Kind == filesystem).
	FilesystemRoot string
	// S3 configures the S3Backend (Kind == s3).
	S3 S3Config
}

// NewBackend builds the selected BlobBackend.
func NewBackend(cfg BackendConfig) (BlobBackend, error) {
	switch cfg.Kind {
	case BackendFilesystem, "":
		if cfg.FilesystemRoot == "" {
			return nil, fmt.Errorf("journal: filesystem backend requires FilesystemRoot")
		}
		return &FilesystemBackend{Root: cfg.FilesystemRoot}, nil
	case BackendS3:
		return NewS3Backend(cfg.S3)
	default:
		return nil, fmt.Errorf("journal: unknown backend kind %q", cfg.Kind)
	}
}
