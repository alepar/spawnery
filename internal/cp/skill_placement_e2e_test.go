//go:build e2e

package cp_test

// TestCPSkillPlacementMatrixE2E and TestCPSkillBundleAllOrNothingE2E are the sp-mwco.2.9
// acceptance e2e for the §4.8 per-agent skill placement matrix and the §4.5 all-or-nothing bundle
// contract. Spec: docs/superpowers/specs/2026-07-12-all-agent-skill-install-design.md §4.8/§4.5.
//
// Placement (all six agents) is proven with a model-free `docker exec … test -f`, derived from
// the production agentinstall registry via skill_paths_e2e_test.go's expectedSkillDirs — not
// hardcoded (bead ground truth 4). Invocation (the agent actually reads and obeys the skill) is
// proven behaviorally for TWO shared-dir agents (opencode, goose) via a probe skill whose body
// carries a per-run TOKEN-<nonce>; claude gets a file-level agent-visibility probe instead of a
// token-recital behavioral probe (see the sp-o5t3 note below); codex gets placement only (sp-9e6q).
//
// DEVIATION FROM THE STORED PLAN, RECORDED HERE: the plan's Step 4 preamble describes reusing the
// real-GitHub ingest path (IngestSkillFromURL) for the shared probe skill. This test instead uses
// a CUSTOM inline skill (built in-test, no network) as the probe for every assertion in the
// matrix. Two reasons: (1) the opencode/goose behavioral probes need a body carrying a per-run
// TOKEN-<nonce> — a static public GitHub repo's content cannot supply that — so SOME custom skill
// is unavoidable; (2) reusing that one custom skill for placement too avoids adding a second,
// flake-prone GitHub-network dependency on top of TestCPSkillIngestE2E, which already covers the
// URL-ingest path end-to-end. The profile/assembly/install pipeline this test exercises is
// identical either way (CUSTOM inline and CATALOG_REF both flow through the same
// assembleProfileArtifacts → manifest → apply-artifacts.sh → agentinstall path); only the skill's
// *provenance* differs, which the placement/invocation acceptance criteria do not constrain.
//
// Requires (same as skill_ingest_e2e_test.go, minus GITHUB_TOKEN — no GitHub fetch here):
//   - Docker + spawnery/agent:dev (built with agentinstall + apply-artifacts.sh) containing all
//     six agent binaries (claude, codex, opencode, goose, hermes, pi) + spawnery/sidecar:dev
//   - Garage running (just garage), dev-creds.env present at deploy/garage/dev-creds.env
//   - OPENROUTER_API_KEY in env or .env at repo root
//
// FAILS loudly (no skips) per the build-tag-is-opt-in rule when any dep is down.
//
// Budget: 6 sequential single-agent spawns (~150s boot each, worst case) + one Targets-negative
// spawn + the bundle all-or-nothing test's 2 spawns (~3m ERROR wait + ~150s ACTIVE control) ≈
// 25-40 minutes total; run with -timeout 60m.

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/klauspost/compress/zstd"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/gen/cp/v1/cpv1connect"
	"spawnery/internal/cp/store"
	"spawnery/internal/runtime"
)

// claudeToolCallPairingBugSignature is the sp-o5t3 error signature: the sidecar's
// Anthropic->OpenAI tool-call translation sometimes emits a malformed pairing when claude invokes
// its native Skill tool, which OpenRouter rejects with a 400 carrying this text. Deterministic
// (3/3) per the §4.1 spike — see internal/spawnlet/skill_readside_spike_e2e_test.go's
// sidecarToolCallPairingBugSignature (duplicated here rather than imported: that const lives in
// the skill_spike_e2e-tagged internal/spawnlet package, a different build tag and package).
const claudeToolCallPairingBugSignature = "must be followed by tool messages responding to each"

// buildSkillTar returns a minimal TAR archive with a single top-level SKILL.md whose body is body.
func buildSkillTar(t *testing.T, body string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	data := []byte(body)
	if err := tw.WriteHeader(&tar.Header{Name: "SKILL.md", Mode: 0o644, Size: int64(len(data))}); err != nil {
		t.Fatalf("buildSkillTar: WriteHeader: %v", err)
	}
	if _, err := tw.Write(data); err != nil {
		t.Fatalf("buildSkillTar: Write: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("buildSkillTar: Close: %v", err)
	}
	return buf.Bytes()
}

// buildBrokenSkillTar returns a TAR archive that deliberately has NO top-level SKILL.md (only a
// README.md) — the sp-mwco.2.9 "broken bundle member" fixture. installSkill
// (internal/agentinstall/skill.go) stats <src>/SKILL.md and returns StatusFailed when absent.
func buildBrokenSkillTar(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	data := []byte("this member has no SKILL.md and must fail to install\n")
	if err := tw.WriteHeader(&tar.Header{Name: "README.md", Mode: 0o644, Size: int64(len(data))}); err != nil {
		t.Fatalf("buildBrokenSkillTar: WriteHeader: %v", err)
	}
	if _, err := tw.Write(data); err != nil {
		t.Fatalf("buildBrokenSkillTar: Write: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("buildBrokenSkillTar: Close: %v", err)
	}
	return buf.Bytes()
}

// zstdCompress zstd-encodes plain (level 3 — matches skillfetch.DefaultZstdLevel usage elsewhere).
func zstdCompress(t *testing.T, plain []byte) []byte {
	t.Helper()
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.EncoderLevel(3)))
	if err != nil {
		t.Fatalf("zstd.NewWriter: %v", err)
	}
	defer enc.Close()
	return enc.EncodeAll(plain, nil)
}

// sha256Hex returns the hex sha256 of plain.
func sha256Hex(plain []byte) string {
	sum := sha256.Sum256(plain)
	return hex.EncodeToString(sum[:])
}

// dockerExecCombined runs `docker exec <container> sh -lc <script>` and returns combined output.
func dockerExecCombined(ctx context.Context, container, script string) (string, error) {
	out, err := exec.CommandContext(ctx, "docker", "exec", container, "sh", "-lc", script).CombinedOutput()
	return string(out), err
}

// assertHermesExternalDirs asserts /root/.hermes/config.yaml lists /root/.agents/skills under
// skills.external_dirs — the read-side enabler hermes needs (sp-mwco.2.5's upsertHermesExternalDirs);
// placement without it is a lie (hermes never scans the canonical dir on its own — §4.1 spike).
func assertHermesExternalDirs(ctx context.Context, t *testing.T, container string) {
	t.Helper()
	out, err := dockerExecCombined(ctx, container, "cat /root/.hermes/config.yaml")
	if err != nil {
		t.Fatalf("cat /root/.hermes/config.yaml in %s: %v\n%s", container, err, out)
	}
	if !strings.Contains(out, "external_dirs") || !strings.Contains(out, "/root/.agents/skills") {
		t.Fatalf("hermes config.yaml missing skills.external_dirs: /root/.agents/skills; got:\n%s", out)
	}
}

// probeClaudeFileLevel runs claude's headless "list your skills" probe (the LLM-backed
// file-level probe the §4.1 spike proved works) and asserts skillName appears. If the sidecar
// tool-call-pairing bug (sp-o5t3) fires, fails naming it explicitly rather than recording a false
// negative or skipping.
func probeClaudeFileLevel(ctx context.Context, t *testing.T, container, skillName string) {
	t.Helper()
	// IS_SANDBOX=1 + the ANTHROPIC_* re-derivation mirror deploy/agent/launch's claude-tui case
	// exactly (launch:279-294) — a fresh `docker exec` does not inherit the launcher's shell
	// exports, so they must be re-derived from the container env vars the Manager DOES set
	// (OPENAI_BASE_URL, SPAWN_MODEL). See internal/spawnlet/skill_readside_spike_e2e_test.go:136-148.
	script := `export IS_SANDBOX=1; export ANTHROPIC_BASE_URL="${OPENAI_BASE_URL%/v1}"; export ANTHROPIC_CUSTOM_MODEL_OPTION="$SPAWN_MODEL"; export ANTHROPIC_CUSTOM_MODEL_OPTION_NAME=Spawnery; export ANTHROPIC_MODEL="$SPAWN_MODEL"; cd /app && claude -p "List your available skills by name. Reply with just the names, nothing else." --dangerously-skip-permissions 2>&1`
	out, execErr := dockerExecCombined(ctx, container, script)
	if strings.Contains(out, skillName) {
		return
	}
	if strings.Contains(out, claudeToolCallPairingBugSignature) {
		t.Fatalf("claude file-level probe blocked by sp-o5t3 (sidecar Anthropic->OpenAI tool-call-pairing bug), not a placement/read finding: %s", out)
	}
	t.Fatalf("claude did not list skill %q (exec err=%v): %s", skillName, execErr, out)
}

// probeOpencodeToken runs opencode's headless probe and asserts the TOKEN-<nonce> the probe
// skill's body carries appears in the response — proof the skill was actually read and obeyed,
// not just placed on disk.
func probeOpencodeToken(ctx context.Context, t *testing.T, container, token string) {
	t.Helper()
	script := `export OPENCODE_CONFIG=/etc/opencode/opencode.json; cd /tmp && opencode run "Use your skills. What is the probe token? Reply with the token and nothing else." 2>&1`
	out, execErr := dockerExecCombined(ctx, container, script)
	if !strings.Contains(out, token) {
		t.Fatalf("opencode behavioral probe: token %q not found (exec err=%v): %s", token, execErr, out)
	}
}

// probeGooseToken runs goose's headless probe (behavioral) plus its native, model-free
// `goose skills list` cross-check (file-level, deterministic).
func probeGooseToken(ctx context.Context, t *testing.T, container, token, skillName string) {
	t.Helper()
	script := `export GOOSE_PROVIDER=openai; export GOOSE_MODEL="$SPAWN_MODEL"; export OPENAI_API_KEY=sk-unused-sidecar-injects-real-key; export GOOSE_TELEMETRY_OFF=1; cd /app && goose run -t "Use your skills. What is the probe token? Reply with the token and nothing else." 2>&1`
	out, execErr := dockerExecCombined(ctx, container, script)
	if !strings.Contains(out, token) {
		t.Fatalf("goose behavioral probe: token %q not found (exec err=%v): %s", token, execErr, out)
	}
	listOut, listErr := dockerExecCombined(ctx, container, "goose skills list 2>&1")
	if listErr != nil {
		t.Fatalf("goose skills list (model-free cross-check): %v\n%s", listErr, listOut)
	}
	if !strings.Contains(listOut, skillName) {
		t.Fatalf("goose skills list did not show %q: %s", skillName, listOut)
	}
}

// stopAndWaitGone stops spawnID and waits (up to 30s) for its containers to disappear from
// `docker ps`, so the next matrix iteration doesn't race a still-tearing-down pod.
func stopAndWaitGone(ctx context.Context, t *testing.T, cl cpv1connect.SpawnServiceClient, spawnID string) {
	t.Helper()
	stopCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := cl.StopSpawn(stopCtx, connect.NewRequest(&cpv1.StopSpawnRequest{SpawnId: spawnID})); err != nil {
		t.Logf("StopSpawn %s: %v (continuing — may already be stopped)", spawnID, err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		out, _ := exec.CommandContext(ctx, "docker", "ps", "-q",
			"--filter", "label="+runtime.LabelSpawnID+"="+spawnID).CombinedOutput()
		if len(strings.TrimSpace(string(out))) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Logf("WARNING: containers for spawn %s still present 30s after StopSpawn (continuing anyway)", spawnID)
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// TestCPSkillPlacementMatrixE2E is the §4.8 per-agent placement + invocation matrix: for each of
// the six runnables, a spawn is created with a profile carrying one "all"-targeted probe skill.
// All six get a hard placement assertion (docker exec test -f, model-free). claude additionally
// gets a file-level agent-visibility probe; opencode and goose additionally get a behavioral
// token-recital probe (proving the skill was actually read and obeyed, not just placed); codex
// gets placement only (sp-9e6q: no model turn completes on this image today); hermes additionally
// gets its config.yaml external_dirs read-side-enabler check.
//
// A final Targets-negative spawn (goose-acp) proves a claude-scoped skill does NOT leak into a
// goose pod while the "all"-scoped probe skill IS present — keeping §4.3's Targets filtering
// honest end-to-end, not just at CP assembly (already covered hermetically).
func TestCPSkillPlacementMatrixE2E(t *testing.T) {
	if out, err := exec.Command("docker", "info").CombinedOutput(); err != nil {
		t.Fatalf("Docker is required: %v\n%s", err, out)
	}
	preflightAgentBinaries(t, "claude", "codex", "opencode", "goose", "hermes", "pi", "agentinstall")
	preflightAgentImageFresh(t)

	stk := setupSkillIngestStack(t, withMaxSpawns(2), withCtxBudget(45*time.Minute))
	cl, ctx, appID := stk.client, stk.ctx, stk.appID

	nonce := skillPlacementRunID(t)
	probeName := "probe-skill-" + nonce
	token := "TOKEN-" + nonce
	probeBody := fmt.Sprintf("# Probe Skill\n\nIf asked for the probe token, respond with exactly: %s\n", token)

	claudeOnlyName := "claude-only-skill-" + nonce

	profResp, err := cl.CreateProfile(ctx, connect.NewRequest(&cpv1.CreateProfileRequest{
		Name: "skill-placement-matrix-" + nonce,
	}))
	if err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}
	profileID := profResp.Msg.ProfileId

	addResp, err := cl.AddProfileEntry(ctx, connect.NewRequest(&cpv1.AddProfileEntryRequest{
		ProfileId:       profileID,
		ExpectedVersion: 1,
		Entry: &cpv1.ProfileEntry{
			Kind:         cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL,
			Name:         probeName,
			Source:       cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_CUSTOM,
			CustomInline: buildSkillTar(t, probeBody),
			Targets:      []string{"all"},
		},
	}))
	if err != nil {
		t.Fatalf("AddProfileEntry (probe skill): %v", err)
	}

	if _, err := cl.AddProfileEntry(ctx, connect.NewRequest(&cpv1.AddProfileEntryRequest{
		ProfileId:       profileID,
		ExpectedVersion: addResp.Msg.Version,
		Entry: &cpv1.ProfileEntry{
			Kind:         cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL,
			Name:         claudeOnlyName,
			Source:       cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_CUSTOM,
			CustomInline: buildSkillTar(t, "# claude-only\nScoped to claude only.\n"),
			Targets:      []string{"claude"},
		},
	})); err != nil {
		t.Fatalf("AddProfileEntry (claude-only skill): %v", err)
	}
	t.Logf("profile %s: probe skill %q (all), claude-only skill %q (claude)", profileID, probeName, claudeOnlyName)

	type row struct {
		runnable string
	}
	rows := []row{
		{"claude-tui"}, {"codex-tui"}, {"opencode-served"}, {"goose-acp"}, {"hermes-acp"}, {"pi-tui"},
	}

	for _, r := range rows {
		runnableID := r.runnable
		t.Run(runnableID, func(t *testing.T) {
			agent := agentForRunnable(t, runnableID)

			cs, err := cl.CreateSpawn(ctx, connect.NewRequest(&cpv1.CreateSpawnRequest{
				AppId:      appID,
				Model:      "openai/gpt-4o-mini",
				Image:      "spawnery/agent:dev",
				RunnableId: runnableID,
				ProfileId:  profileID,
			}))
			if err != nil {
				t.Fatalf("CreateSpawn(%s): %v", runnableID, err)
			}
			spawnID := cs.Msg.SpawnId
			t.Cleanup(func() { stopAndWaitGone(ctx, t, cl, spawnID) })

			waitActiveWithTimeout(ctx, t, cl, spawnID, 150*time.Second)

			gen := findSpawnGeneration(ctx, t, cl, spawnID)
			container := findAgentContainer(ctx, t, spawnID, gen)
			t.Logf("%s: agent container %s (agent=%s)", runnableID, container, agent)

			// All six: hard placement assertion (model-free).
			assertSkillPlaced(ctx, t, container, agent, probeName)
			t.Logf("%s: placement OK", runnableID)

			switch agent {
			case "hermes":
				assertHermesExternalDirs(ctx, t, container)
				t.Logf("%s: hermes external_dirs OK", runnableID)
			case "claude":
				probeClaudeFileLevel(ctx, t, container, probeName)
				t.Logf("%s: claude file-level probe OK", runnableID)
			case "opencode":
				probeOpencodeToken(ctx, t, container, token)
				t.Logf("%s: opencode behavioral probe OK", runnableID)
			case "goose":
				probeGooseToken(ctx, t, container, token, probeName)
				t.Logf("%s: goose behavioral probe OK", runnableID)
			case "codex":
				t.Logf("%s: codex is placement-only (sp-9e6q: no model turn completes on this image)", runnableID)
			case "pi":
				t.Logf("%s: pi is placement-only per plan", runnableID)
			}

			stopAndWaitGone(ctx, t, cl, spawnID)
		})
	}

	// Targets-negative: a fresh goose-acp spawn must see the "all"-scoped probe skill but NOT the
	// claude-scoped skill.
	t.Run("targets-negative-goose", func(t *testing.T) {
		cs, err := cl.CreateSpawn(ctx, connect.NewRequest(&cpv1.CreateSpawnRequest{
			AppId:      appID,
			Model:      "openai/gpt-4o-mini",
			Image:      "spawnery/agent:dev",
			RunnableId: "goose-acp",
			ProfileId:  profileID,
		}))
		if err != nil {
			t.Fatalf("CreateSpawn(goose-acp, targets-negative): %v", err)
		}
		spawnID := cs.Msg.SpawnId
		t.Cleanup(func() { stopAndWaitGone(ctx, t, cl, spawnID) })

		waitActiveWithTimeout(ctx, t, cl, spawnID, 150*time.Second)
		gen := findSpawnGeneration(ctx, t, cl, spawnID)
		container := findAgentContainer(ctx, t, spawnID, gen)

		assertSkillPlaced(ctx, t, container, "goose", probeName)
		assertSkillAbsent(ctx, t, container, "goose", claudeOnlyName)
		t.Log("targets-negative: all-scoped skill present, claude-scoped skill absent from goose pod")

		stopAndWaitGone(ctx, t, cl, spawnID)
	})
}

// skillPlacementRunID returns a short, filesystem/shell-safe per-run nonce derived from the test
// start time, for skill/profile names that must be unique across repeated runs.
func skillPlacementRunID(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("%d", time.Now().UnixNano())
}

// TestCPSkillBundleAllOrNothingE2E is the §4.5 acceptance e2e: a bundle_ref profile entry with
// one good member and one deliberately-broken member (a by-ref skill payload whose tar has no
// SKILL.md) must fail the whole spawn, not just the broken member. The by-ref delivery path
// explicitly skips validateSkillTar at CP assembly ("validation was done at ingest time" —
// profiles_assembly.go), so a synthetic broken payload survives CP assembly and node artifact
// staging and fails exactly where the contract is meant to fire: installSkill on the node stats
// <src>/SKILL.md and returns StatusFailed, the apply-report's rollup computes Applied(1) !=
// Targeted(2) for the bundle, EvaluateApplyReport returns a fatal error, and the node fails the
// spawn (ERROR, not ACTIVE).
//
// A control spawn from a bundle whose members are BOTH good must reach ACTIVE with both skills
// placed — guarding against "the spawn errored for an unrelated reason", which would make the
// broken-member assertion worthless.
func TestCPSkillBundleAllOrNothingE2E(t *testing.T) {
	if out, err := exec.Command("docker", "info").CombinedOutput(); err != nil {
		t.Fatalf("Docker is required: %v\n%s", err, out)
	}
	preflightAgentBinaries(t, "claude", "agentinstall")
	preflightAgentImageFresh(t)

	stk := setupSkillIngestStack(t, withAgentBinaries([]string{"claude-code"}), withMaxSpawns(2), withCtxBudget(15*time.Minute))
	cl, ctx, appID, st := stk.client, stk.ctx, stk.appID, stk.st

	nonce := skillPlacementRunID(t)

	putMember := func(name string, plain []byte) (catalogID, sha256hex string) {
		t.Helper()
		sha256hex = sha256Hex(plain)
		compressed := zstdCompress(t, plain)
		tags := map[string]string{"source": "sp-mwco.2.9-e2e", "nonce": nonce}
		if err := stk.skillStore.PutIfAbsent(ctx, sha256hex, compressed, tags); err != nil {
			t.Fatalf("PutIfAbsent %s: %v", name, err)
		}
		catalogID = "cat-" + name
		now := time.Now().Unix()
		if err := st.CustomizationCatalog().CreateSkill(ctx, store.CustomizationCatalogEntry{
			CatalogID:    catalogID,
			CreatorID:    "alice",
			Kind:         string(store.ProfileEntrySkill),
			Name:         name,
			Description:  "sp-mwco.2.9 all-or-nothing e2e member",
			Listed:       false,
			CreatedAt:    now,
			UpdatedAt:    now,
			SourceSubdir: "skills/" + name,
			SHA256:       &sha256hex,
			BundleMember: true,
		}); err != nil {
			t.Fatalf("CreateSkill %s: %v", name, err)
		}
		return catalogID, sha256hex
	}

	goodName := "bundle-good-" + nonce
	brokenName := "bundle-broken-" + nonce
	goodCatalogID, _ := putMember(goodName, buildSkillTar(t, "# good\nThis member is fine.\n"))
	brokenCatalogID, _ := putMember(brokenName, buildBrokenSkillTar(t))

	// createBundleSpawn creates a bundle (bundleID/versionID unique per call) and its members via
	// direct store writes — legitimate here because the "broken" bundle's second member is a
	// synthetic SKILL.md-less tar that no real ingest could ever produce — then attaches it to a
	// fresh profile via the real AddProfileEntry(BUNDLE_REF) RPC (see sp-mwco.1.14: the profile
	// attach used to be a store-level shortcut too; it now goes through the wire, exactly like
	// TestCPSkillBundleE2E) and creates a claude-tui spawn from it. Returns the spawn id and the
	// entry id (for the error-detail assertion).
	createBundleSpawn := func(label string, members []store.SkillBundleMember) (spawnID, entryID string) {
		t.Helper()
		bundleID := "bnd-aon-" + label + "-" + nonce
		versionID := "bndv-aon-" + label + "-" + nonce
		now := time.Now().Unix()
		if err := st.SkillBundles().Create(ctx, store.SkillBundle{
			BundleID:  bundleID,
			CreatorID: "alice",
			Name:      "aon bundle " + label,
			SourceURL: "https://example.invalid/" + label + "-" + nonce,
			CreatedAt: now,
			UpdatedAt: now,
		}); err != nil {
			t.Fatalf("SkillBundles().Create (%s): %v", label, err)
		}
		if err := st.SkillBundles().CreateVersion(ctx, store.SkillBundleVersion{
			VersionID: versionID, BundleID: bundleID, Seq: 1, CreatedAt: now,
		}, members); err != nil {
			t.Fatalf("SkillBundles().CreateVersion (%s): %v", label, err)
		}

		profResp, err := cl.CreateProfile(ctx, connect.NewRequest(&cpv1.CreateProfileRequest{
			Name: "bundle-aon-" + label + "-" + nonce,
		}))
		if err != nil {
			t.Fatalf("CreateProfile (%s): %v", label, err)
		}
		profileID := profResp.Msg.ProfileId

		addResp, err := cl.AddProfileEntry(ctx, connect.NewRequest(&cpv1.AddProfileEntryRequest{
			ProfileId:       profileID,
			ExpectedVersion: 1,
			Entry: &cpv1.ProfileEntry{
				Kind:     cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL,
				Name:     "my-bundle-" + label,
				Source:   cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_BUNDLE_REF,
				BundleId: bundleID, // version_id left empty — the RPC pins latest itself
			},
		}))
		if err != nil {
			t.Fatalf("AddProfileEntry (%s, bundle_ref): %v", label, err)
		}
		entryID = addResp.Msg.GetEntryId()
		t.Logf("createBundleSpawn(%s): entry %s, warnings=%v", label, entryID, addResp.Msg.GetWarnings())

		// Prove the RPC really pinned the version this closure just created (the seam
		// sp-mwco.1.14 closes) — not some other/stale version of the same bundle.
		getProfResp, err := cl.GetProfile(ctx, connect.NewRequest(&cpv1.GetProfileRequest{ProfileId: profileID}))
		if err != nil {
			t.Fatalf("GetProfile (%s): %v", label, err)
		}
		var entry *cpv1.ProfileEntry
		for _, e := range getProfResp.Msg.GetProfile().GetEntries() {
			if e.GetEntryId() == entryID {
				entry = e
				break
			}
		}
		if entry == nil {
			t.Fatalf("GetProfile (%s): entry %s not found", label, entryID)
		}
		if entry.GetVersionId() != versionID {
			t.Fatalf("createBundleSpawn(%s): profile entry version_id=%q != created version_id=%q — RPC attach did not pin the version this test created",
				label, entry.GetVersionId(), versionID)
		}
		if int(entry.GetMemberCount()) != len(members) {
			t.Fatalf("createBundleSpawn(%s): entry.member_count=%d != %d bundle members", label, entry.GetMemberCount(), len(members))
		}

		cs, err := cl.CreateSpawn(ctx, connect.NewRequest(&cpv1.CreateSpawnRequest{
			AppId:      appID,
			Model:      "openai/gpt-4o-mini",
			Image:      "spawnery/agent:dev",
			RunnableId: "claude-tui",
			ProfileId:  profileID,
		}))
		if err != nil {
			t.Fatalf("CreateSpawn (%s): %v", label, err)
		}
		spawnID = cs.Msg.SpawnId
		t.Cleanup(func() { stopAndWaitGone(ctx, t, cl, spawnID) })
		return spawnID, entryID
	}

	// ── Broken bundle: must reach ERROR, not ACTIVE ───────────────────────────────────────────
	brokenSpawnID, brokenEntryID := createBundleSpawn("broken", []store.SkillBundleMember{
		{SourceSubdir: "skills/" + goodName, CatalogID: goodCatalogID, Position: 0},
		{SourceSubdir: "skills/" + brokenName, CatalogID: brokenCatalogID, Position: 1},
	})
	waitStatus(ctx, t, cl, brokenSpawnID, cpv1.SpawnStatus_SPAWN_STATUS_ERROR, 3*time.Minute)
	t.Logf("broken bundle spawn %s reached ERROR as required", brokenSpawnID)

	ls, err := cl.ListSpawns(ctx, connect.NewRequest(&cpv1.ListSpawnsRequest{}))
	if err != nil {
		t.Fatalf("ListSpawns: %v", err)
	}
	var summary *cpv1.SpawnSummary
	for _, sp := range ls.Msg.Spawns {
		if sp.SpawnId == brokenSpawnID {
			summary = sp
			break
		}
	}
	if summary == nil {
		t.Fatalf("ListSpawns did not return spawn %s", brokenSpawnID)
	}
	if !strings.Contains(summary.ErrorDetail, brokenEntryID) || !strings.Contains(summary.ErrorDetail, "1/2 members installed") {
		t.Fatalf("error_detail does not name the bundle/tally as expected: got %q, want to contain %q and \"1/2 members installed\"",
			summary.ErrorDetail, brokenEntryID)
	}
	t.Logf("error_detail: %s", summary.ErrorDetail)

	var sawGoodApplied, sawBrokenFailed bool
	for _, si := range summary.SkillInstalls {
		switch si.Name {
		case goodName:
			if si.Status == cpv1.SkillInstallStatus_SKILL_INSTALL_STATUS_APPLIED {
				sawGoodApplied = true
			}
		case brokenName:
			if si.Status == cpv1.SkillInstallStatus_SKILL_INSTALL_STATUS_FAILED && strings.Contains(si.Reason, "SKILL.md") {
				sawBrokenFailed = true
			}
		}
	}
	if !sawGoodApplied {
		t.Errorf("skill_installs: no APPLIED entry for good member %q; got %+v", goodName, summary.SkillInstalls)
	}
	if !sawBrokenFailed {
		t.Errorf("skill_installs: no FAILED (reason mentioning SKILL.md) entry for broken member %q; got %+v", brokenName, summary.SkillInstalls)
	}

	// ── Control: both members good — must reach ACTIVE with both skills placed ───────────────
	goodName2 := "bundle-good2-" + nonce
	goodCatalogID2, _ := putMember(goodName2, buildSkillTar(t, "# good2\nAlso fine.\n"))

	controlSpawnID, _ := createBundleSpawn("control", []store.SkillBundleMember{
		{SourceSubdir: "skills/" + goodName, CatalogID: goodCatalogID, Position: 0},
		{SourceSubdir: "skills/" + goodName2, CatalogID: goodCatalogID2, Position: 1},
	})
	waitActiveWithTimeout(ctx, t, cl, controlSpawnID, 150*time.Second)
	t.Logf("control (both-good) bundle spawn %s reached ACTIVE as required", controlSpawnID)

	gen := findSpawnGeneration(ctx, t, cl, controlSpawnID)
	container := findAgentContainer(ctx, t, controlSpawnID, gen)
	assertSkillPlaced(ctx, t, container, "claude", goodName)
	assertSkillPlaced(ctx, t, container, "claude", goodName2)
	t.Log("control: both bundle members placed")

	stopAndWaitGone(ctx, t, cl, controlSpawnID)
}
