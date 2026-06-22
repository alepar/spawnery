package main

import (
	"testing"

	cpv1 "spawnery/gen/cp/v1"
)

func TestProvisionProgress(t *testing.T) {
	tests := []struct {
		name string
		s    *cpv1.SpawnSummary
		want string
	}{
		{
			name: "in-progress",
			s:    &cpv1.SpawnSummary{ProvisionStep: 3, ProvisionTotal: 9, ProvisionStepLabel: "create-pod"},
			want: "[3/9] create-pod",
		},
		{
			name: "zero total means no progress",
			s:    &cpv1.SpawnSummary{ProvisionStep: 1, ProvisionTotal: 0},
			want: "",
		},
		{
			name: "empty summary",
			s:    &cpv1.SpawnSummary{},
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := provisionProgress(tt.s)
			if got != tt.want {
				t.Errorf("provisionProgress = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestProvisionFailure(t *testing.T) {
	tests := []struct {
		name string
		s    *cpv1.SpawnSummary
		want string
	}{
		{
			name: "step and detail",
			s:    &cpv1.SpawnSummary{ErrorStep: "create-pod", ErrorDetail: "403 [accepted-permissions=administration=write]"},
			want: "✗ failed at create-pod: 403 [accepted-permissions=administration=write]",
		},
		{
			name: "empty step fallback",
			s:    &cpv1.SpawnSummary{ErrorDetail: "something went wrong"},
			want: "✗ failed: something went wrong",
		},
		{
			name: "step only no detail",
			s:    &cpv1.SpawnSummary{ErrorStep: "authorize"},
			want: "✗ failed at authorize",
		},
		{
			name: "both empty",
			s:    &cpv1.SpawnSummary{},
			want: "✗ failed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := provisionFailure(tt.s)
			if got != tt.want {
				t.Errorf("provisionFailure = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSpawnStatusErrorStep(t *testing.T) {
	tests := []struct {
		name string
		s    *cpv1.SpawnSummary
		want string
	}{
		{
			name: "error with step",
			s:    &cpv1.SpawnSummary{Status: cpv1.SpawnStatus_SPAWN_STATUS_ERROR, ErrorStep: "authorize"},
			want: "ERROR:authorize",
		},
		{
			name: "error no step",
			s:    &cpv1.SpawnSummary{Status: cpv1.SpawnStatus_SPAWN_STATUS_ERROR},
			want: "ERROR",
		},
		{
			name: "transition phase takes precedence over error step",
			s: &cpv1.SpawnSummary{
				Status:          cpv1.SpawnStatus_SPAWN_STATUS_ERROR,
				TransitionPhase: "cleanup",
				ErrorStep:       "authorize",
			},
			want: "ERROR:cleanup",
		},
		{
			name: "active",
			s:    &cpv1.SpawnSummary{Status: cpv1.SpawnStatus_SPAWN_STATUS_ACTIVE},
			want: "ACTIVE",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := spawnStatus(tt.s)
			if got != tt.want {
				t.Errorf("spawnStatus = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNextProgressLine(t *testing.T) {
	s := &cpv1.SpawnSummary{ProvisionStep: 3, ProvisionTotal: 9, ProvisionStepLabel: "create-pod"}

	// First call with empty prev: new line, changed.
	line, changed := nextProgressLine("", s)
	if !changed || line != "[3/9] create-pod" {
		t.Errorf("first call: line=%q changed=%v, want %q true", line, changed, "[3/9] create-pod")
	}

	// Second call with same prev: deduped, not changed.
	_, changed2 := nextProgressLine("[3/9] create-pod", s)
	if changed2 {
		t.Errorf("repeated identical step: changed=true, want false (dedup)")
	}

	// Zero total: no progress line, no change.
	sEmpty := &cpv1.SpawnSummary{}
	line3, changed3 := nextProgressLine("", sEmpty)
	if changed3 || line3 != "" {
		t.Errorf("zero total: line=%q changed=%v, want '' false", line3, changed3)
	}
}
