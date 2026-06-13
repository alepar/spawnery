package runtime

import (
	"strings"
	"testing"
)

func TestCapPolicyForUsernsMode(t *testing.T) {
	cases := []struct {
		mode string
		want CapPolicy
	}{
		{"remap", CapDefaultSet},
		{"native", CapDefaultSet},
		{"off", CapDropAll},
		{"", CapDropAll},
		{"unknown", CapDropAll},
		{"REMAP", CapDropAll}, // case-sensitive
	}
	for _, tc := range cases {
		got := CapPolicyForUsernsMode(tc.mode)
		if got != tc.want {
			t.Errorf("CapPolicyForUsernsMode(%q) = %v, want %v", tc.mode, got, tc.want)
		}
	}
}

func TestParseRemapBase(t *testing.T) {
	cases := []struct {
		rootDir  string
		wantBase uint32
		wantOK   bool
	}{
		{"/var/lib/docker/700000.700000", 700000, true},
		{"/var/lib/docker/1000.1000", 1000, true},
		{"/var/lib/docker/0.0", 0, true},
		// No dot suffix → not a remap path
		{"/var/lib/docker", 0, false},
		// Non-numeric UID
		{"/var/lib/docker/user.1000", 0, false},
		// Empty last segment edge case
		{"/", 0, false},
	}
	for _, tc := range cases {
		got, ok := parseRemapBase(tc.rootDir)
		if ok != tc.wantOK || got != tc.wantBase {
			t.Errorf("parseRemapBase(%q) = (%d, %v), want (%d, %v)",
				tc.rootDir, got, ok, tc.wantBase, tc.wantOK)
		}
	}
}

func TestAssertNoAddedCaps(t *testing.T) {
	// No CapAdd → no error.
	if err := assertNoAddedCaps(ContainerSpec{}); err != nil {
		t.Fatalf("expected nil for empty CapAdd, got %v", err)
	}
	if err := assertNoAddedCaps(ContainerSpec{CapAdd: nil}); err != nil {
		t.Fatalf("expected nil for nil CapAdd, got %v", err)
	}

	// Any CapAdd entry → error that mentions CAP_NET_ADMIN.
	err := assertNoAddedCaps(ContainerSpec{CapAdd: []string{"CAP_NET_ADMIN"}})
	if err == nil {
		t.Fatal("expected error when CapAdd is set, got nil")
	}
	if !strings.Contains(err.Error(), "CAP_NET_ADMIN") {
		t.Errorf("error should mention CAP_NET_ADMIN, got: %v", err)
	}

	// Multiple caps also rejected.
	err2 := assertNoAddedCaps(ContainerSpec{CapAdd: []string{"CAP_SYS_ADMIN", "CAP_NET_ADMIN"}})
	if err2 == nil {
		t.Fatal("expected error for multiple CapAdd entries, got nil")
	}
}
