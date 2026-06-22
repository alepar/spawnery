package spawnlet

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/klauspost/compress/zstd"
)

// ArtifactsMountPath is where a spawn's non-sensitive artifact staging dir is bind-mounted inside
// the agent container. It mirrors SecretsMountPath; agentinstall (sp-1bia) reads the materialized
// tree (and any upstream-authored manifest.json landed here as an ordinary artifact) in place.
const ArtifactsMountPath = "/run/spawnery/artifacts"

// maxPlainTarBytes is the maximum decoded (uncompressed) tar size accepted for by-ref artifacts.
// The LimitedReader fires at this cap before sha256 verification and unpack, bounding peak memory.
const maxPlainTarBytes = 50 << 20 // 50 MiB

// fetchTimeout is applied to each by-ref HTTP GET. 30m covers the ~30m presigned-URL TTL with
// slack for a slow network; it is intentionally generous — the SHA256 gate ensures integrity.
const fetchTimeout = 5 * time.Minute

// ArtifactContentType is the spawnlet-side copy of node.v1.ArtifactContentType (spawnlet imports no
// proto; internal/node converts). It selects how an artifact's inline bytes are materialized.
type ArtifactContentType int

const (
	ArtifactBytes ArtifactContentType = iota // single file written verbatim at Mode
	ArtifactTar                              // packaged dir tree, unpacked preserving per-file modes
)

// Artifact is the spawnlet-side copy of a node.v1.ArtifactSpec the node threads into AgentSelection.
// Content-agnostic: the substrate lands bytes; it ascribes no skill/mcp/config meaning.
// By-ref delivery: non-empty PresignedURL (or ObjectKey) selects the fetch path; ObjectKey +
// Sha256 are the durable identity; PresignedURL is the transient GET URL minted at StartSpawn.
type Artifact struct {
	ID          string
	Inline      []byte
	ContentType ArtifactContentType
	DestPath    string // confined under the staging (or, for sensitive, secrets) mount root
	Mode        uint32 // unix mode for BYTES; 0 => 0o644 (TAR carries per-file modes)
	Sensitive   bool
	EnvVarName  string // sensitive routing key under the secrets mount; falls back to DestPath

	// By-ref fields (non-sensitive only). Non-empty PresignedURL => fetch from object store.
	ObjectKey    string // content-addressed key, e.g. skills/<sha256>.tar.zst; durable identity
	Sha256       string // hex sha256 of the PLAIN (uncompressed) tar; integrity gate
	PresignedURL string // CP-minted short-lived GET URL; transient, redact from logs
}

// FetchError is the typed error returned by fetchObjectTar. Terminal=true means the artifact
// cannot be retrieved without operator intervention (wrong object, hash mismatch, size exceeded);
// Terminal=false means the error is transient and the caller may retry (network, 5xx, etc.).
type FetchError struct {
	Terminal bool
	msg      string
	err      error
}

func (e *FetchError) Error() string {
	if e.err != nil {
		return fmt.Sprintf("%s: %v", e.msg, e.err)
	}
	return e.msg
}

func (e *FetchError) Unwrap() error { return e.err }

func terminalFetch(msg string, err error) *FetchError { return &FetchError{Terminal: true, msg: msg, err: err} }
func retryFetch(msg string, err error) *FetchError    { return &FetchError{Terminal: false, msg: msg, err: err} }

// bytesFetcher abstracts the HTTP GET for by-ref artifacts (injectable in tests).
type bytesFetcher func(ctx context.Context, url string) ([]byte, error)

// ArtifactStager owns a per-node root of per-spawn staging dirs (parallel to SecretInjector). The
// per-spawn dir is bind-mounted into the agent at ArtifactsMountPath. Root should be a tmpfs in
// production; non-sensitive artifact bytes live here (world-/agent-readable per their declared mode).
type ArtifactStager struct {
	Root    string
	fetcher bytesFetcher // nil => defaultFetcher
}

// DirFor returns the per-spawn host staging dir (the bind-mount source for ArtifactsMountPath).
func (a ArtifactStager) DirFor(spawnID string) string {
	return filepath.Join(a.Root, spawnID)
}

// Remove deletes a spawn's whole staging subdir (teardown / pre-apply reseed). Missing dir is ok.
func (a ArtifactStager) Remove(spawnID string) error {
	if err := os.RemoveAll(a.DirFor(spawnID)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Materialize lands artifacts for spawnID. The per-spawn staging dir is wiped and recreated first so
// a resume re-thread is idempotent (no stale payloads survive). Non-sensitive artifacts are staged
// here at their confined DestPath (BYTES verbatim, TAR unpacked preserving per-file modes). Sensitive
// artifacts carrying inline bytes are routed to the secrets tmpfs at 0600 via secrets.Write keyed by
// EnvVarName (fallback DestPath) — never into the agent-readable staging dir; a sensitive artifact
// with empty inline is a no-op (its value arrives async over the SecretDelivery path).
// By-ref artifacts (non-empty PresignedURL) are fetched, sha256-verified, and unpacked via the
// existing unpackTar path. A *FetchError is returned for transport failures so the node can classify
// terminal vs retryable outcomes.
func (a ArtifactStager) Materialize(ctx context.Context, spawnID string, artifacts []Artifact, secrets SecretInjector) error {
	dir := a.DirFor(spawnID)
	if err := os.RemoveAll(dir); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("artifacts: reset staging dir: %w", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("artifacts: mkdir staging dir: %w", err)
	}
	for _, art := range artifacts {
		if art.Sensitive {
			if len(art.Inline) == 0 {
				continue // delivered async over SecretDelivery -> InjectSecret
			}
			key := art.EnvVarName
			if key == "" {
				key = art.DestPath
			}
			if _, err := secrets.Write(spawnID, key, art.Inline); err != nil {
				return fmt.Errorf("artifacts: route sensitive %q: %w", art.ID, err)
			}
			continue
		}
		if err := a.stage(ctx, dir, art); err != nil {
			return fmt.Errorf("artifacts: stage %q: %w", art.ID, err)
		}
	}
	return nil
}

func (a ArtifactStager) stage(ctx context.Context, stageDir string, art Artifact) error {
	rel, err := safeRel(art.DestPath)
	if err != nil {
		return err
	}
	dest := filepath.Join(stageDir, rel)

	// By-ref path: fetch zstd-compressed tar from presigned URL, verify sha256, unpack.
	if art.PresignedURL != "" {
		blob, err := a.fetchObjectTar(ctx, art)
		if err != nil {
			return err // *FetchError already typed
		}
		return unpackTar(dest, blob)
	}

	switch art.ContentType {
	case ArtifactTar:
		return unpackTar(dest, art.Inline)
	default: // ArtifactBytes
		mode := os.FileMode(art.Mode)
		if mode == 0 {
			mode = 0o644
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(dest, art.Inline, mode); err != nil {
			return err
		}
		return os.Chmod(dest, mode) // defeat umask
	}
}

// fetchObjectTar fetches art.PresignedURL, decodes the zstd wrapper, verifies the sha256 of the
// plain tar bytes against art.Sha256, and returns the verified plain tar bytes ready for unpackTar.
// A *FetchError is returned:
//   - transport/DNS/timeout errors -> Terminal=false ("Garage unreachable")
//   - HTTP 404               -> Terminal=true  ("skill object missing")
//   - other non-2xx (incl. 403/5xx) -> Terminal=false (retryable; future re-presign can hook in)
//   - plain tar exceeds cap  -> Terminal=true  ("skill object too large")
//   - sha256 mismatch        -> Terminal=true  ("sha256 mismatch")
func (a ArtifactStager) fetchObjectTar(ctx context.Context, art Artifact) ([]byte, error) {
	fetch := a.fetcher
	if fetch == nil {
		fetch = defaultFetcher
	}

	raw, err := fetch(ctx, art.PresignedURL)
	if err != nil {
		return nil, err // already a *FetchError from defaultFetcher or test stub
	}

	// Decode zstd wrapper.
	dec, err := zstd.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, terminalFetch("skill object: invalid zstd stream", err)
	}
	defer dec.Close()

	// Bounded read: cap at maxPlainTarBytes+1 so we can distinguish exact-size vs oversize.
	lr := &io.LimitedReader{R: dec, N: int64(maxPlainTarBytes) + 1}
	plain, err := io.ReadAll(lr)
	if err != nil {
		return nil, retryFetch("skill object: read error", err)
	}
	if int64(len(plain)) > int64(maxPlainTarBytes) {
		return nil, terminalFetch(fmt.Sprintf("skill object too large (> %d bytes)", maxPlainTarBytes), nil)
	}

	// Integrity gate: sha256 over the decoded plain tar bytes.
	if art.Sha256 != "" {
		sum := sha256.Sum256(plain)
		got := hex.EncodeToString(sum[:])
		if got != art.Sha256 {
			return nil, terminalFetch(fmt.Sprintf("sha256 mismatch: got %s want %s", got, art.Sha256), nil)
		}
	}

	return plain, nil
}

// defaultFetcher is the production HTTP GET implementation. It applies a per-request timeout and
// classifies transport vs HTTP-status errors per the §4.6 error taxonomy.
var defaultFetcher bytesFetcher = func(ctx context.Context, url string) ([]byte, error) {
	client := &http.Client{Timeout: fetchTimeout}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, terminalFetch("skill object: bad presigned URL", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, retryFetch("Garage unreachable", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	switch {
	case resp.StatusCode == http.StatusOK:
		// proceed
	case resp.StatusCode == http.StatusNotFound:
		return nil, terminalFetch("skill object missing (404)", nil)
	default:
		return nil, retryFetch(fmt.Sprintf("skill object fetch: HTTP %d", resp.StatusCode), nil)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, retryFetch("skill object: read response body", err)
	}
	return body, nil
}

// unpackTar extracts a tar blob into root, confining every entry under root and preserving per-file
// modes. Only regular files and directories are honored (symlinks/devices rejected as an escape risk).
func unpackTar(root string, blob []byte) error {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	tr := tar.NewReader(bytes.NewReader(blob))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		rel, err := safeRel(hdr.Name)
		if err != nil {
			return fmt.Errorf("tar entry %q: %w", hdr.Name, err)
		}
		target := filepath.Join(root, rel)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(hdr.Mode).Perm()); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode).Perm())
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil { //nolint:gosec // bounded by inline size cap upstream
				_ = f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
			if err := os.Chmod(target, os.FileMode(hdr.Mode).Perm()); err != nil {
				return err
			}
		default:
			return fmt.Errorf("tar entry %q: unsupported type %d", hdr.Name, hdr.Typeflag)
		}
	}
}
