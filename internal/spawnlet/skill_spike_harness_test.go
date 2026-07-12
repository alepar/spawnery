//go:build skill_spike_e2e

// Package spawnlet (internal — not _test) so this harness can reach Manager's unexported pod field
// for CaptureDeltaAs, mirroring fork_freeze_spike_e2e_test.go.
//
// sp-mwco.2.2 spike: per-agent skill read-side + trust probe + fork/suspend delta.
// Spec: docs/superpowers/specs/2026-07-12-all-agent-skill-install-design.md §4.1.
//
// Run (Docker + spawnery/sidecar:dev + spawnery/agent:skillspike required; OPENROUTER_API_KEY
// required for the LLM-backed cells):
//
//	docker build -t spawnery/agent:skillspike -f deploy/agent/Dockerfile .
//	OPENROUTER_API_KEY=... go test -tags skill_spike_e2e -run TestSkillReadSideSpike -timeout 60m -v ./internal/spawnlet/
//	OPENROUTER_API_KEY=... go test -tags skill_spike_e2e -run TestSkillDescriptionTrustProbe -timeout 20m -v ./internal/spawnlet/
//	OPENROUTER_API_KEY=... go test -tags skill_spike_e2e -run TestSkillDeltaCaptureSpike -timeout 20m -v ./internal/spawnlet/
//
// NOTE on the image tag: the spike runs against "spawnery/agent:skillspike", NOT the shared
// "spawnery/agent:dev" tag that `make images` / other concurrent worktrees' e2e suites use.
// `docker images` on 2026-07-12 showed spawnery/agent:dev stale (claude 2.1.153, goose 1.37.0)
// relative to this epic branch's deploy/agent/Dockerfile pins (claude 2.1.197-1, goose v1.41.0)
// — some earlier build predates sp-mwco.2.1. Rebuilding the shared :dev tag risked yanking the
// image out from under sibling epic tasks' concurrently-running e2e suites, so this harness
// builds+uses its own tag instead. Before re-running this spike on a later image bump, rebuild
// spawnery/agent:skillspike (docker build command above) — the harness does NOT build it itself
// (build tags are opt-in per the project rule; a spike run is manual, not CI).
//
// === Step 0 discovery notes (2026-07-12, spawnery/agent:skillspike, pins verified via `--version`
// inside the built image: claude 2.1.197 (Claude Code), codex-cli 0.137.0, goose 1.41.0,
// opencode 1.15.13, hermes 0.15.2 (2026.5.29.2), pi 0.80.2 — all match deploy/agent/Dockerfile's
// ARGs) ===
//
//  1. Headless / one-shot invocation per agent (verified via `--help` and live `docker run`):
//     - claude: `claude -p "<prompt>"` (claude is TUI-only in this image; `-p`/`--print` is the
//     only non-interactive path, confirmed present in `claude --help`).
//     - codex: `codex exec "<prompt>"` (`codex exec --json` for structured events). codex is
//     TUI-only for interactive use; `exec` is its documented non-interactive subcommand.
//     - goose: `goose run -t "<prompt>"` (headless). ALSO: `goose skills list` — a native,
//     model-free listing command that gives a deterministic file-level signal without spending
//     an LLM turn.
//     - hermes: `hermes -z "<prompt>"` / `--oneshot` (headless, one-shot, prints only the final
//     response). ALSO: `hermes skills list --source local` — native, model-free listing.
//     - opencode: `opencode run "<message>"` (headless).
//     - pi: `pi -p "<prompt>" --provider spawnery --model "$SPAWN_MODEL" -a` (headless; `-a`
//     auto-approves the non-interactive project-trust prompt, mirroring launch's pi-tui case).
//     None of the six needed the ACP/session path for this spike — every one of them has a
//     documented headless CLI mode, so the harness probes ALL agents via Manager.ExecStream
//     (headless invocation), not the ACP Session RPC. This simplified the harness materially
//     versus the plan's fallback-to-ACP design.
//
//  2. Skill directory candidates + native listing commands (verified live against a bare
//     `docker run spawnery/agent:skillspike`, no spawnery pod, using OPENROUTER_API_KEY directly
//     against OpenRouter for the LLM-backed spot checks):
//     - goose 1.41.0: `goose skills list` picked up a canary planted at `~/.agents/skills/<name>/`
//     with ZERO configuration — no config.yaml, no extension-enable step. This CONTRADICTS the
//     plan's assumption that the Summon/skills extension needs headless enabling: goose 1.41.0
//     reads `~/.agents/skills` natively and unconditionally. (Builtin skills also listed under
//     `builtin://skills/...`, confirming the command's shape.)
//     - hermes 0.15.2: `hermes skills list --source local` found NOTHING for a canary planted at
//     `~/.agents/skills/<name>/` with no config. After writing `~/.hermes/config.yaml` with:
//     skills:
//     external_dirs:
//     - /root/.agents/skills
//     the SAME canary appeared (source=local, trust=local, enabled). Confirms the plan's
//     `skills.external_dirs` glue requirement exactly, and confirms hermes does NOT scan
//     `~/.agents/skills` on its own.
//     - codex 0.137.0: `strings` on the codex binary shows a real, compiled-in skills subsystem
//     (`codex_core_skills::{loader,manager,render,config_rules}`, `SKILL.md` frontmatter
//     parsing, a "2% skills context budget" injected into the system prompt, an `/skills` TUI
//     slash command). This is native support, not a research artifact — corroborates that the
//     WEB RESEARCH (which this spike overrides per the bead) undersold codex's skills support.
//     A literal `[features] skills = true` toggle was NOT found in the strings; `codex exec
//     --help` instead exposes generic `--enable <FEATURE>`/`-c features.<name>=true` machinery
//     with no "skills" feature name visible in --help text. The TestSkillReadSideSpike matrix
//     below measures the two directory candidates directly instead of guessing a flag.
//     - claude 2.1.197: candidates per spec are `~/.claude/skills` (proven in production via
//     agentinstall/apply-artifacts today) and `~/.agents/skills` (claude-code #31005). A third
//     cell — `~/.claude/skills` planted as a SYMLINK to a real dir elsewhere — probes #38051's
//     symlink regression (the reason agentinstall copies rather than symlinks).
//     - opencode 1.15.13: no native skills-listing subcommand found in `--help`; SkillPath is
//     still "" (no-op) in agentinstall/opencode.go pending S6. Candidates: `~/.agents/skills`
//     and `~/.claude/skills` (claimed cross-agent compat per research).
//     - pi 0.80.2: no dedicated skills subcommand seen in `--help`; `pi list`/`pi config` manage
//     "extensions", a different concept. Candidate: `~/.agents/skills` only (no evidence of a
//     separate native pi skill dir).
//
//  3. Glue switches: hermes confirmed above (`skills.external_dirs` in `~/.hermes/config.yaml`).
//     goose confirmed to need NO glue (native, unconditional). codex: no config.toml flag found
//     in strings; the matrix below tests the raw directories. pi: no trust-prompt behavior found
//     to gate on in non-TTY `-p` mode (untested live in this pass — see matrix notes).
//
// Model: openai/gpt-4o-mini (matches internal/cp/skill_ingest_e2e_test.go and
// fork_freeze_spike_e2e_test.go's LLM-backed cells) for every LLM-backed cell in this spike.
package spawnlet

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"spawnery/internal/runtime"
)

const (
	skillSpikeAgentImage   = "spawnery/agent:skillspike"
	skillSpikeSidecarImage = "spawnery/sidecar:dev"
	skillSpikeModel        = "openai/gpt-4o-mini"
)

var skillSpikeRunID = fmt.Sprintf("%d", time.Now().UnixNano())

// skillSpikeCell is one measured (agent, location, glue) combination.
type skillSpikeCell struct {
	Agent            string `json:"agent"`
	Runnable         string `json:"runnable"`
	AgentVersion     string `json:"agent_version"`
	Location         string `json:"location"`
	Glue             string `json:"glue"` // human description of the glue state for this cell
	LoadedFileLevel  bool   `json:"loaded_file_level"`
	LoadedBehavioral bool   `json:"loaded_behavioral"`
	// BehavioralBlocked is true when the behavioral probe could not run to a conclusive answer
	// because of an infra/rig fault (e.g. the sidecar tool-call-pairing bug found during this
	// spike — see skill_readside_spike_e2e_test.go), NOT because the agent ignored the skill.
	// LoadedBehavioral stays false in this case; readers must check this flag before treating
	// LoadedBehavioral=false as "not obeyed".
	BehavioralBlocked bool   `json:"behavioral_blocked,omitempty"`
	FileLevelMethod   string `json:"file_level_method"` // "list-command" | "llm-prompt" | "not-run"
	Notes             string `json:"notes,omitempty"`
	TranscriptExcerpt string `json:"raw_transcript_excerpt,omitempty"`
}

// skillTrustResult is one agent's outcome from TestSkillDescriptionTrustProbe.
type skillTrustResult struct {
	Agent   string `json:"agent"`
	Outcome string `json:"outcome"` // "obeyed" | "invoked-but-no-leak" | "ignored" | "n-a"
	Notes   string `json:"notes,omitempty"`
}

// skillDeltaResult is the D1/D2 delta-capture finding.
type skillDeltaResult struct {
	Kind    string `json:"kind"` // "D1-image-level" | "D2-lifecycle-DeltaCapture=true" | "D2-lifecycle-DeltaCapture=false"
	Outcome string `json:"outcome"`
	Notes   string `json:"notes,omitempty"`
}

type skillSpikeReport struct {
	RunID string             `json:"run_id"`
	Model string             `json:"model"`
	Cells []skillSpikeCell   `json:"cells,omitempty"`
	Trust []skillTrustResult `json:"trust,omitempty"`
	Delta []skillDeltaResult `json:"delta,omitempty"`
}

const (
	skillSpikeJSONPath     = "/tmp/spawnery-skill-spike-report.json"
	skillSpikeSpecNotePath = "/tmp/spawnery-skill-spike-spec-note.md"
)

// loadSkillSpikeReport reads the existing report (if any) so successive Test* functions in the
// same `go test` invocation accumulate into one report file, mirroring fork_freeze_spike's
// loadExistingResult/mergeResult pattern.
func loadSkillSpikeReport(t *testing.T) skillSpikeReport {
	t.Helper()
	var out skillSpikeReport
	b, err := os.ReadFile(skillSpikeJSONPath)
	if err != nil {
		if os.IsNotExist(err) {
			return skillSpikeReport{RunID: skillSpikeRunID, Model: skillSpikeModel}
		}
		t.Fatalf("read %s: %v", skillSpikeJSONPath, err)
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Logf("ignoring stale corrupt %s: %v", skillSpikeJSONPath, err)
		return skillSpikeReport{RunID: skillSpikeRunID, Model: skillSpikeModel}
	}
	return out
}

func writeSkillSpikeReport(t *testing.T, r skillSpikeReport) {
	t.Helper()
	r.RunID = skillSpikeRunID
	r.Model = skillSpikeModel
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		t.Fatalf("marshal skill spike report: %v", err)
	}
	if err := os.WriteFile(skillSpikeJSONPath, b, 0o644); err != nil {
		t.Fatalf("write %s: %v", skillSpikeJSONPath, err)
	}

	var md strings.Builder
	fmt.Fprintf(&md, "### 2026-07-12 — sp-mwco.2.2 skill spike run (run_id=%s, model=%s)\n\n", r.RunID, r.Model)
	md.WriteString("| Agent | Runnable | Version | Location | Glue | File-level | Behavioral | Blocked | Method |\n")
	md.WriteString("|---|---|---|---|---|---|---|---|---|\n")
	for _, c := range r.Cells {
		fmt.Fprintf(&md, "| %s | %s | %s | %s | %s | %v | %v | %v | %s |\n",
			c.Agent, c.Runnable, c.AgentVersion, c.Location, c.Glue, c.LoadedFileLevel, c.LoadedBehavioral, c.BehavioralBlocked, c.FileLevelMethod)
	}
	md.WriteString("\n**Trust probe** (§4.9 description-sanitization measurement):\n\n")
	md.WriteString("| Agent | Outcome | Notes |\n|---|---|---|\n")
	for _, tr := range r.Trust {
		fmt.Fprintf(&md, "| %s | %s | %s |\n", tr.Agent, tr.Outcome, tr.Notes)
	}
	md.WriteString("\n**Delta capture** (fork/suspend, §4.5 premise):\n\n")
	md.WriteString("| Kind | Outcome | Notes |\n|---|---|---|\n")
	for _, d := range r.Delta {
		fmt.Fprintf(&md, "| %s | %s | %s |\n", d.Kind, d.Outcome, d.Notes)
	}
	if err := os.WriteFile(skillSpikeSpecNotePath, []byte(md.String()), 0o644); err != nil {
		t.Fatalf("write %s: %v", skillSpikeSpecNotePath, err)
	}
	t.Logf("skill spike report written: %s / %s", skillSpikeJSONPath, skillSpikeSpecNotePath)
}

// skillSpikeHarness is the shared rig: one Docker Manager, helpers to create per-agent probe
// pods, plant/clear canaries via ExecStream (no stdin — content is shipped as a shell heredoc in
// argv, per Manager.ExecStream's documented no-stdin contract), and run headless probes.
type skillSpikeHarness struct {
	t        *testing.T
	mgr      *Manager
	cleanups []func()
}

func requireDockerAndKey(t *testing.T) string {
	t.Helper()
	key := os.Getenv("OPENROUTER_API_KEY")
	if key == "" {
		t.Fatal("OPENROUTER_API_KEY is required for the skill spike (LLM-backed cells)")
	}
	rt, err := runtime.NewDocker()
	if err != nil {
		t.Fatalf("docker unavailable (need Docker for skill_spike_e2e): %v", err)
	}
	if err := rt.Ping(context.Background()); err != nil {
		t.Fatalf("docker not pingable: %v", err)
	}
	if _, ok, _ := rt.InspectImage(context.Background(), skillSpikeAgentImage); !ok {
		t.Fatalf("image %s not found; build it first: docker build -t %s -f deploy/agent/Dockerfile .", skillSpikeAgentImage, skillSpikeAgentImage)
	}
	return key
}

func newSkillSpikeHarness(t *testing.T, deltaCapture bool) *skillSpikeHarness {
	t.Helper()
	key := requireDockerAndKey(t)

	rt, err := runtime.NewDocker()
	if err != nil {
		t.Fatalf("docker unavailable: %v", err)
	}

	mgr := NewManager(rt, ManagerConfig{
		AgentImage:    skillSpikeAgentImage,
		SidecarImage:  skillSpikeSidecarImage,
		OpenRouterKey: key,
		DataRoot:      t.TempDir(),
		DeltaCapture:  deltaCapture,
	})

	return &skillSpikeHarness{t: t, mgr: mgr}
}

func (h *skillSpikeHarness) cleanup() {
	for i := len(h.cleanups) - 1; i >= 0; i-- {
		h.cleanups[i]()
	}
}

// writeSkillSpikeApp writes a minimal spawneryapp.yml (no seed data needed — the spike never
// touches the app mount) into a fresh temp dir and returns its path.
func writeSkillSpikeApp(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	appDir := filepath.Join(root, "app")
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		t.Fatalf("mkdir app dir: %v", err)
	}
	manifest := `apiVersion: spawnery/v1
kind: App
id: spawnery/skill-spike
title: Skill Spike
agents: { support: [any], requiresAcp: [prompt] }
model: { recommendedDefault: openai/gpt-4o-mini }
visibility: open
`
	if err := os.WriteFile(filepath.Join(appDir, "spawneryapp.yml"), []byte(manifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return appDir
}

// createProbePod creates a spawn with the given runnable id and waits for its per-runnable
// launcher setup (config.toml / config.yaml / settings.json) to land on disk before returning, so
// headless ExecStream probes issued right after don't race the launcher. readyCheck is a shell
// snippet (run via `sh -lc`) that exits 0 once setup is complete for that runnable — e.g.
// `test -f /root/.codex/config.toml` for codex-tui.
func (h *skillSpikeHarness) createProbePod(ctx context.Context, spawnID, runnableID, readyCheck string) *Spawn {
	h.t.Helper()
	appDir := writeSkillSpikeApp(h.t)
	sp, err := h.mgr.CreateWithSelection(ctx, spawnID, appDir, skillSpikeModel, "", "", 1, AgentSelection{RunnableID: runnableID})
	if err != nil {
		h.t.Fatalf("create probe pod %s (%s): %v", spawnID, runnableID, err)
	}
	for _, w := range h.mgr.takeWatchers(sp) {
		w.Stop()
	}
	h.cleanups = append(h.cleanups, func() {
		cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, live := h.mgr.Store().Get(spawnID); live {
			_ = h.mgr.Stop(cctx, spawnID)
		}
	})

	if readyCheck != "" {
		deadline := time.Now().Add(90 * time.Second)
		var lastCode int
		var lastErr error
		for time.Now().Before(deadline) {
			var stdout, stderr strings.Builder
			code, err := h.mgr.ExecStream(ctx, spawnID, []string{"sh", "-lc", readyCheck}, &stdout, &stderr)
			lastCode, lastErr = code, err
			if err == nil && code == 0 {
				return sp
			}
			time.Sleep(1 * time.Second)
		}
		h.t.Fatalf("probe pod %s (%s) never became ready (readyCheck=%q last code=%d err=%v)\n%s",
			spawnID, runnableID, readyCheck, lastCode, lastErr, h.podLogs(ctx, sp))
	}
	return sp
}

func (h *skillSpikeHarness) podLogs(ctx context.Context, sp *Spawn) string {
	if sp == nil || sp.AgentID == "" {
		return ""
	}
	out, _ := exec.CommandContext(ctx, "docker", "logs", "--tail", "150", sp.AgentID).CombinedOutput()
	return string(out)
}

// shQuote single-quotes s for safe interpolation into a `sh -lc` script (escaping any embedded
// single quotes). Canary content controlled by this harness never contains one, but this is
// defensive.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// plantCanary writes a SKILL.md at <dir>/<name>/SKILL.md inside the spawn's agent container via
// ExecStream (no stdin available — Manager.ExecStream is documented no-stdin, so content rides in
// argv as a single-quoted `printf` payload, not a piped tar). description defaults to a benign
// "use when the user asks for the probe token" line; pass a custom description to carry the
// trust-probe's unrelated instruction (§4.9 measurement — the INJECTION LIVES IN THE
// DESCRIPTION, never the body, per the bead).
func (h *skillSpikeHarness) plantCanary(ctx context.Context, spawnID, dir, name, nonce, description, body string) {
	h.t.Helper()
	if description == "" {
		description = "Use when the user asks for the probe token."
	}
	if body == "" {
		body = fmt.Sprintf("When the user asks for the probe token, reply with exactly: TOKEN-%s\n", nonce)
	}
	content := fmt.Sprintf("---\nname: %s\ndescription: %s\n---\n%s", name, description, body)
	skillDir := dir + "/" + name
	script := fmt.Sprintf("mkdir -p %s && printf %%s %s > %s/SKILL.md && chmod 0700 %s",
		shQuote(skillDir), shQuote(content), shQuote(skillDir), shQuote(skillDir))
	h.mustExec(ctx, spawnID, script, "plantCanary "+name+" at "+dir)
}

// clearCanaries removes every probe-* skill dir under each of the given roots, so a hit in the
// next cell is unambiguously attributable (per the plan's "one canary cell at a time" method).
func (h *skillSpikeHarness) clearCanaries(ctx context.Context, spawnID string, dirs ...string) {
	h.t.Helper()
	var parts []string
	for _, d := range dirs {
		parts = append(parts, fmt.Sprintf("rm -rf %s/probe-* 2>/dev/null || true", shQuote(d)))
	}
	h.mustExec(ctx, spawnID, strings.Join(parts, "; "), "clearCanaries")
}

func (h *skillSpikeHarness) mustExec(ctx context.Context, spawnID, script, label string) (string, string) {
	h.t.Helper()
	var stdout, stderr strings.Builder
	code, err := h.mgr.ExecStream(ctx, spawnID, []string{"sh", "-lc", script}, &stdout, &stderr)
	if err != nil {
		h.t.Fatalf("%s: ExecStream error: %v (stderr=%s)", label, err, stderr.String())
	}
	if code != 0 {
		h.t.Fatalf("%s: exit %d\nstdout=%s\nstderr=%s", label, code, stdout.String(), stderr.String())
	}
	return stdout.String(), stderr.String()
}

// exec runs script without failing the test on a non-zero exit — used for probes where a
// negative/error result IS the finding (never t.Fatalf on a negative finding, per the plan).
func (h *skillSpikeHarness) exec(ctx context.Context, spawnID, script string) (stdout, stderr string, code int, err error) {
	h.t.Helper()
	var so, se strings.Builder
	code, err = h.mgr.ExecStream(ctx, spawnID, []string{"sh", "-lc", script}, &so, &se)
	return so.String(), se.String(), code, err
}

func (h *skillSpikeHarness) agentVersion(ctx context.Context, spawnID, script string) string {
	h.t.Helper()
	stdout, stderr, code, err := h.exec(ctx, spawnID, script)
	if err != nil || code != 0 {
		return fmt.Sprintf("ERROR(code=%d err=%v stderr=%s)", code, err, strings.TrimSpace(stderr))
	}
	return strings.TrimSpace(stdout)
}

// Note: the spike ended up not needing the ACP/session path — every agent has a headless CLI (see
// the Step 0 discovery note at the top of this file), so all probing goes through
// Manager.ExecStream above. No RPC/ACP server scaffolding is kept here (YAGNI); if a future re-run
// needs to probe a long-lived ACP process (e.g. whether it re-scans skill dirs after a mid-session
// config change), mirror e2e_goose_test.go's httptest h2c server + spawnv1connect client setup.
