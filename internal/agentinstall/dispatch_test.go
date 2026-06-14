package agentinstall_test

import (
	"os"
	"path/filepath"
	"testing"

	"spawnery/internal/agentinstall"
)

func TestDispatchExplicitTarget(t *testing.T) {
	home := t.TempDir()
	env := agentinstall.MapEnviron{"HOME": home}
	reg := agentinstall.NewRegistry(env)

	m := agentinstall.Manifest{
		Artifacts: []agentinstall.Artifact{
			{
				Kind:    agentinstall.KindSkill,
				Name:    "my-skill",
				Targets: []string{"claude"},
				Skill:   &agentinstall.SkillPayload{Dir: "payloads/my-skill"},
			},
		},
	}
	opts := agentinstall.Options{HomeDir: home}
	result := agentinstall.Apply(reg, m, opts, env)

	if len(result.Reports) != 1 {
		t.Fatalf("expected 1 report, got %d", len(result.Reports))
	}
	r := result.Reports[0]
	if r.Agent != "claude" {
		t.Errorf("agent: got %q, want %q", r.Agent, "claude")
	}
	if r.Kind != agentinstall.KindSkill {
		t.Errorf("kind: got %q, want %q", r.Kind, agentinstall.KindSkill)
	}
	if r.Name != "my-skill" {
		t.Errorf("name: got %q, want %q", r.Name, "my-skill")
	}
	// Base placeholder returns skipped
	if r.Status != agentinstall.StatusSkipped {
		t.Errorf("status: got %q, want %q", r.Status, agentinstall.StatusSkipped)
	}
}

func TestDispatchUnknownAgent(t *testing.T) {
	home := t.TempDir()
	env := agentinstall.MapEnviron{"HOME": home}
	reg := agentinstall.NewRegistry(env)

	m := agentinstall.Manifest{
		Artifacts: []agentinstall.Artifact{
			{
				Kind:    agentinstall.KindSkill,
				Name:    "test-skill",
				Targets: []string{"unknown-agent", "claude"},
				Skill:   &agentinstall.SkillPayload{Dir: "payloads/test-skill"},
			},
		},
	}
	result := agentinstall.Apply(reg, m, agentinstall.Options{HomeDir: home}, env)

	if len(result.Reports) != 2 {
		t.Fatalf("expected 2 reports, got %d", len(result.Reports))
	}

	// First report: unknown-agent → skipped
	r0 := result.Reports[0]
	if r0.Agent != "unknown-agent" {
		t.Errorf("report[0].Agent: got %q, want %q", r0.Agent, "unknown-agent")
	}
	if r0.Status != agentinstall.StatusSkipped {
		t.Errorf("report[0].Status: got %q, want %q", r0.Status, agentinstall.StatusSkipped)
	}
	if r0.Reason != "unknown or unsupported agent" {
		t.Errorf("report[0].Reason: got %q", r0.Reason)
	}

	// Second report: claude → skipped (base placeholder)
	r1 := result.Reports[1]
	if r1.Agent != "claude" {
		t.Errorf("report[1].Agent: got %q, want %q", r1.Agent, "claude")
	}
	if r1.Status != agentinstall.StatusSkipped {
		t.Errorf("report[1].Status: got %q, want %q", r1.Status, agentinstall.StatusSkipped)
	}
}

func TestDispatchAllDetected(t *testing.T) {
	home := t.TempDir()
	// Create claude and codex config roots
	for _, d := range []string{".claude", ".codex"} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	env := agentinstall.MapEnviron{"HOME": home}
	reg := agentinstall.NewRegistry(env)

	m := agentinstall.Manifest{
		Artifacts: []agentinstall.Artifact{
			{
				Kind:    agentinstall.KindMCP,
				Name:    "my-mcp",
				Targets: []string{"all-detected"},
				MCP: &agentinstall.MCPPayload{
					Stdio: &agentinstall.MCPTransportStdio{
						Command: "node",
						Args:    []string{"server.js"},
					},
				},
			},
		},
	}
	result := agentinstall.Apply(reg, m, agentinstall.Options{HomeDir: home}, env)

	// Should have exactly 2 reports: claude + codex
	if len(result.Reports) != 2 {
		t.Fatalf("expected 2 reports (claude+codex), got %d: %+v", len(result.Reports), result.Reports)
	}
	agents := map[string]bool{}
	for _, r := range result.Reports {
		agents[r.Agent] = true
		if r.Kind != agentinstall.KindMCP {
			t.Errorf("report for %s: kind %q, want mcp", r.Agent, r.Kind)
		}
	}
	if !agents["claude"] || !agents["codex"] {
		t.Errorf("expected claude and codex, got %v", agents)
	}
}

func TestDispatchOpencodeSkillPermanentNoOp(t *testing.T) {
	home := t.TempDir()
	env := agentinstall.MapEnviron{"HOME": home}
	reg := agentinstall.NewRegistry(env)

	m := agentinstall.Manifest{
		Artifacts: []agentinstall.Artifact{
			{
				Kind:    agentinstall.KindSkill,
				Name:    "my-skill",
				Targets: []string{"opencode"},
				Skill:   &agentinstall.SkillPayload{Dir: "payloads/my-skill"},
			},
		},
	}
	result := agentinstall.Apply(reg, m, agentinstall.Options{HomeDir: home}, env)

	if len(result.Reports) != 1 {
		t.Fatalf("expected 1 report, got %d", len(result.Reports))
	}
	r := result.Reports[0]
	if r.Status != agentinstall.StatusSkipped {
		t.Errorf("expected skipped, got %q", r.Status)
	}
	if r.Reason != "opencode skills layout unconfirmed (S6)" {
		t.Errorf("unexpected reason: %q", r.Reason)
	}
}

func TestDispatchGooseSkillPermanentNoOp(t *testing.T) {
	home := t.TempDir()
	env := agentinstall.MapEnviron{"HOME": home}
	reg := agentinstall.NewRegistry(env)

	m := agentinstall.Manifest{
		Artifacts: []agentinstall.Artifact{
			{
				Kind:    agentinstall.KindSkill,
				Name:    "my-skill",
				Targets: []string{"goose"},
				Skill:   &agentinstall.SkillPayload{Dir: "payloads/my-skill"},
			},
		},
	}
	result := agentinstall.Apply(reg, m, agentinstall.Options{HomeDir: home}, env)

	if len(result.Reports) != 1 {
		t.Fatalf("expected 1 report, got %d", len(result.Reports))
	}
	r := result.Reports[0]
	if r.Status != agentinstall.StatusSkipped {
		t.Errorf("expected skipped, got %q", r.Status)
	}
	if r.Reason != "deferred" {
		t.Errorf("unexpected reason: %q", r.Reason)
	}
}

func TestDispatchHermesAllDeferred(t *testing.T) {
	home := t.TempDir()
	env := agentinstall.MapEnviron{"HOME": home}
	reg := agentinstall.NewRegistry(env)

	for _, kind := range []agentinstall.Kind{agentinstall.KindSkill, agentinstall.KindMCP, agentinstall.KindConfig} {
		var a agentinstall.Artifact
		a.Kind = kind
		a.Name = "test"
		a.Targets = []string{"hermes"}
		switch kind {
		case agentinstall.KindSkill:
			a.Skill = &agentinstall.SkillPayload{Dir: "payloads/test"}
		case agentinstall.KindMCP:
			a.MCP = &agentinstall.MCPPayload{Stdio: &agentinstall.MCPTransportStdio{Command: "node"}}
		case agentinstall.KindConfig:
			a.Config = &agentinstall.ConfigPayload{Normalized: map[string]interface{}{}}
		}

		m := agentinstall.Manifest{Artifacts: []agentinstall.Artifact{a}}
		result := agentinstall.Apply(reg, m, agentinstall.Options{HomeDir: home}, env)

		if len(result.Reports) != 1 {
			t.Fatalf("kind=%s: expected 1 report, got %d", kind, len(result.Reports))
		}
		r := result.Reports[0]
		if r.Status != agentinstall.StatusSkipped {
			t.Errorf("kind=%s: expected skipped, got %q", kind, r.Status)
		}
		if r.Reason != "deferred to sp-mofj" {
			t.Errorf("kind=%s: unexpected reason: %q", kind, r.Reason)
		}
	}
}

func TestDispatchBasePlaceholdersReturnSkipped(t *testing.T) {
	home := t.TempDir()
	env := agentinstall.MapEnviron{"HOME": home}
	reg := agentinstall.NewRegistry(env)

	// claude, codex, goose (mcp/config) all return skipped from base placeholder
	tests := []struct {
		agent string
		kind  agentinstall.Kind
	}{
		{"claude", agentinstall.KindMCP},
		{"claude", agentinstall.KindConfig},
		{"codex", agentinstall.KindSkill},
		{"codex", agentinstall.KindMCP},
		{"codex", agentinstall.KindConfig},
		{"opencode", agentinstall.KindMCP},
		{"opencode", agentinstall.KindConfig},
		{"goose", agentinstall.KindMCP},
		{"goose", agentinstall.KindConfig},
	}

	for _, tc := range tests {
		var a agentinstall.Artifact
		a.Kind = tc.kind
		a.Name = "test"
		a.Targets = []string{tc.agent}
		switch tc.kind {
		case agentinstall.KindSkill:
			a.Skill = &agentinstall.SkillPayload{Dir: "payloads/test"}
		case agentinstall.KindMCP:
			a.MCP = &agentinstall.MCPPayload{HTTP: &agentinstall.MCPTransportHTTP{URL: "http://localhost:8080"}}
		case agentinstall.KindConfig:
			a.Config = &agentinstall.ConfigPayload{Normalized: map[string]interface{}{}}
		}

		m := agentinstall.Manifest{Artifacts: []agentinstall.Artifact{a}}
		result := agentinstall.Apply(reg, m, agentinstall.Options{HomeDir: home}, env)

		if len(result.Reports) != 1 {
			t.Fatalf("agent=%s kind=%s: expected 1 report, got %d", tc.agent, tc.kind, len(result.Reports))
		}
		r := result.Reports[0]
		if r.Status != agentinstall.StatusSkipped {
			t.Errorf("agent=%s kind=%s: expected skipped, got %q (reason: %q)", tc.agent, tc.kind, r.Status, r.Reason)
		}
	}
}

func TestDispatchEmptyManifest(t *testing.T) {
	home := t.TempDir()
	env := agentinstall.MapEnviron{"HOME": home}
	reg := agentinstall.NewRegistry(env)

	m := agentinstall.Manifest{}
	result := agentinstall.Apply(reg, m, agentinstall.Options{HomeDir: home}, env)
	if len(result.Reports) != 0 {
		t.Errorf("expected empty reports, got %v", result.Reports)
	}
}

func TestDispatchMultipleArtifacts(t *testing.T) {
	home := t.TempDir()
	env := agentinstall.MapEnviron{"HOME": home}
	reg := agentinstall.NewRegistry(env)

	m := agentinstall.Manifest{
		Artifacts: []agentinstall.Artifact{
			{
				Kind:    agentinstall.KindSkill,
				Name:    "skill-a",
				Targets: []string{"claude"},
				Skill:   &agentinstall.SkillPayload{Dir: "payloads/skill-a"},
			},
			{
				Kind:    agentinstall.KindMCP,
				Name:    "mcp-b",
				Targets: []string{"codex"},
				MCP:     &agentinstall.MCPPayload{HTTP: &agentinstall.MCPTransportHTTP{URL: "http://localhost"}},
			},
		},
	}
	result := agentinstall.Apply(reg, m, agentinstall.Options{HomeDir: home}, env)

	if len(result.Reports) != 2 {
		t.Fatalf("expected 2 reports, got %d", len(result.Reports))
	}
}
