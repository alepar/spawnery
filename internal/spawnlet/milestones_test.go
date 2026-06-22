package spawnlet

import (
	"testing"
)

func TestApplicableMilestones(t *testing.T) {
	tests := []struct {
		name     string
		flags    ProvisionFlags
		wantKeys []string
	}{
		{
			name:  "fresh no-github tmux (minimal)",
			flags: ProvisionFlags{},
			wantKeys: []string{
				MilestoneAuthorize, MilestonePrepareMounts,
				MilestoneCreatePod, MilestonePullImage, MilestoneStartAgent,
			},
		},
		{
			name:  "await-ready (non-tmux ACP spawn)",
			flags: ProvisionFlags{AwaitReady: true},
			wantKeys: []string{
				MilestoneAuthorize, MilestonePrepareMounts,
				MilestoneCreatePod, MilestonePullImage, MilestoneStartAgent, MilestoneAwaitReady,
			},
		},
		{
			name:  "github mint",
			flags: ProvisionFlags{MintCredentials: true},
			wantKeys: []string{
				MilestoneAuthorize, MilestoneMintCredentials, MilestonePrepareMounts,
				MilestoneCreatePod, MilestonePullImage, MilestoneStartAgent,
			},
		},
		{
			name:  "restore-snapshot (journal or rootfs artifacts)",
			flags: ProvisionFlags{RestoreSnapshot: true},
			wantKeys: []string{
				MilestoneAuthorize, MilestonePrepareMounts, MilestoneRestoreSnapshot,
				MilestoneCreatePod, MilestonePullImage, MilestoneStartAgent,
			},
		},
		{
			name:  "setup-network (ghControl or egress)",
			flags: ProvisionFlags{SetupNetwork: true},
			wantKeys: []string{
				MilestoneAuthorize, MilestonePrepareMounts,
				MilestoneCreatePod, MilestoneSetupNetwork, MilestonePullImage, MilestoneStartAgent,
			},
		},
		{
			name:  "all flags set (full catalog)",
			flags: ProvisionFlags{MintCredentials: true, RestoreSnapshot: true, SetupNetwork: true, AwaitReady: true},
			wantKeys: []string{
				MilestoneAuthorize, MilestoneMintCredentials, MilestonePrepareMounts,
				MilestoneRestoreSnapshot, MilestoneCreatePod, MilestoneSetupNetwork,
				MilestonePullImage, MilestoneStartAgent, MilestoneAwaitReady,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ApplicableMilestones(tt.flags)
			if len(got) != len(tt.wantKeys) {
				t.Fatalf("ApplicableMilestones(%+v) len=%d, want %d; keys=%v",
					tt.flags, len(got), len(tt.wantKeys), keys(got))
			}
			for i, m := range got {
				if m.Key != tt.wantKeys[i] {
					t.Errorf("index %d: key=%q, want %q", i, m.Key, tt.wantKeys[i])
				}
				if m.Label == "" {
					t.Errorf("index %d (key=%q): empty label", i, m.Key)
				}
			}
			// StepTotal (N) must equal the number of applicable milestones.
			if wantN := len(tt.wantKeys); len(got) != wantN {
				t.Errorf("N=%d, want %d", len(got), wantN)
			}
		})
	}
}

func TestFindMilestone(t *testing.T) {
	steps := ApplicableMilestones(ProvisionFlags{MintCredentials: true, AwaitReady: true})

	t.Run("found first", func(t *testing.T) {
		idx, m, ok := FindMilestone(steps, MilestoneAuthorize)
		if !ok || idx != 1 || m.Key != MilestoneAuthorize {
			t.Errorf("FindMilestone(authorize) = (%d, %v, %v), want (1, authorize, true)", idx, m.Key, ok)
		}
	})

	t.Run("found last", func(t *testing.T) {
		idx, m, ok := FindMilestone(steps, MilestoneAwaitReady)
		if !ok || idx != len(steps) || m.Key != MilestoneAwaitReady {
			t.Errorf("FindMilestone(await-ready) = (%d, %v, %v), want (%d, await-ready, true)", idx, m.Key, ok, len(steps))
		}
	})

	t.Run("found middle 1-based", func(t *testing.T) {
		// MintCredentials is at index 1 in the full slice (0-based), so 1-based = 2.
		idx, m, ok := FindMilestone(steps, MilestoneMintCredentials)
		if !ok || idx != 2 || m.Key != MilestoneMintCredentials {
			t.Errorf("FindMilestone(mint-credentials) = (%d, %v, %v), want (2, mint-credentials, true)", idx, m.Key, ok)
		}
	})

	t.Run("not found", func(t *testing.T) {
		idx, m, ok := FindMilestone(steps, "nonexistent-key")
		if ok || idx != 0 || m.Key != "" {
			t.Errorf("FindMilestone(nonexistent) = (%d, %v, %v), want (0, '', false)", idx, m.Key, ok)
		}
	})
}

func keys(ms []Milestone) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.Key
	}
	return out
}
