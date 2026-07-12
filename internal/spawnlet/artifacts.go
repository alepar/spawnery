package spawnlet

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
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

// fetchTimeout is applied to each by-ref HTTP GET attempt (sp-mwco.4.1). A 50 MiB-capped object at
// even 1 MiB/s is well under 60s; the sha256 gate — not a generous timeout — is what protects
// integrity, so the per-attempt timeout only needs to bound a stalled/dead connection. Previously
// 5m, which let a single artifact consume most of the aggregate stagingBudget on its own.
const fetchTimeout = 60 * time.Second

// stagingBudget is the aggregate deadline for Materialize-ing ALL artifacts of one spawn (sp-mwco.4.1).
// Applied once via context.WithTimeout so an N-member bundle cannot multiply the budget by summing
// per-artifact timeouts; it sits well inside the CP's 10m absolute resume backstop
// (cp/lifecycle.go defaultResumeTimeout).
const stagingBudget = 5 * time.Minute

// stagingFetchConcurrency bounds the number of by-ref artifact GETs in flight at once. A serial loop
// turns a 20-member bundle into 20 sequential Garage round-trips; bounded parallelism amortizes
// latency across artifacts without unbounded fan-out against the object store.
const stagingFetchConcurrency = 4

// stagingHeartbeatInterval paces the in-progress progress event emitted while by-ref fetches are in
// flight, so the CP's stall detector (30s no-progress window) keeps resetting during one slow
// artifact rather than only at artifact-completion boundaries. A package var (not const) so tests can
// shadow it to observe multiple heartbeats without a slow test.
var stagingHeartbeatInterval = 10 * time.Second

// retryAttempts caps the number of GET attempts for a single by-ref artifact when the failure is
// retryable (network error, 5xx, etc.); a Terminal *FetchError short-circuits after one attempt.
const retryAttempts = 3

// retryBaseDelay is the base backoff between retry attempts; fetchWithRetry doubles it per attempt
// (~0.5s, ~1s) with +/-25% jitter to avoid a thundering herd of retries against Garage.
const retryBaseDelay = 500 * time.Millisecond

// cacheMaxBytes bounds the total on-disk size of the node-local sha-keyed artifact cache. Unbounded
// growth is not acceptable on a node's disk; eviction is safe because a deleted entry is just a
// re-fetch (object keys are immutable content hashes). A package var (not const) so tests can shadow
// it to observe eviction without writing a 1 GiB fixture.
var cacheMaxBytes int64 = 1 << 30 // 1 GiB

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

func terminalFetch(msg string, err error) *FetchError {
	return &FetchError{Terminal: true, msg: msg, err: err}
}
func retryFetch(msg string, err error) *FetchError {
	return &FetchError{Terminal: false, msg: msg, err: err}
}

// bytesFetcher abstracts the HTTP GET for by-ref artifacts (injectable in tests).
type bytesFetcher func(ctx context.Context, url string) ([]byte, error)

// ArtifactStager owns a per-node root of per-spawn staging dirs (parallel to SecretInjector). The
// per-spawn dir is bind-mounted into the agent at ArtifactsMountPath. Root should be a tmpfs in
// production; non-sensitive artifact bytes live here (world-/agent-readable per their declared mode).
type ArtifactStager struct {
	Root        string
	CacheDir    string        // node-local sha-keyed by-ref artifact cache; "" => cache disabled
	fetcher     bytesFetcher  // nil => defaultFetcher
	maxTarBytes int64         // max decoded tar size; 0 => maxPlainTarBytes (50 MiB)
	budget      time.Duration // aggregate staging deadline; 0 => stagingBudget
	retryBase   time.Duration // retry backoff base; 0 => retryBaseDelay
}

// tarCap returns the effective decoded-tar size cap, applying the default when the field is unset.
func (a ArtifactStager) tarCap() int64 {
	if a.maxTarBytes > 0 {
		return a.maxTarBytes
	}
	return maxPlainTarBytes
}

// effectiveStagingBudget returns the aggregate staging deadline, applying the default when unset.
func (a ArtifactStager) effectiveStagingBudget() time.Duration {
	if a.budget > 0 {
		return a.budget
	}
	return stagingBudget
}

// effectiveRetryBase returns the retry backoff base, applying the default when unset.
func (a ArtifactStager) effectiveRetryBase() time.Duration {
	if a.retryBase > 0 {
		return a.retryBase
	}
	return retryBaseDelay
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
// a resume re-thread is idempotent (no stale payloads survive). Non-sensitive inline artifacts are
// staged here at their confined DestPath (BYTES verbatim, TAR unpacked preserving per-file modes),
// serially and first. Sensitive artifacts carrying inline bytes are routed to the secrets tmpfs at
// 0600 via secrets.Write keyed by EnvVarName (fallback DestPath) — never into the agent-readable
// staging dir; a sensitive artifact with empty inline is a no-op (its value arrives async over the
// SecretDelivery path).
//
// By-ref artifacts (non-empty PresignedURL) are staged by a bounded worker pool
// (stagingFetchConcurrency) with a node-local sha-keyed cache (cacheLoad/cacheStore) and per-artifact
// retry (fetchWithRetry). The whole call is bounded by an aggregate staging deadline
// (effectiveStagingBudget) applied once via context.WithTimeout — an N-member bundle cannot multiply
// the budget by summing per-artifact timeouts. progress (optional, nil-safe) fires once per completed
// by-ref artifact plus a wall-clock heartbeat while fetches are in flight, always from Materialize's
// own goroutine (collected off a channel — see materializeByRef), so the CP's no-progress stall timer
// keeps resetting across a whole bundle, not just at the start/end of Materialize.
func (a ArtifactStager) Materialize(ctx context.Context, spawnID string, artifacts []Artifact, secrets SecretInjector, progress func(phase, detail, stepKey string)) error {
	dir := a.DirFor(spawnID)
	if err := os.RemoveAll(dir); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("artifacts: reset staging dir: %w", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("artifacts: mkdir staging dir: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, a.effectiveStagingBudget())
	defer cancel()

	var byRef []Artifact
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
		if art.PresignedURL != "" {
			byRef = append(byRef, art)
			continue
		}
		if err := a.stage(dir, art); err != nil {
			return fmt.Errorf("artifacts: stage %q: %w", art.ID, err)
		}
	}

	if len(byRef) == 0 {
		return nil
	}
	if err := a.materializeByRef(ctx, dir, byRef, progress); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("artifacts: staging deadline (%s) exceeded materializing %d by-ref artifact(s): %w",
				a.effectiveStagingBudget(), len(byRef), err)
		}
		return err
	}
	return nil
}

// destPathFor resolves art's confined destination under stageDir.
func destPathFor(stageDir string, art Artifact) (string, error) {
	rel, err := safeRel(art.DestPath)
	if err != nil {
		return "", err
	}
	return filepath.Join(stageDir, rel), nil
}

// stage lands a single non-sensitive INLINE artifact (BYTES or TAR). By-ref artifacts go through
// materializeByRef/stageByRef instead.
func (a ArtifactStager) stage(stageDir string, art Artifact) error {
	dest, err := destPathFor(stageDir, art)
	if err != nil {
		return err
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

// byRefResult is one completed (or failed) worker outcome, reported back to materializeByRef's
// collector loop over a channel so progress() is only ever called from Materialize's own goroutine.
type byRefResult struct {
	art Artifact
	err error
}

// materializeByRef stages all by-ref artifacts through a bounded worker pool
// (stagingFetchConcurrency), collecting completions on the calling goroutine so progress fires from
// a single goroutine. The first worker error (terminal or retry-exhausted) cancels the rest via
// workCtx and is returned once all workers have unwound.
//
// Dispatch (gating new workers on the semaphore) runs in its own goroutine, concurrently with the
// collector loop below — NOT inline in this function before the collector starts. A semaphore-gated
// dispatch loop blocks synchronously once stagingFetchConcurrency workers are outstanding; if that
// loop ran on this goroutine before the collector loop began, a bundle bigger than the concurrency
// limit would see zero progress() calls until the ENTIRE bundle had been dispatched (i.e. until most
// of it had already finished) — silently reintroducing the stall-window gap this task exists to close.
func (a ArtifactStager) materializeByRef(ctx context.Context, stageDir string, artifacts []Artifact, progress func(phase, detail, stepKey string)) error {
	total := len(artifacts)
	results := make(chan byRefResult, total)
	sem := make(chan struct{}, stagingFetchConcurrency)
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var once sync.Once
	var firstErr error
	var wg sync.WaitGroup
	wg.Add(total)
	go func() {
		for _, art := range artifacts {
			sem <- struct{}{}
			go func(art Artifact) {
				defer wg.Done()
				defer func() { <-sem }()
				err := a.stageByRef(workCtx, stageDir, art)
				if err != nil {
					once.Do(func() {
						firstErr = fmt.Errorf("artifacts: stage %q: %w", art.ID, err)
						cancel()
					})
				}
				results <- byRefResult{art: art, err: err}
			}(art)
		}
	}()
	go func() {
		wg.Wait()
		close(results)
	}()

	heartbeat := time.NewTicker(stagingHeartbeatInterval)
	defer heartbeat.Stop()
	completed := 0
	for completed < total {
		select {
		case r, ok := <-results:
			if !ok {
				completed = total // channel closed early is not expected, but don't spin
				break
			}
			completed++
			if r.err == nil && progress != nil {
				progress("staging_artifacts", fmt.Sprintf("staged %s (%d/%d)", r.art.ID, completed, total), "")
			}
		case <-heartbeat.C:
			if progress != nil {
				progress("staging_artifacts", "fetching skill objects (in progress)", "")
			}
		}
	}
	wg.Wait() // all workers' firstErr writes happen-before their wg.Done(); safe to read firstErr now.

	return firstErr
}

// stageByRef stages a single by-ref artifact: a node-local sha-keyed cache hit skips the GET
// entirely; a miss fetches (with retry), verifies, caches (best-effort), and unpacks.
func (a ArtifactStager) stageByRef(ctx context.Context, stageDir string, art Artifact) error {
	dest, err := destPathFor(stageDir, art)
	if err != nil {
		return err
	}
	if plain, ok := a.cacheLoad(art); ok {
		return unpackTar(dest, plain)
	}
	plain, err := a.fetchWithRetry(ctx, art)
	if err != nil {
		return err
	}
	a.cacheStore(art, plain)
	return unpackTar(dest, plain)
}

// fetchWithRetry attempts art's by-ref fetch up to retryAttempts times. A Terminal *FetchError
// returns immediately (no retry — the object is unrecoverable without operator intervention).
// Retryable failures back off with doubling jittered delays (base effectiveRetryBase), aborting
// early without sleeping if the remaining aggregate budget is shorter than the next delay.
func (a ArtifactStager) fetchWithRetry(ctx context.Context, art Artifact) ([]byte, error) {
	base := a.effectiveRetryBase()
	for attempt := 1; attempt <= retryAttempts; attempt++ {
		plain, err := a.fetchObjectTar(ctx, art)
		if err == nil {
			return plain, nil
		}
		var fe *FetchError
		if errors.As(err, &fe) && fe.Terminal {
			return nil, err
		}
		if attempt == retryAttempts {
			break
		}
		delay := backoffDelay(base, attempt)
		if dl, ok := ctx.Deadline(); ok && time.Until(dl) < delay {
			break // not enough budget left for another attempt; report exhaustion below
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, retryFetch(fmt.Sprintf("Garage unreachable after %d attempt(s)", attempt), nil)
		case <-timer.C:
		}
	}
	return nil, retryFetch(fmt.Sprintf("Garage unreachable after %d attempts", retryAttempts), nil)
}

// backoffDelay returns the delay before retry attempt+1: base doubled per prior attempt, +/-25% jitter.
func backoffDelay(base time.Duration, attempt int) time.Duration {
	d := base * time.Duration(int64(1)<<uint(attempt-1))
	quarter := d / 4
	if quarter <= 0 {
		return d
	}
	jitter := time.Duration(rand.Int63n(int64(quarter)*2)) - quarter
	return d + jitter
}

// cachePath returns the on-disk path for sha's cache entry.
func (a ArtifactStager) cachePath(sha string) string {
	return filepath.Join(a.CacheDir, sha+".tar")
}

// cacheLoad returns the verified plain tar bytes for art from the node-local cache. ok is false on a
// disabled cache (CacheDir=="" or art.Sha256==""), a miss, a read error, or a hash mismatch — a
// mismatch deletes the stale/corrupt entry so the caller falls through to a clean re-fetch.
func (a ArtifactStager) cacheLoad(art Artifact) ([]byte, bool) {
	if a.CacheDir == "" || art.Sha256 == "" {
		return nil, false
	}
	path := a.cachePath(art.Sha256)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != art.Sha256 {
		_ = os.Remove(path) // corrupt/tampered entry; do not serve it
		return nil, false
	}
	return data, true
}

// cacheStore writes plain (already sha256-verified by the caller) into the cache keyed by
// art.Sha256, via a tmp file + atomic rename so a concurrent reader never observes a partial write.
// Content is addressed by its hash, so concurrent identical writers racing to the same final path are
// harmless. Best-effort: any error here must not fail staging (the artifact is already staged from
// the just-fetched bytes regardless of whether it lands in the cache).
func (a ArtifactStager) cacheStore(art Artifact, plain []byte) {
	if a.CacheDir == "" || art.Sha256 == "" {
		return
	}
	if err := os.MkdirAll(a.CacheDir, 0o755); err != nil {
		return
	}
	a.pruneCache(int64(len(plain)))
	tmp, err := os.CreateTemp(a.CacheDir, "tmp-*")
	if err != nil {
		return
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(plain); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return
	}
	if err := os.Rename(tmpPath, a.cachePath(art.Sha256)); err != nil {
		_ = os.Remove(tmpPath)
	}
}

// pruneCache deletes oldest-by-mtime cache entries until the cache dir's total size, plus incoming
// bytes about to be written, is under cacheMaxBytes. Unbounded node disk growth is not acceptable;
// eviction is safe here because a deleted entry is just a re-fetch (immutable content-addressed key).
func (a ArtifactStager) pruneCache(incoming int64) {
	entries, err := os.ReadDir(a.CacheDir)
	if err != nil {
		return
	}
	type cacheFile struct {
		path    string
		size    int64
		modTime time.Time
	}
	files := make([]cacheFile, 0, len(entries))
	var total int64
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, cacheFile{path: filepath.Join(a.CacheDir, e.Name()), size: info.Size(), modTime: info.ModTime()})
		total += info.Size()
	}
	total += incoming
	if total <= cacheMaxBytes {
		return
	}
	sort.Slice(files, func(i, j int) bool { return files[i].modTime.Before(files[j].modTime) })
	for _, f := range files {
		if total <= cacheMaxBytes {
			return
		}
		if err := os.Remove(f.path); err == nil {
			total -= f.size
		}
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

	// Bounded read: cap at tarCap()+1 so we can distinguish exact-size vs oversize.
	cap := a.tarCap()
	lr := &io.LimitedReader{R: dec, N: cap + 1}
	plain, err := io.ReadAll(lr)
	if err != nil {
		return nil, retryFetch("skill object: read error", err)
	}
	if int64(len(plain)) > cap {
		return nil, terminalFetch(fmt.Sprintf("skill object too large (> %d bytes)", cap), nil)
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
	// Bound the compressed read: a legitimate payload that decompresses to <= maxPlainTarBytes
	// cannot have a compressed body larger than maxPlainTarBytes, so this caps peak memory end-to-end.
	clr := &io.LimitedReader{R: resp.Body, N: maxPlainTarBytes + 1}
	body, err := io.ReadAll(clr)
	if err != nil {
		return nil, retryFetch("skill object: read response body", err)
	}
	if int64(len(body)) > maxPlainTarBytes {
		return nil, terminalFetch(fmt.Sprintf("skill object compressed body too large (> %d bytes)", maxPlainTarBytes), nil)
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
