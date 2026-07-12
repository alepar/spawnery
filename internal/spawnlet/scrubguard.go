package spawnlet

// scrubguard.go: the delta-scrub mount guard (sp-2tx8.2.2, spec
// docs/superpowers/specs/2026-07-12-suspend-torn-snapshot-fix-design.md §4.2).
//
// The scrub is best-effort layer hygiene: `rm -rf <DeltaScrubPaths>` inside the agent container before
// the rootfs delta is captured. It runs BEFORE the mount snapshot (sp-2tx8.2.1), so a scrub path that
// overlaps one of the spawn's mounts would delete the user's persistent data AND then have the deletion
// snapshotted as authoritative. Hence: never scrub a path that overlaps a mount.

import (
	"context"
	"log"
	"path/filepath"
	"strings"

	"spawnery/internal/runtime"
)

// mountTargetsOf lifts the container-side paths off the agent's bind list. Called by Create, whose
// []runtime.Mount is the only place the container-side mount table exists.
func mountTargetsOf(mounts []runtime.Mount) []string {
	targets := make([]string, 0, len(mounts))
	for _, mt := range mounts {
		if mt.ContainerPath == "" {
			continue
		}
		targets = append(targets, mt.ContainerPath)
	}
	return targets
}

// pathUnder reports whether child is strictly under parent (both already Cleaned, absolute).
func pathUnder(child, parent string) bool {
	if parent == "/" {
		return child != "/"
	}
	return strings.HasPrefix(child, parent+"/")
}

// pathsOverlap reports whether deleting a (recursively) could touch b, in either direction:
// a == b, a is under b (scrub inside a mount), or b is under a (rm -rf recurses through the
// mountpoint). Both args must be absolute; they are Cleaned here so "/app/data/" == "/app/./data"
// and "/app/database" is NOT under "/app/data".
func pathsOverlap(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	return a == b || pathUnder(a, b) || pathUnder(b, a)
}

// filterScrubPaths splits the configured DeltaScrubPaths into the ones that are safe to `rm -rf`
// inside the agent (kept) and the ones that overlap the spawn's mount table (skipped). See the file
// header: the scrub must never delete a mount's contents, because the mount snapshot is taken after
// it and would record the deletion as authoritative.
//
// Fail-safe rules:
//   - an empty mount table means "we do not know what is mounted" -> scrub nothing;
//   - a non-absolute scrub path cannot be reasoned about -> skip it.
func filterScrubPaths(paths, mountTargets []string) (kept, skipped []string) {
	for _, p := range paths {
		if !filepath.IsAbs(p) {
			skipped = append(skipped, p)
			continue
		}
		if len(mountTargets) == 0 {
			skipped = append(skipped, p)
			continue
		}
		overlaps := false
		for _, tgt := range mountTargets {
			if !filepath.IsAbs(tgt) {
				continue // not a container-side bind target we can reason about
			}
			if pathsOverlap(p, tgt) {
				overlaps = true
				break
			}
		}
		if overlaps {
			skipped = append(skipped, p)
			continue
		}
		kept = append(kept, p)
	}
	return kept, skipped
}

// runDeltaScrub runs the pre-capture layer-hygiene scrub for sp, with the mount guard applied
// (spec §4.2): every configured DeltaScrubPaths entry that overlaps one of the spawn's container-side
// mount targets is skipped and logged at WARN, because the scrub runs BEFORE the mount snapshot and an
// `rm -rf` through a mountpoint would delete the user's persistent data and then have the deletion
// captured as authoritative. Silence would hide exactly that, so every skip is logged.
//
// Best-effort and non-fatal throughout: a scrub failure is logged and the caller proceeds to capture.
// Called from the single scrub call site on the suspend path (teardown today; sp-2tx8.2.1 moves that
// call site earlier, before Pause — this helper is unaffected either way).
func (m *Manager) runDeltaScrub(ctx context.Context, sp *Spawn) {
	if len(m.cfg.DeltaScrubPaths) == 0 || m.scrubFn == nil || sp.AgentID == "" {
		return
	}
	paths, skipped := filterScrubPaths(m.cfg.DeltaScrubPaths, sp.MountTargets)
	if len(skipped) > 0 {
		log.Printf("WARN delta scrub for %s: skipping %v — at or under a mount (mounts: %v); "+
			"scrubbing them would delete the spawn's persistent data", sp.ID, skipped, sp.MountTargets)
	}
	if len(paths) == 0 {
		return
	}
	// Pass sp.AgentID directly: on the FinishSuspend path the spawn has already been removed from the
	// store by Claim, so re-looking-up via ExecRun would fail with "spawn X has no agent container".
	if err := m.scrubFn(ctx, sp.AgentID, paths); err != nil {
		log.Printf("delta scrub for %s: %v (non-fatal; proceeding with capture)", sp.ID, err)
	}
}
