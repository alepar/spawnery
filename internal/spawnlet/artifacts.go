package spawnlet

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// ArtifactsMountPath is where a spawn's non-sensitive artifact staging dir is bind-mounted inside
// the agent container. It mirrors SecretsMountPath; agentinstall (sp-1bia) reads the materialized
// tree (and any upstream-authored manifest.json landed here as an ordinary artifact) in place.
const ArtifactsMountPath = "/run/spawnery/artifacts"

// ArtifactContentType is the spawnlet-side copy of node.v1.ArtifactContentType (spawnlet imports no
// proto; internal/node converts). It selects how an artifact's inline bytes are materialized.
type ArtifactContentType int

const (
	ArtifactBytes ArtifactContentType = iota // single file written verbatim at Mode
	ArtifactTar                              // packaged dir tree, unpacked preserving per-file modes
)

// Artifact is the spawnlet-side copy of a node.v1.ArtifactSpec the node threads into AgentSelection.
// Content-agnostic: the substrate lands bytes; it ascribes no skill/mcp/config meaning.
type Artifact struct {
	ID          string
	Inline      []byte
	ContentType ArtifactContentType
	DestPath    string // confined under the staging (or, for sensitive, secrets) mount root
	Mode        uint32 // unix mode for BYTES; 0 => 0o644 (TAR carries per-file modes)
	Sensitive   bool
	EnvVarName  string // sensitive routing key under the secrets mount; falls back to DestPath
}

// ArtifactStager owns a per-node root of per-spawn staging dirs (parallel to SecretInjector). The
// per-spawn dir is bind-mounted into the agent at ArtifactsMountPath. Root should be a tmpfs in
// production; non-sensitive artifact bytes live here (world-/agent-readable per their declared mode).
type ArtifactStager struct {
	Root string
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
func (a ArtifactStager) Materialize(spawnID string, artifacts []Artifact, secrets SecretInjector) error {
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
		if err := a.stage(dir, art); err != nil {
			return fmt.Errorf("artifacts: stage %q: %w", art.ID, err)
		}
	}
	return nil
}

func (a ArtifactStager) stage(stageDir string, art Artifact) error {
	rel, err := safeRel(art.DestPath)
	if err != nil {
		return err
	}
	dest := filepath.Join(stageDir, rel)
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
