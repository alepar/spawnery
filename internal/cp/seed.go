package cp

import (
	"context"
	"strings"
	"time"

	"spawnery/internal/cp/store"
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
			Tags: strings.Join(a.Tags, ","), Visibility: "public", Listed: true, CreatedAt: now,
		}); err != nil {
			return err
		}
		decls := make([]store.MountDecl, len(a.Mounts))
		for i, name := range a.Mounts {
			decls[i] = store.MountDecl{AppID: a.ID, Version: a.Version, Name: name, Required: true}
		}
		if err := st.Apps().UpsertVersion(ctx,
			store.AppVersion{AppID: a.ID, Version: a.Version, Ref: a.Ref, Tier: store.TierReviewed, CreatedAt: now},
			decls); err != nil {
			return err
		}
	}
	return nil
}
