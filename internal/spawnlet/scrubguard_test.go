package spawnlet

// scrubguard_test.go: hermetic tests for the delta-scrub mount guard (sp-2tx8.2.2, spec §4.2).
//
// Test matrix:
//   MT1: mountTargetsOf lifts the container-side paths off the agent's runtime.Mount slice.
//   MT2: Create records the container-side mount table on the Spawn.
//   G-tbl: filterScrubPaths drops every scrub path that overlaps a mount target (either direction),
//          keeps disjoint ones, and refuses to scrub anything when the mount table is unknown.

import (
	"context"
	"reflect"
	"testing"

	"spawnery/internal/runtime"
)

// MT1: the guard reads its mount table from the container-side targets of the agent's binds.
func TestMountTargetsOf(t *testing.T) {
	got := mountTargetsOf([]runtime.Mount{
		{HostPath: "/data/app", ContainerPath: "/app", ReadOnly: true},
		{HostPath: "/data/main", ContainerPath: "/app/data"},
		{HostPath: "/data/secrets", ContainerPath: SecretsMountPath},
		{HostPath: "/data/empty", ContainerPath: ""}, // defensive: never a target
	})
	want := []string{"/app", "/app/data", SecretsMountPath}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mountTargetsOf = %v, want %v", got, want)
	}
}

// MT2: Create records the container-side mount table on the Spawn, so the scrub guard has a
// mount table to test against at suspend time.
func TestCreateRecordsMountTargets(t *testing.T) {
	fb := fakeBackend(t)
	m := NewManagerWithBackend(fb, &fakeApplier{}, ManagerConfig{
		AgentImage: "agent:base", SidecarImage: "s", DataRoot: t.TempDir(),
	})
	sp, err := m.Create(context.Background(), "sp-mt", writeApp(t), "model", "", "", 0)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// writeApp declares one mount: name=main path=data -> /app/data. /app itself is always bound.
	for _, want := range []string{"/app", "/app/data"} {
		found := false
		for _, tgt := range sp.MountTargets {
			if tgt == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("MountTargets = %v, missing %q", sp.MountTargets, want)
		}
	}
}

// G-tbl: filterScrubPaths keeps only scrub paths that cannot touch a mount.
func TestFilterScrubPaths(t *testing.T) {
	cases := []struct {
		name        string
		paths       []string
		targets     []string
		wantKept    []string
		wantSkipped []string
	}{
		{
			name:        "default paths are disjoint from the default mount table",
			paths:       []string{"/var/cache/apt", "/var/lib/apt/lists", "/tmp"},
			targets:     []string{"/app", "/app/data", SecretsMountPath},
			wantKept:    []string{"/var/cache/apt", "/var/lib/apt/lists", "/tmp"},
			wantSkipped: nil,
		},
		{
			name:        "scrub path IS a mount",
			paths:       []string{"/app/data", "/tmp"},
			targets:     []string{"/app", "/app/data"},
			wantKept:    []string{"/tmp"},
			wantSkipped: []string{"/app/data"},
		},
		{
			name:        "scrub path is UNDER a mount",
			paths:       []string{"/app/data/node_modules", "/tmp"},
			targets:     []string{"/app", "/app/data"},
			wantKept:    []string{"/tmp"},
			wantSkipped: []string{"/app/data/node_modules"},
		},
		{
			name:        "a mount is UNDER the scrub path (rm -rf recurses into it)",
			paths:       []string{"/tmp"},
			targets:     []string{"/app", "/tmp/work"},
			wantKept:    nil,
			wantSkipped: []string{"/tmp"},
		},
		{
			name:        "sibling prefix is not containment",
			paths:       []string{"/app/database"},
			targets:     []string{"/app/data"},
			wantKept:    []string{"/app/database"},
			wantSkipped: nil,
		},
		{
			name:        "trailing slashes and dot segments are normalized",
			paths:       []string{"/app/data/"},
			targets:     []string{"/app/./data"},
			wantKept:    nil,
			wantSkipped: []string{"/app/data/"},
		},
		{
			name:        "root scrub path overlaps everything",
			paths:       []string{"/"},
			targets:     []string{"/app"},
			wantKept:    nil,
			wantSkipped: []string{"/"},
		},
		{
			name:        "non-absolute scrub path is never scrubbed",
			paths:       []string{"tmp", ""},
			targets:     []string{"/app"},
			wantKept:    nil,
			wantSkipped: []string{"tmp", ""},
		},
		{
			name:        "unknown mount table scrubs nothing (fail-safe)",
			paths:       []string{"/tmp"},
			targets:     nil,
			wantKept:    nil,
			wantSkipped: []string{"/tmp"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kept, skipped := filterScrubPaths(tc.paths, tc.targets)
			if !equalPaths(kept, tc.wantKept) {
				t.Errorf("kept = %v, want %v", kept, tc.wantKept)
			}
			if !equalPaths(skipped, tc.wantSkipped) {
				t.Errorf("skipped = %v, want %v", skipped, tc.wantSkipped)
			}
		})
	}
}

// equalPaths compares two string slices, treating nil and empty as equal.
func equalPaths(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
