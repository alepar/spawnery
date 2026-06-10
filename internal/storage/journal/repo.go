package journal

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"

	"github.com/kopia/kopia/fs"
	"github.com/kopia/kopia/fs/localfs"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/format"
	"github.com/kopia/kopia/repo/maintenance"
	"github.com/kopia/kopia/repo/manifest"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/policy"
	"github.com/kopia/kopia/snapshot/restore"
	"github.com/kopia/kopia/snapshot/snapshotfs"
	"github.com/kopia/kopia/snapshot/snapshotmaintenance"
	"github.com/kopia/kopia/snapshot/upload"
)

// Snapshot tag/label keys. Tags ride in the manifest labels (design §3:
// "generation rides in snapshot tags"), so a restore can be pinned and a
// generation filtered. Keys must not collide with Kopia's reserved source
// labels (type/hostname/username/path).
const (
	tagGeneration = "spawneryGeneration"
	tagMount      = "spawneryMount"
)

// spawnUser is the SourceInfo.UserName stamped on every snapshot — set to the
// spawn id so all of a spawn's mounts (and generations) group + dedup together
// but never across spawns (design §1b, T.5).
//
// spawnHost is the SourceInfo.Host — a fixed logical host so the source key is
// stable across the real node hostname changing under the spawn.
const spawnHost = "spawnery-node"

// spawnRepo wraps an opened per-spawn Kopia repository. It is not safe for the
// same mount to be snapshotted concurrently — serialization is the serialQueue's
// job; the repo itself supports concurrent writers across mounts.
type spawnRepo struct {
	spawnID    string
	rep        repo.Repository
	configFile string
}

// sourceInfo is the stable SourceInfo for one mount of this spawn.
func (r *spawnRepo) sourceInfo(mountName string) snapshot.SourceInfo {
	return snapshot.SourceInfo{Host: spawnHost, UserName: r.spawnID, Path: "/" + mountName}
}

// openOrCreateRepo opens spawnID's Kopia repo, initializing + connecting it on
// first use. The blob backend is the seam (filesystem here, Garage/S3 later).
// configFile holds the local connection config + cache settings; it lives under
// repoRoot/<spawnID>.
func openOrCreateRepo(ctx context.Context, spawnID, repoRoot, password string, backend BlobBackend) (*spawnRepo, error) {
	dir := filepath.Join(repoRoot, spawnID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("journal: mkdir spawn repo dir: %w", err)
	}
	configFile := filepath.Join(dir, "kopia.config")

	st, err := backend.Open(ctx, spawnID, true)
	if err != nil {
		return nil, err
	}

	// Initialize the repo if the blob store has no format blob yet. Tolerate
	// "already initialized" (idempotent re-open).
	if err := repo.Initialize(ctx, st, &repo.NewRepositoryOptions{}, password); err != nil {
		if !errors.Is(err, format.ErrAlreadyInitialized) {
			_ = st.Close(ctx)
			return nil, fmt.Errorf("journal: initialize repo: %w", err)
		}
	}

	// Connect writes the local config file if absent.
	if _, statErr := os.Stat(configFile); errors.Is(statErr, os.ErrNotExist) {
		if err := repo.Connect(ctx, configFile, st, password, &repo.ConnectOptions{}); err != nil {
			_ = st.Close(ctx)
			return nil, fmt.Errorf("journal: connect repo: %w", err)
		}
	}
	// Connect dup'd the storage handle into the config; close ours.
	_ = st.Close(ctx)

	rep, err := repo.Open(ctx, configFile, password, &repo.Options{})
	if err != nil {
		return nil, fmt.Errorf("journal: open repo (wrong password or corrupt): %w", err)
	}
	return &spawnRepo{spawnID: spawnID, rep: rep, configFile: configFile}, nil
}

// snapshotMount snapshots hostDir as mountName at generation gen, stamping the
// generation + mount tags. It dedups against the latest prior manifest for the
// source. Returns the new manifest id.
func (r *spawnRepo) snapshotMount(ctx context.Context, mountName, hostDir string, gen uint64) (ManifestID, error) {
	si := r.sourceInfo(mountName)

	// Latest prior manifest (any generation) as the dedup base — cheap, and
	// Kopia's CDC dedups regardless.
	var previous []*snapshot.Manifest
	if prev, err := r.latestManifest(ctx, si, nil); err == nil && prev != nil {
		previous = []*snapshot.Manifest{prev}
	}

	var newID manifest.ID
	err := repo.WriteSession(ctx, r.rep, repo.WriteSessionOptions{Purpose: "snapshot:" + mountName}, func(ctx context.Context, w repo.RepositoryWriter) error {
		entry, err := localfs.NewEntry(hostDir)
		if err != nil {
			return fmt.Errorf("local fs entry: %w", err)
		}
		dir, ok := entry.(fs.Directory)
		if !ok {
			return fmt.Errorf("mount host path %q is not a directory", hostDir)
		}

		policyTree, err := policy.TreeForSource(ctx, w, si)
		if err != nil {
			// Fall back to the default policy if none is defined for the source.
			policyTree = policy.BuildTree(nil, policy.DefaultPolicy)
		}

		u := upload.NewUploader(w)
		man, err := u.Upload(ctx, dir, policyTree, si, previous...)
		if err != nil {
			return fmt.Errorf("upload: %w", err)
		}
		man.Tags = map[string]string{
			tagGeneration: strconv.FormatUint(gen, 10),
			tagMount:      mountName,
		}
		man.Description = fmt.Sprintf("spawnery %s mount=%s gen=%d", r.spawnID, mountName, gen)

		id, err := snapshot.SaveSnapshot(ctx, w, man)
		if err != nil {
			return fmt.Errorf("save snapshot: %w", err)
		}
		newID = id
		return nil
	})
	if err != nil {
		return "", err
	}
	return ManifestID(newID), nil
}

// restore restores a pinned manifest id into targetDir. The id is explicit —
// never "latest" (design §3, roast C1). Existing files are overwritten and
// written atomically (temp+rename) so a crash mid-restore leaves no partials.
func (r *spawnRepo) restore(ctx context.Context, id ManifestID, targetDir string) error {
	if id == "" {
		return fmt.Errorf("journal restore: empty manifest id (restore must be pinned, not latest)")
	}
	man, err := snapshot.LoadSnapshot(ctx, r.rep, manifest.ID(id))
	if err != nil {
		return fmt.Errorf("journal restore: load manifest %s: %w", id, err)
	}
	root, err := snapshotfs.SnapshotRoot(r.rep, man)
	if err != nil {
		return fmt.Errorf("journal restore: snapshot root: %w", err)
	}
	if err := os.MkdirAll(targetDir, 0o700); err != nil {
		return fmt.Errorf("journal restore: mkdir target: %w", err)
	}
	out := &restore.FilesystemOutput{
		TargetPath:           targetDir,
		OverwriteDirectories: true,
		OverwriteFiles:       true,
		OverwriteSymlinks:    true,
		WriteFilesAtomically: true,
	}
	if err := out.Init(ctx); err != nil {
		return fmt.Errorf("journal restore: init output: %w", err)
	}
	// RestoreDirEntryAtDepth=MaxInt32 forces a full ("deep") restore — the
	// default (0) shallow-restores every entry as a .kopia-entry placeholder.
	if _, err := restore.Entry(ctx, r.rep, out, root, restore.Options{RestoreDirEntryAtDepth: math.MaxInt32}); err != nil {
		return fmt.Errorf("journal restore: restore entry: %w", err)
	}
	return nil
}

// latestForGeneration returns the latest COMPLETE manifest id for (mount, gen)
// — the crash fallback ONLY (design §2/§3). "Latest" = latest StartTime among
// complete manifests. Returns ("", nil) when no complete manifest exists.
func (r *spawnRepo) latestForGeneration(ctx context.Context, mountName string, gen uint64) (ManifestID, error) {
	si := r.sourceInfo(mountName)
	tags := map[string]string{tagGeneration: strconv.FormatUint(gen, 10), tagMount: mountName}
	man, err := r.latestManifest(ctx, si, tags)
	if err != nil {
		return "", err
	}
	if man == nil {
		return "", nil
	}
	return ManifestID(man.ID), nil
}

// latestManifest returns the COMPLETE manifest with the latest StartTime for the
// source, optionally filtered by tags. nil (no error) means none matched.
func (r *spawnRepo) latestManifest(ctx context.Context, si snapshot.SourceInfo, tags map[string]string) (*snapshot.Manifest, error) {
	ids, err := snapshot.ListSnapshotManifests(ctx, r.rep, &si, tags)
	if err != nil {
		return nil, fmt.Errorf("journal: list manifests: %w", err)
	}
	if len(ids) == 0 {
		return nil, nil
	}
	mans, err := snapshot.LoadSnapshots(ctx, r.rep, ids)
	if err != nil {
		return nil, fmt.Errorf("journal: load manifests: %w", err)
	}
	// Keep only complete manifests (IncompleteReason == "").
	complete := mans[:0]
	for _, m := range mans {
		if m != nil && m.IncompleteReason == "" {
			complete = append(complete, m)
		}
	}
	if len(complete) == 0 {
		return nil, nil
	}
	sort.Slice(complete, func(i, j int) bool {
		return complete[i].StartTime.Before(complete[j].StartTime)
	})
	return complete[len(complete)-1], nil
}

// quickMaintenance runs index-compacting (non-deleting) maintenance (design §2,
// roast M5): mandatory at a regular cadence so seconds-cadence snapshots don't
// accumulate thousands of index blobs and wedge the repo. ModeQuick never
// deletes a blob without another copy, so it does not reopen the
// zombie-deletion hole. Full (deleting) maintenance is CP-commanded — out of
// scope for this slice.
func (r *spawnRepo) quickMaintenance(ctx context.Context) error {
	dr, ok := r.rep.(repo.DirectRepository)
	if !ok {
		return fmt.Errorf("journal: repo is not a DirectRepository; cannot run maintenance")
	}
	return repo.DirectWriteSession(ctx, dr, repo.WriteSessionOptions{Purpose: "quick-maintenance"},
		func(ctx context.Context, dw repo.DirectRepositoryWriter) error {
			// force=true: the journaler is the sole owner of this per-spawn repo,
			// so bypass Kopia's designated-maintenance-owner check (which
			// otherwise rejects a repo with no recorded owner).
			if err := snapshotmaintenance.Run(ctx, dw, maintenance.ModeQuick, true, maintenance.SafetyFull); err != nil {
				return fmt.Errorf("journal: quick maintenance: %w", err)
			}
			return nil
		})
}

func (r *spawnRepo) close(ctx context.Context) error {
	if r.rep == nil {
		return nil
	}
	return r.rep.Close(ctx)
}
