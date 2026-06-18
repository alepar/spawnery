package cp

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"spawnery/internal/cp/store"
	"spawnery/internal/manifest"
)

// AppSeed describes a demo app to register at boot (until E5's real publishing/registration).
type AppSeed struct {
	ID          string   // public app id (e.g. "spawnery/wiki")
	Ref         string   // definition ref the node mounts
	Version     string   // seeded version
	DisplayName string   // catalog title
	Summary     string   // one-line catalog blurb
	Tags        []string // catalog tags
	Mounts      []string // declared mount names
}

// SeedApps idempotently registers the demo apps/versions/mounts (no owner seeding).
// Used in prod mode where accountIds are created lazily by the AS, not here.
func SeedApps(ctx context.Context, st store.Store, apps []AppSeed) error {
	return Seed(ctx, st, nil, apps)
}

// Seed idempotently registers the dev-token owners + the demo apps/versions/mounts so CreateSpawn
// can resolve them. Owners come FROM the token map (every token's owner -> a row), so auth always
// resolves to a real owner. Replaced by E4 (OAuth) + E5 (catalog) later; no schema change.
func Seed(ctx context.Context, st store.Store, tokens map[string]string, apps []AppSeed) error {
	now := time.Now().Unix()
	seen := map[string]bool{}
	for _, owner := range tokens {
		if seen[owner] {
			continue
		}
		seen[owner] = true
		if err := st.Owners().Upsert(ctx, store.Owner{ID: owner, CreatedAt: now}); err != nil {
			return err
		}
	}
	for _, a := range apps {
		display := a.DisplayName
		if display == "" {
			display = a.ID
		}
		if err := st.Apps().Upsert(ctx, store.App{
			ID: a.ID, DisplayName: display, Summary: a.Summary,
			Tags: strings.Join(a.Tags, ","), Visibility: "public", Listed: true, CreatorID: "spawnery", CreatedAt: now,
		}); err != nil {
			return err
		}
		manifestJSON, manifestMounts, err := seedManifestJSON(a)
		if err != nil {
			return err
		}
		decls := make([]store.MountDecl, len(a.Mounts))
		for i, name := range a.Mounts {
			mt := manifestMounts[name]
			decls[i] = store.MountDecl{AppID: a.ID, Version: a.Version, Name: name, Path: mt.Path, Seed: mt.Seed, Required: true, Github: mt.Github}
		}
		if err := st.Apps().UpsertVersion(ctx,
			store.AppVersion{AppID: a.ID, Version: a.Version, Ref: a.Ref, Tier: store.TierReviewed, Manifest: manifestJSON, CreatedAt: now},
			decls); err != nil {
			return err
		}
	}
	return nil
}

func seedManifestJSON(a AppSeed) (string, map[string]manifest.Mount, error) {
	mounts := map[string]manifest.Mount{}
	mf, err := parseSeedManifest(a.Ref)
	if err != nil {
		if len(a.Mounts) == 0 {
			return "", mounts, nil
		}
		return "", nil, fmt.Errorf("parse seed manifest for app %q ref %q: %w", a.ID, a.Ref, err)
	}
	for _, mt := range mf.Storage.Mounts {
		mounts[mt.Name] = mt
	}
	type seedManifestMount struct {
		Name       string `json:"name"`
		Path       string `json:"path,omitempty"`
		Seed       string `json:"seed,omitempty"`
		Durability string `json:"durability,omitempty"`
		Github     bool   `json:"github,omitempty"`
	}
	type seedManifest struct {
		Mounts []seedManifestMount `json:"mounts"`
	}
	out := seedManifest{Mounts: make([]seedManifestMount, 0, len(a.Mounts))}
	for _, name := range a.Mounts {
		mt := mounts[name]
		out.Mounts = append(out.Mounts, seedManifestMount{
			Name:       name,
			Path:       mt.Path,
			Seed:       mt.Seed,
			Durability: mt.Durability,
			Github:     mt.Github,
		})
	}
	b, err := json.Marshal(out)
	if err != nil {
		return "", nil, err
	}
	return string(b), mounts, nil
}

func parseSeedManifest(ref string) (*manifest.Manifest, error) {
	mf, err := manifest.Parse(ref)
	if err == nil || filepath.IsAbs(ref) {
		return mf, err
	}
	return manifest.Parse(filepath.Join("..", "..", ref))
}
