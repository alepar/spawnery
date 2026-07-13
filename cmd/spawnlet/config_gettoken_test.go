package main

import "testing"

// GETTOKEN_LISTEN_IP was READ by internal/spawnlet (ManagerConfig.GetTokenListenIP) and SET only by
// tests — cmd/spawnlet never mapped the env var, so in the shipped binary the field was always empty.
// The consequence was not a degraded feature but a dead one: on any non-userns-remap lane (notably
// CRI/runsc, the production lane) the GitHub control server fell back to binding a POD IP, which no host
// can bind, and every spawn died at setup-network with
//
//	github control server tcp 10.234.0.2:8082: listen: bind: cannot assign requested address
//
// It went unnoticed because the control server is only constructed when the AS mint lane is configured
// (internal/node/attach.go: "if cfg.GitHubMint != nil"), and no CRI lane ever had AS_URL set — so the TCP
// branch never executed in anger. The manager's own comment claimed "Set by cmd/spawnlet from
// GETTOKEN_LISTEN_IP"; cmd/spawnlet did not.
//
// This test pins the wiring end to end: the env var must reach the config field. A knob that silently
// does not exist is worse than no knob, because every operator who sets it believes it took effect.
func TestSpawnletConfig_GetTokenListenIPEnvAlias(t *testing.T) {
	cfg, err := loadSpawnletTest(t, "dev", map[string]string{
		"GETTOKEN_LISTEN_IP": "10.234.0.1",
	})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.GetTokenListenIP != "10.234.0.1" {
		t.Fatalf("GetTokenListenIP = %q, want 10.234.0.1 — the GETTOKEN_LISTEN_IP env alias is not wired, "+
			"so the GitHub control server's TCP lane cannot be configured and will bind an unbindable pod IP",
			cfg.GetTokenListenIP)
	}
}

// Absent by default: the userns-remap lane uses a bind-mounted UNIX socket and must not be handed a
// listen IP it would ignore, and production must not acquire a listener nobody asked for.
func TestSpawnletConfig_GetTokenListenIPDefaultsEmpty(t *testing.T) {
	cfg, err := loadSpawnletTest(t, "dev", nil)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.GetTokenListenIP != "" {
		t.Fatalf("GetTokenListenIP = %q, want empty by default", cfg.GetTokenListenIP)
	}
}
