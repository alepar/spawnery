package main

import (
	"errors"
	"testing"

	"spawnery/internal/spawnlet"
)

func TestBuildManagerRunscPath(t *testing.T) {
	m, err := buildManager(spawnlet.ManagerConfig{
		ContainerRuntime: "runsc", AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("runsc buildManager: %v", err)
	}
	if m == nil {
		t.Fatal("nil manager")
	}
}

func TestBuildManagerDockerDefault(t *testing.T) {
	m, err := buildManager(spawnlet.ManagerConfig{
		AgentImage: "a", SidecarImage: "s", DataRoot: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("docker buildManager: %v", err)
	}
	if m == nil {
		t.Fatal("nil manager")
	}
}

func TestApplyUsernsProbe(t *testing.T) {
	probeErr := errors.New("daemon unreachable")
	cases := []struct {
		name     string
		base     uint32
		active   bool
		probeErr error
		wantMode string
		wantBase uint32
	}{
		// Happy path: probe succeeds, userns active, base parsed.
		{"success", 700000, true, nil, "remap", 700000},
		{"success base zero", 0, true, nil, "remap", 0},
		// Degraded: probe OK but daemon not running with userns-remap.
		{"not active", 0, false, nil, "off", 0},
		// Degraded: daemon info call failed.
		{"probe error", 0, false, probeErr, "off", 0},
		// The subtle ordering: active=true but base unparseable (err!=nil) — error-first
		// check means this still degrades rather than proceeding with a zero base.
		{"active but unparseable base", 0, true, probeErr, "off", 0},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			mode, base := applyUsernsProbe(tc.base, tc.active, tc.probeErr)
			if mode != tc.wantMode || base != tc.wantBase {
				t.Errorf("applyUsernsProbe(%d, %v, %v) = (%q, %d), want (%q, %d)",
					tc.base, tc.active, tc.probeErr, mode, base, tc.wantMode, tc.wantBase)
			}
		})
	}
}
