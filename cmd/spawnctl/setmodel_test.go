package main

import "testing"

func TestFormatSetModelResult(t *testing.T) {
	cases := []struct {
		name    string
		model   string
		applied bool
		want    string
	}{
		{
			name:    "applied",
			model:   "anthropic/claude-3.5-sonnet",
			applied: true,
			want:    "model set to anthropic/claude-3.5-sonnet (applied)",
		},
		{
			name:    "pending",
			model:   "openai/gpt-4o",
			applied: false,
			want:    "model set to openai/gpt-4o (saved; pending — agent not yet switched)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := formatSetModelResult(tc.model, tc.applied)
			if got != tc.want {
				t.Fatalf("formatSetModelResult(%q, %v) = %q, want %q", tc.model, tc.applied, got, tc.want)
			}
		})
	}
}
