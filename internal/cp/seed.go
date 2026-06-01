package cp

import (
	"context"
	"time"

	"spawnery/internal/cp/store"
)

// AppSeed describes a demo app to register at boot (until E5's real publishing/registration).
type AppSeed struct {
	ID      string   // public app id (what clients send, e.g. "secret-app")
	Ref     string   // definition ref the node mounts (e.g. "examples/secret-app")
	Version string   // seeded version (e.g. "1.0.0")
	Mounts  []string // declared mount names (from the app manifest, e.g. ["main"])
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
		if err := st.Apps().Upsert(ctx, store.App{ID: a.ID, DisplayName: a.ID, CreatedAt: now}); err != nil {
			return err
		}
		decls := make([]store.MountDecl, len(a.Mounts))
		for i, name := range a.Mounts {
			decls[i] = store.MountDecl{AppID: a.ID, Version: a.Version, Name: name, Required: true}
		}
		if err := st.Apps().UpsertVersion(ctx,
			store.AppVersion{AppID: a.ID, Version: a.Version, Ref: a.Ref, Reviewed: true, CreatedAt: now},
			decls); err != nil {
			return err
		}
	}
	return nil
}
