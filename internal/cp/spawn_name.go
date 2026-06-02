package cp

import "fmt"

// nextSpawnName returns the first free display name given the base (the app's display name) and the
// set of names already taken by the owner. The first instance gets the bare base; collisions get
// " 2", " 3", … (the first free suffix). Best-effort dedup: the spawn id is the real key, so a rare
// concurrent-create race may still yield two equal names — harmless.
func nextSpawnName(base string, taken map[string]bool) string {
	if base == "" {
		base = "spawn"
	}
	if !taken[base] {
		return base
	}
	for i := 2; ; i++ {
		cand := fmt.Sprintf("%s %d", base, i)
		if !taken[cand] {
			return cand
		}
	}
}
