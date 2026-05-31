// Package apps resolves a public app_id to the definition ref a node mounts at
// /app. A static map for the slice; the E5 catalog replaces it later.
package apps

type Resolver struct{ m map[string]string }

func New(m map[string]string) *Resolver { return &Resolver{m: m} }

func (r *Resolver) Resolve(appID string) (ref string, ok bool) {
	ref, ok = r.m[appID]
	return
}
