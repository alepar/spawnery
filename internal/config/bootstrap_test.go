package config

import (
	"strings"
	"testing"
)

func envFrom(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func TestResolveEnv(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		env     map[string]string
		want    string
		wantErr string // substring expected in the error; "" means no error
	}{
		{
			name: "SPAWNERY_ENV is honored",
			env:  map[string]string{"SPAWNERY_ENV": "prod"},
			want: "prod",
		},
		{
			name: "--env=value overrides SPAWNERY_ENV",
			args: []string{"--env=staging"},
			env:  map[string]string{"SPAWNERY_ENV": "prod"},
			want: "staging",
		},
		{
			name: "--env value (space form) is honored",
			args: []string{"--env", "dev"},
			want: "dev",
		},
		{
			name:    "neither set is fatal",
			wantErr: "SPAWNERY_ENV",
		},
		{
			name:    "unknown SPAWNERY_ENV is fatal",
			env:     map[string]string{"SPAWNERY_ENV": "production"},
			wantErr: "production",
		},
		{
			name:    "unknown --env is fatal",
			args:    []string{"--env=prd"},
			wantErr: "prd",
		},
		{
			name:    "trailing whitespace is not silently trimmed (fatal)",
			env:     map[string]string{"SPAWNERY_ENV": "prod "},
			wantErr: "prod",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveEnv(tc.args, envFrom(tc.env))
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil (value %q)", tc.wantErr, got)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}
