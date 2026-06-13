package journal

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/kopia/kopia/fs"
	"github.com/kopia/kopia/fs/localfs"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/manifest"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/policy"
	"github.com/kopia/kopia/snapshot/upload"
)

const (
	tagArtifactType       = "spawneryArtifactType"
	tagArtifactID         = "spawneryArtifactID"
	tagArtifactSequence   = "spawneryArtifactSequence"
	artifactPayloadName   = "payload"
	artifactDescriptionV1 = "spawnery-artifact/v1 "
)

// PutArtifact implements JournalManager.
func (m *Manager) PutArtifact(ctx context.Context, spawnID string, generation uint64, desc ArtifactDescriptor, r io.Reader) (ArtifactDescriptor, error) {
	s, err := m.state(ctx, spawnID)
	if err != nil {
		return ArtifactDescriptor{}, err
	}
	return s.repo.putArtifact(ctx, spawnID, generation, desc, r)
}

// GetArtifact implements JournalManager.
func (m *Manager) GetArtifact(ctx context.Context, spawnID string, generation uint64, artifactID string, w io.Writer) (ArtifactDescriptor, error) {
	if artifactID == "" {
		return ArtifactDescriptor{}, fmt.Errorf("journal artifact restore: empty artifact id (restore must be pinned, not latest)")
	}
	s, err := m.state(ctx, spawnID)
	if err != nil {
		return ArtifactDescriptor{}, err
	}
	return s.repo.getArtifact(ctx, spawnID, generation, artifactID, w)
}

// ListArtifacts implements JournalManager.
func (m *Manager) ListArtifacts(ctx context.Context, spawnID string, generation uint64, typ string) ([]ArtifactDescriptor, error) {
	s, err := m.state(ctx, spawnID)
	if err != nil {
		return nil, err
	}
	return s.repo.listArtifacts(ctx, spawnID, generation, typ)
}

func (r *spawnRepo) artifactSourceInfo() snapshot.SourceInfo {
	return snapshot.SourceInfo{Host: spawnHost, UserName: r.spawnID, Path: "/.spawnery/artifacts"}
}

func (r *spawnRepo) putArtifact(ctx context.Context, spawnID string, generation uint64, desc ArtifactDescriptor, rdr io.Reader) (ArtifactDescriptor, error) {
	if spawnID == "" {
		return ArtifactDescriptor{}, fmt.Errorf("journal artifact: empty spawn id")
	}
	if desc.Type == "" {
		return ArtifactDescriptor{}, fmt.Errorf("journal artifact: empty artifact type")
	}
	if err := validateArtifactFormat(desc); err != nil {
		return ArtifactDescriptor{}, err
	}
	if desc.ArtifactID == "" {
		id, err := newArtifactID()
		if err != nil {
			return ArtifactDescriptor{}, err
		}
		desc.ArtifactID = id
	}
	desc.SpawnID = spawnID
	desc.Generation = generation

	tmp, err := os.MkdirTemp("", "spawnery-artifact-*")
	if err != nil {
		return ArtifactDescriptor{}, fmt.Errorf("journal artifact: temp dir: %w", err)
	}
	defer os.RemoveAll(tmp)

	payloadPath := filepath.Join(tmp, artifactPayloadName)
	f, err := os.OpenFile(payloadPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return ArtifactDescriptor{}, fmt.Errorf("journal artifact: create payload temp file: %w", err)
	}
	h := sha256.New()
	n, copyErr := io.Copy(f, io.TeeReader(rdr, h))
	closeErr := f.Close()
	if copyErr != nil {
		return ArtifactDescriptor{}, fmt.Errorf("journal artifact: copy payload: %w", copyErr)
	}
	if closeErr != nil {
		return ArtifactDescriptor{}, fmt.Errorf("journal artifact: close payload: %w", closeErr)
	}
	desc.UncompressedSize = n
	digest := "sha256:" + hex.EncodeToString(h.Sum(nil))
	if desc.ContentDigest != "" && desc.ContentDigest != digest {
		return ArtifactDescriptor{}, fmt.Errorf("journal artifact: content digest mismatch: got %s want %s", digest, desc.ContentDigest)
	}
	desc.ContentDigest = digest

	si := r.artifactSourceInfo()
	var newID manifest.ID
	err = repo.WriteSession(ctx, r.rep, repo.WriteSessionOptions{Purpose: "artifact:" + desc.Type}, func(ctx context.Context, w repo.RepositoryWriter) error {
		entry, err := localfs.NewEntry(tmp)
		if err != nil {
			return fmt.Errorf("artifact local fs entry: %w", err)
		}
		dir, ok := entry.(fs.Directory)
		if !ok {
			return fmt.Errorf("artifact payload temp path %q is not a directory", tmp)
		}
		policyTree, err := policy.TreeForSource(ctx, w, si)
		if err != nil {
			policyTree = policy.BuildTree(nil, policy.DefaultPolicy)
		}
		u := upload.NewUploader(w)
		man, err := u.Upload(ctx, dir, policyTree, si)
		if err != nil {
			return fmt.Errorf("artifact upload: %w", err)
		}
		man.Tags = map[string]string{
			tagGeneration:       strconv.FormatUint(generation, 10),
			tagArtifactType:     desc.Type,
			tagArtifactID:       desc.ArtifactID,
			tagArtifactSequence: strconv.Itoa(desc.Sequence),
		}
		b, err := json.Marshal(desc)
		if err != nil {
			return fmt.Errorf("artifact descriptor marshal: %w", err)
		}
		man.Description = artifactDescriptionV1 + string(b)
		id, err := snapshot.SaveSnapshot(ctx, w, man)
		if err != nil {
			return fmt.Errorf("artifact save snapshot: %w", err)
		}
		newID = id
		return nil
	})
	if err != nil {
		return ArtifactDescriptor{}, err
	}
	if newID == "" {
		return ArtifactDescriptor{}, fmt.Errorf("journal artifact: empty Kopia artifact manifest id")
	}
	return desc, nil
}

func (r *spawnRepo) getArtifact(ctx context.Context, spawnID string, generation uint64, artifactID string, w io.Writer) (ArtifactDescriptor, error) {
	desc, id, err := r.findArtifact(ctx, spawnID, generation, artifactID)
	if err != nil {
		return ArtifactDescriptor{}, err
	}
	tmp, err := os.MkdirTemp("", "spawnery-artifact-restore-*")
	if err != nil {
		return ArtifactDescriptor{}, fmt.Errorf("journal artifact: restore temp dir: %w", err)
	}
	defer os.RemoveAll(tmp)
	if err := r.restore(ctx, ManifestID(id), tmp); err != nil {
		return ArtifactDescriptor{}, err
	}
	f, err := os.Open(filepath.Join(tmp, artifactPayloadName))
	if err != nil {
		return ArtifactDescriptor{}, fmt.Errorf("journal artifact: open restored payload: %w", err)
	}
	defer f.Close()
	if _, err := io.Copy(w, f); err != nil {
		return ArtifactDescriptor{}, fmt.Errorf("journal artifact: copy restored payload: %w", err)
	}
	return desc, nil
}

func (r *spawnRepo) listArtifacts(ctx context.Context, spawnID string, generation uint64, typ string) ([]ArtifactDescriptor, error) {
	if typ == "" {
		return nil, fmt.Errorf("journal artifact: empty artifact type")
	}
	tags := map[string]string{
		tagGeneration:   strconv.FormatUint(generation, 10),
		tagArtifactType: typ,
	}
	mans, err := r.artifactManifests(ctx, tags)
	if err != nil {
		return nil, err
	}
	out := make([]ArtifactDescriptor, 0, len(mans))
	for _, man := range mans {
		desc, err := descriptorFromManifest(man)
		if err != nil {
			return nil, err
		}
		if desc.SpawnID == spawnID && desc.Generation == generation && desc.Type == typ {
			out = append(out, desc)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Sequence == out[j].Sequence {
			return out[i].ArtifactID < out[j].ArtifactID
		}
		return out[i].Sequence < out[j].Sequence
	})
	return out, nil
}

func (r *spawnRepo) findArtifact(ctx context.Context, spawnID string, generation uint64, artifactID string) (ArtifactDescriptor, manifest.ID, error) {
	tags := map[string]string{
		tagGeneration: strconv.FormatUint(generation, 10),
		tagArtifactID: artifactID,
	}
	mans, err := r.artifactManifests(ctx, tags)
	if err != nil {
		return ArtifactDescriptor{}, "", err
	}
	for _, man := range mans {
		desc, err := descriptorFromManifest(man)
		if err != nil {
			return ArtifactDescriptor{}, "", err
		}
		if desc.SpawnID == spawnID && desc.Generation == generation && desc.ArtifactID == artifactID {
			return desc, man.ID, nil
		}
	}
	return ArtifactDescriptor{}, "", fmt.Errorf("journal artifact: pinned artifact %q for spawn %q generation %d not found", artifactID, spawnID, generation)
}

func (r *spawnRepo) artifactManifests(ctx context.Context, tags map[string]string) ([]*snapshot.Manifest, error) {
	si := r.artifactSourceInfo()
	ids, err := snapshot.ListSnapshotManifests(ctx, r.rep, &si, tags)
	if err != nil {
		return nil, fmt.Errorf("journal artifact: list manifests: %w", err)
	}
	mans, err := snapshot.LoadSnapshots(ctx, r.rep, ids)
	if err != nil {
		return nil, fmt.Errorf("journal artifact: load manifests: %w", err)
	}
	out := mans[:0]
	for _, man := range mans {
		if man != nil && man.IncompleteReason == "" {
			out = append(out, man)
		}
	}
	return out, nil
}

func descriptorFromManifest(man *snapshot.Manifest) (ArtifactDescriptor, error) {
	if man == nil || !strings.HasPrefix(man.Description, artifactDescriptionV1) {
		return ArtifactDescriptor{}, fmt.Errorf("journal artifact: manifest missing artifact descriptor")
	}
	var desc ArtifactDescriptor
	if err := json.Unmarshal([]byte(strings.TrimPrefix(man.Description, artifactDescriptionV1)), &desc); err != nil {
		return ArtifactDescriptor{}, fmt.Errorf("journal artifact: descriptor decode: %w", err)
	}
	return desc, nil
}

func validateArtifactFormat(desc ArtifactDescriptor) error {
	if desc.Type == ArtifactRootfsDelta {
		switch desc.Format {
		case ArtifactFormatOCILayerTar, ArtifactFormatOCILayout:
			return nil
		case "":
			return fmt.Errorf("journal artifact: rootfs_delta format is required")
		default:
			return fmt.Errorf("journal artifact: rootfs_delta format %q is not allowed; use uncompressed OCI input", desc.Format)
		}
	}
	if desc.Format == "" {
		return fmt.Errorf("journal artifact: format is required")
	}
	return nil
}

func newArtifactID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("journal artifact: generate artifact id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}
