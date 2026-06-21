package config

import (
	"strings"
	"testing"
)

func envFrom(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func TestResolveEnv_DefaultEnvWhenUnset(t *testing.T) {
	// A client CLI passes a default env so it is not fail-closed like a server.
	got, err := resolveEnv(nil, envFrom(nil), "dev")
	if err != nil || got != "dev" {
		t.Fatalf("resolveEnv(default dev) = %q, %v; want dev", got, err)
	}
	// An explicit --env still wins over the default.
	if got, _ := resolveEnv([]string{"--env=prod"}, envFrom(nil), "dev"); got != "prod" {
		t.Errorf("explicit --env should win over default, got %q", got)
	}
	// An invalid default is still validated.
	if _, err := resolveEnv(nil, envFrom(nil), "bogus"); err == nil {
		t.Error("invalid default env should error")
	}
}

func TestResolveEnv_EmptyEnvFlagDoesNotFallThrough(t *testing.T) {
	// --env= (e.g. an unset shell var expansion) must be treated as an explicit invalid value,
	// not silently fall through to a stale SPAWNERY_ENV.
	if _, err := resolveEnv([]string{"--env="}, envFrom(map[string]string{"SPAWNERY_ENV": "prod"}), ""); err == nil {
		t.Fatal("--env= with SPAWNERY_ENV=prod should error, not return prod")
	}
	if _, err := resolveEnv([]string{"--env"}, envFrom(map[string]string{"SPAWNERY_ENV": "prod"}), ""); err == nil {
		t.Fatal("trailing --env (no value) should error, not fall through")
	}
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
			got, err := resolveEnv(tc.args, envFrom(tc.env), "")
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
