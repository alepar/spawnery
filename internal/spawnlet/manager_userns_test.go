package spawnlet

import (
	"context"
	"testing"
)

// TestManagerUsernsMode verifies that the manager computes AgentSpec.DropAllCaps from
// UsernsMode (via CapPolicyForUsernsMode), and that RemapBase() returns the configured value.
func TestManagerUsernsMode(t *testing.T) {
	cases := []struct {
		name            string
		usernsMode      string
		remapBase       uint32
		wantDropAllCaps bool
	}{
		{"off (default)", "off", 0, true},
		{"empty (default)", "", 0, true},
		{"remap", "remap", 700000, false},
		{"native", "native", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fb := &fakePodBackend{}
			m := NewManagerWithBackend(fb, &fakeApplier{}, ManagerConfig{
				AgentImage:      "a",
				SidecarImage:    "s",
				DataRoot:        t.TempDir(),
				EgressEnforce:   true,
				UsernsMode:      tc.usernsMode,
				UsernsRemapBase: tc.remapBase,
			})

			_, err := m.Create(context.Background(), "sp1", writeApp(t), "model", "", "", 0)
			if err != nil {
				t.Fatalf("Create: %v", err)
			}

			// AgentSpec.DropAllCaps must reflect the UsernsMode-derived CapPolicy.
			if fb.agentSpec.DropAllCaps != tc.wantDropAllCaps {
				t.Errorf("AgentSpec.DropAllCaps = %v, want %v (UsernsMode=%q)",
					fb.agentSpec.DropAllCaps, tc.wantDropAllCaps, tc.usernsMode)
			}

			// RemapBase must return the configured base.
			if m.RemapBase() != tc.remapBase {
				t.Errorf("RemapBase() = %d, want %d", m.RemapBase(), tc.remapBase)
			}
		})
	}
}

// TestManagerRemapBaseGetter verifies the getter in isolation.
func TestManagerRemapBaseGetter(t *testing.T) {
	m := NewManagerWithBackend(&fakePodBackend{}, &fakeApplier{}, ManagerConfig{
		AgentImage:      "a",
		SidecarImage:    "s",
		DataRoot:        t.TempDir(),
		UsernsRemapBase: 100000,
	})
	if got := m.RemapBase(); got != 100000 {
		t.Fatalf("RemapBase() = %d, want 100000", got)
	}
}

// Ensure the default (no UsernsMode set) still produces DropAllCaps=true (backward compat).
func TestManagerUsernsDefault_DropAllCapsPreserved(t *testing.T) {
	fb := &fakePodBackend{}
	m := NewManagerWithBackend(fb, &fakeApplier{}, ManagerConfig{
		AgentImage:    "a",
		SidecarImage:  "s",
		DataRoot:      t.TempDir(),
		EgressEnforce: true,
		// UsernsMode intentionally not set → defaults to "" → CapDropAll
	})
	_, err := m.Create(context.Background(), "sp2", writeApp(t), "model", "", "", 0)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !fb.agentSpec.DropAllCaps {
		t.Error("default config (no UsernsMode) must use DropAllCaps=true for backward compatibility")
	}
}

