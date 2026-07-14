//go:build e2e

package cp_test

// TestCPSkillBundleE2E is the §4.11 acceptance e2e for multi-skill bundle ingest
// (sp-mwco.1). It proves the whole chain against the REAL obra/superpowers repo — the exact
// repo whose top-level AGENTS.md symlink used to break ingest closed, and which sp-mwco.1.1
// fixed:
//
//	IngestSkillFromURL (whole-repo, no subdir) → skill_bundle + N skill_bundle_member rows
//	  (root AGENTS.md symlink skipped, not fatal) →
//	GetBundle → the authoritative member set →
//	CreateProfile + AddProfileEntry(SKILL, BUNDLE_REF, pinned to latest version) →
//	GetProfile → pinned_seq == latest_seq, effective on-disk names (no collisions expected) →
//	CreateSpawn (claude-tui, spawnery/agent:dev) → ACTIVE →
//	EVERY member's SKILL.md present at the agentinstall-canonical path (+ claude's native path)
//	  inside the agent container, with NO extra/missing directories (anti-merge fence).
//
// This is also the sp-mwco.1.14 whole-journey acceptance: the only e2e that drives
// ingest-RPC -> BUNDLE_REF attach-RPC -> spawn in a single test, with zero store-level shortcuts
// anywhere on the path.
//
// This is deliberately a NEW file, not an edit to skill_ingest_e2e_test.go: §5 requires the
// sp-mwco.1 and sp-mwco.2 epics' e2e edits to be sequenced to avoid a collision, and sp-mwco.2.9
// already solved this by keeping its tests + shared helpers in their own files
// (skill_placement_e2e_test.go, skill_paths_e2e_test.go). Everything in package cp_test is
// visible across files, so this file gets setupSkillIngestStack + the path helpers for free at
// zero merge surface — the bead's intent ("extend TestCPSkillIngestE2E's stack") is honoured;
// only the byte-level location differs.
//
// Requires:
//   - Docker + spawnery/agent:dev + spawnery/sidecar:dev (built with agentinstall + apply-artifacts.sh
//     — run `make images`)
//   - Garage running (`just garage`), dev-creds.env present at deploy/garage/dev-creds.env
//   - OPENROUTER_API_KEY in env or .env at repo root
//   - Network access to github.com / api.github.com / codeload.github.com (set GITHUB_TOKEN to
//     raise the unauthenticated api.github.com rate limit from ~60/hr to 5000/hr)
//
// FAILS loudly (no skips) per the build-tag-is-opt-in rule when any dep is down.
// The test is intentionally slow (14-member ingest + 14-artifact spawn boot); budget ~6-8 minutes.
//
// Run:
//
//	set -a; . deploy/garage/dev-creds.env; set +a; \
//	  CGO_ENABLED=1 go test -tags e2e -run TestCPSkillBundleE2E -v -count=1 -timeout 20m ./internal/cp/

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/gen/cp/v1/cpv1connect"
	"spawnery/internal/agentinstall"
	"spawnery/internal/runtime"
)

// TestCPSkillBundleE2E is the §4.11 acceptance chain: ingest the real obra/superpowers repo as a
// multi-skill bundle, attach it to a profile as a BUNDLE_REF entry, spawn, and assert every
// member is placed at the agentinstall-canonical path with none merged/missing.
func TestCPSkillBundleE2E(t *testing.T) {
	// Pre-flight: Docker + the agentinstall/claude binaries the placement assertions need.
	if out, err := exec.Command("docker", "info").CombinedOutput(); err != nil {
		t.Fatalf("Docker is required: %v\n%s", err, out)
	}
	preflightAgentBinaries(t, "agentinstall", "claude")
	preflightAgentImageFresh(t)

	// 14 members = 14 repacks + 14 Garage puts on ingest, plus a 14-artifact spawn boot — both
	// materially slower than the single-skill stack's defaults (8m ctx / 120s ACTIVE budget).
	stk := setupSkillIngestStack(t, withCtxBudget(15*time.Minute), withMaxSpawns(2))
	cl, ctx, appID := stk.client, stk.ctx, stk.appID

	// ── Step 2: ingest the real bundle (whole repo — no ref, no subdir) ───────────────────────
	const bundleURL = "https://github.com/obra/superpowers"
	t.Logf("step 2: IngestSkillFromURL %s (whole repo)", bundleURL)

	ingestResp, err := cl.IngestSkillFromURL(ctx, connect.NewRequest(&cpv1.IngestSkillFromURLRequest{
		Url: bundleURL,
	}))
	if err != nil {
		switch connect.CodeOf(err) {
		case connect.CodeUnavailable:
			t.Fatalf("IngestSkillFromURL: GitHub is unreachable — check network + GITHUB_TOKEN: %v", err)
		case connect.CodeResourceExhausted:
			t.Fatalf("IngestSkillFromURL: GitHub rate limit — set GITHUB_TOKEN: %v", err)
		case connect.CodeFailedPrecondition:
			t.Fatalf("IngestSkillFromURL: skill ingest seam not wired (SetSkillIngest not called?): %v", err)
		default:
			msg := err.Error()
			if strings.Contains(msg, "symlink") || strings.Contains(msg, "non-regular") {
				t.Fatalf("IngestSkillFromURL failed on a symlink/non-regular entry — sp-mwco.1.1 (skip "+
					"non-regular tar entries instead of failing ingest) appears to have regressed: %v", err)
			}
			t.Fatalf("IngestSkillFromURL: %v", err)
		}
	}
	ingest := ingestResp.Msg
	t.Logf("step 2: ingested bundle_id=%s version_id=%s catalog_id=%s changed=%v",
		ingest.GetBundleId(), ingest.GetVersionId(), ingest.GetCatalogId(), ingest.GetChanged())

	// ── Step 3: assert the ingest response — §4.1 + §4.2 proof ────────────────────────────────
	if ingest.GetBundleId() == "" {
		t.Fatal("IngestSkillFromURLResponse.bundle_id is empty — whole-repo ingest did not create a bundle")
	}
	if ingest.GetVersionId() == "" {
		t.Fatal("IngestSkillFromURLResponse.version_id is empty")
	}
	if !ingest.GetChanged() {
		t.Fatal("IngestSkillFromURLResponse.changed is false — expected true on this test's first (fresh-store) ingest")
	}
	memberIDs := ingest.GetMemberCatalogIds()
	if len(memberIDs) < 2 {
		t.Fatalf("expected >= 2 member_catalog_ids from a whole-repo ingest of obra/superpowers (got %d: %v) — "+
			"recursive multi-skill discovery (sp-mwco.1.2) appears to have regressed to single-skill behaviour",
			len(memberIDs), memberIDs)
	}
	if ingest.GetCatalogId() != memberIDs[0] {
		t.Fatalf("catalog_id (%q) must equal member_catalog_ids[0] (%q) — documented \"first member\" contract",
			ingest.GetCatalogId(), memberIDs[0])
	}

	skipped := ingest.GetSkippedEntries()
	foundSymlinkSkip := false
	for _, e := range skipped {
		if strings.HasSuffix(e, " (symlink)") {
			foundSymlinkSkip = true
			break
		}
	}
	if !foundSymlinkSkip {
		t.Fatalf("expected skipped_entries to contain an entry ending \" (symlink)\" (obra/superpowers' root "+
			"AGENTS.md symlink — the exact fixture sp-mwco.1.1 fixed ingest for); got skipped_entries=%v — "+
			"if upstream removed the symlink, this assertion needs a new fixture repo", skipped)
	}
	t.Logf("step 3: skipped_entries=%v", skipped)
	t.Logf("step 3: warnings=%v", ingest.GetWarnings())

	// ── Step 4: GetBundle — the authoritative member set ──────────────────────────────────────
	t.Log("step 4: GetBundle")
	bundleResp, err := cl.GetBundle(ctx, connect.NewRequest(&cpv1.GetBundleRequest{BundleId: ingest.GetBundleId()}))
	if err != nil {
		t.Fatalf("GetBundle: %v", err)
	}
	members := bundleResp.Msg.GetMembers()
	if len(members) != len(memberIDs) {
		t.Fatalf("GetBundle returned %d members, ingest reported %d member_catalog_ids — mismatch", len(members), len(memberIDs))
	}
	memberIDSet := make(map[string]bool, len(memberIDs))
	for _, id := range memberIDs {
		memberIDSet[id] = true
	}
	for _, m := range members {
		if !memberIDSet[m.GetCatalogId()] {
			t.Fatalf("GetBundle member catalog_id %q not present in ingest's member_catalog_ids %v", m.GetCatalogId(), memberIDs)
		}
	}

	versions := bundleResp.Msg.GetVersions()
	if len(versions) == 0 {
		t.Fatal("GetBundle returned no versions")
	}
	latestVersion := versions[len(versions)-1]
	if latestVersion.GetVersionId() != ingest.GetVersionId() {
		t.Fatalf("GetBundle's latest version (seq ASC, last element) version_id=%q != ingest's version_id=%q",
			latestVersion.GetVersionId(), ingest.GetVersionId())
	}
	if latestVersion.GetSourceCommit() == "" {
		t.Fatal("latest BundleVersion.source_commit is empty — provenance not recorded")
	}

	nameSet := make(map[string]bool, len(members))
	membersBySubdir := make(map[string]*cpv1.BundleMember, len(members))
	for _, m := range members {
		if m.GetName() == "" {
			t.Fatalf("bundle member (catalog_id=%s) has empty name", m.GetCatalogId())
		}
		if m.GetSourceSubdir() == "" {
			t.Fatalf("bundle member %q has empty source_subdir", m.GetName())
		}
		if nameSet[m.GetName()] {
			t.Fatalf("duplicate member name %q — dedup-on-ingest regression", m.GetName())
		}
		nameSet[m.GetName()] = true
		membersBySubdir[m.GetSourceSubdir()] = m
	}
	t.Logf("step 4: %d members confirmed via GetBundle: %v", len(members), sortedKeys(nameSet))

	// ── Step 5: profile + BUNDLE_REF attach ────────────────────────────────────────────────────
	t.Log("step 5: CreateProfile + AddProfileEntry(SKILL, BUNDLE_REF)")
	profResp, err := cl.CreateProfile(ctx, connect.NewRequest(&cpv1.CreateProfileRequest{
		Name: "skill-bundle-e2e",
	}))
	if err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}
	profileID := profResp.Msg.GetProfileId()

	addResp, err := cl.AddProfileEntry(ctx, connect.NewRequest(&cpv1.AddProfileEntryRequest{
		ProfileId:       profileID,
		ExpectedVersion: 1,
		Entry: &cpv1.ProfileEntry{
			Kind:     cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL,
			Name:     "superpowers",
			Source:   cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_BUNDLE_REF,
			BundleId: ingest.GetBundleId(),
		},
	}))
	if err != nil {
		t.Fatalf("AddProfileEntry (bundle_ref): %v", err)
	}
	t.Logf("step 5: profile %s, entry %s, warnings=%v", profileID, addResp.Msg.GetEntryId(), addResp.Msg.GetWarnings())

	// ── Step 6: GetProfile — the pin + the EFFECTIVE on-disk names ────────────────────────────
	t.Log("step 6: GetProfile")
	getProfResp, err := cl.GetProfile(ctx, connect.NewRequest(&cpv1.GetProfileRequest{ProfileId: profileID}))
	if err != nil {
		t.Fatalf("GetProfile: %v", err)
	}
	var entry *cpv1.ProfileEntry
	for _, e := range getProfResp.Msg.GetProfile().GetEntries() {
		if e.GetSource() == cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_BUNDLE_REF {
			entry = e
			break
		}
	}
	if entry == nil {
		t.Fatal("GetProfile: no BUNDLE_REF entry found in profile")
	}
	if entry.GetBundleId() != ingest.GetBundleId() {
		t.Fatalf("profile entry bundle_id=%q != ingested bundle_id=%q", entry.GetBundleId(), ingest.GetBundleId())
	}
	if entry.GetVersionId() != ingest.GetVersionId() {
		t.Fatalf("profile entry version_id=%q != ingested version_id=%q — attach did not pin to the version just ingested",
			entry.GetVersionId(), ingest.GetVersionId())
	}
	if entry.GetPinnedSeq() == 0 || entry.GetPinnedSeq() != entry.GetLatestSeq() {
		t.Fatalf("expected pinned_seq == latest_seq > 0 on a just-ingested bundle (no update-available badge); got pinned_seq=%d latest_seq=%d",
			entry.GetPinnedSeq(), entry.GetLatestSeq())
	}

	excluded := make(map[string]bool, len(entry.GetExcludedSubdirs()))
	for _, s := range entry.GetExcludedSubdirs() {
		excluded[s] = true
	}
	wantNames := make(map[string]bool)
	for subdir, m := range membersBySubdir {
		if excluded[subdir] {
			continue
		}
		name := m.GetName()
		if renamed, ok := entry.GetMemberRenames()[subdir]; ok {
			name = renamed
		}
		wantNames[name] = true
	}
	if int32(len(wantNames)) != entry.GetMemberCount() {
		t.Fatalf("computed %d effective on-disk names but entry.member_count=%d — exclude/rename accounting mismatch",
			len(wantNames), entry.GetMemberCount())
	}
	if int32(len(members))-int32(len(excluded)) != entry.GetMemberCount() {
		t.Fatalf("member_count=%d != len(members)-len(excluded_subdirs)=%d", entry.GetMemberCount(), len(members)-len(excluded))
	}
	t.Logf("step 6: pinned_seq=%d latest_seq=%d member_count=%d effective names=%v",
		entry.GetPinnedSeq(), entry.GetLatestSeq(), entry.GetMemberCount(), sortedKeys(wantNames))

	// ── Step 7: spawn ───────────────────────────────────────────────────────────────────────────
	t.Log("step 7: CreateSpawn (claude-tui, spawnery/agent:dev, profile attached)")
	cs, err := cl.CreateSpawn(ctx, connect.NewRequest(&cpv1.CreateSpawnRequest{
		AppId:      appID,
		Model:      "openai/gpt-4o-mini",
		Image:      "spawnery/agent:dev",
		RunnableId: "claude-tui",
		ProfileId:  profileID,
	}))
	if err != nil {
		t.Fatalf("CreateSpawn: %v", err)
	}
	spawnID := cs.Msg.GetSpawnId()
	t.Logf("step 7: spawn created %s", spawnID)
	t.Cleanup(func() {
		stopCtx, c := context.WithTimeout(context.Background(), 10*time.Second)
		defer c()
		_, _ = cl.StopSpawn(stopCtx, connect.NewRequest(&cpv1.StopSpawnRequest{SpawnId: spawnID}))
		time.Sleep(2 * time.Second)
	})

	// 14 artifact fetches on the staging path (parallel fetch + aggregate deadline, sp-mwco.4.1) vs
	// the 120s the single-skill test uses; 240s is headroom, not a licence for slowness.
	t.Log("step 7: waiting for ACTIVE (240s budget, 14-member bundle staging path)")
	waitActiveForBundle(ctx, t, cl, spawnID, 240*time.Second)
	t.Logf("step 7: spawn %s is ACTIVE", spawnID)

	// ── Step 8: THE acceptance assertion — every member placed at the canonical path ──────────
	gen := findSpawnGeneration(ctx, t, cl, spawnID)
	agentContainer := findAgentContainer(ctx, t, spawnID, gen)
	agent := agentForRunnable(t, "claude-tui")
	t.Logf("step 8: agent container %s (gen %d, agent %q)", agentContainer, gen, agent)

	for name := range wantNames {
		assertSkillPlaced(ctx, t, agentContainer, agent, name)
	}
	t.Logf("step 8: all %d members placed at their canonical + native paths", len(wantNames))

	// Anti-merge fence: guards against N members silently merging into one payload dir — a
	// per-name test -f alone would NOT catch that, since it only checks presence, not exclusivity.
	canonicalRoot := agentinstall.CanonicalSkillsDir(skillsHomeEnv)
	out, err := exec.CommandContext(ctx, "docker", "exec", agentContainer, "ls", "-1", canonicalRoot).CombinedOutput()
	if err != nil {
		t.Fatalf("ls -1 %s in container %s: %v\n%s\n%s", canonicalRoot, agentContainer, err, out, dumpSkillTrees(ctx, agentContainer))
	}
	gotNames := make(map[string]bool)
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			gotNames[line] = true
		}
	}
	if !stringSetsEqual(gotNames, wantNames) {
		t.Fatalf("canonical skills dir %s contains %v, want exactly %v (extras or missing entries — "+
			"members may have merged into one payload dir)\n%s",
			canonicalRoot, sortedKeys(gotNames), sortedKeys(wantNames), dumpSkillTrees(ctx, agentContainer))
	}

	t.Logf("step 8: §4.11 acceptance PASS — %d members ingested from the real obra/superpowers repo "+
		"(root AGENTS.md symlink skipped), all placed with no extras/missing", len(wantNames))
}

// waitActiveForBundle polls ListSpawns until the spawn reaches ACTIVE, dumping artifact staging
// diagnostics from the agent container before failing on a non-ACTIVE terminal status or timeout.
// An ACTIVE timeout here is also the all-or-nothing contract (§5 / sp-mwco.2) doing its job: a
// partially-installed bundle MUST fail the spawn outright, not come up missing some members.
func waitActiveForBundle(ctx context.Context, t *testing.T, cl cpv1connect.SpawnServiceClient, id string, budget time.Duration) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for {
		ls, err := cl.ListSpawns(ctx, connect.NewRequest(&cpv1.ListSpawnsRequest{}))
		if err != nil {
			t.Fatalf("ListSpawns: %v", err)
		}
		for _, sp := range ls.Msg.GetSpawns() {
			if sp.GetSpawnId() != id {
				continue
			}
			switch sp.GetStatus() {
			case cpv1.SpawnStatus_SPAWN_STATUS_ACTIVE:
				return
			case cpv1.SpawnStatus_SPAWN_STATUS_ERROR, cpv1.SpawnStatus_SPAWN_STATUS_DELETED,
				cpv1.SpawnStatus_SPAWN_STATUS_UNREACHABLE:
				dumpArtifactStagingIfContainerFound(ctx, t, id)
				t.Fatalf("spawn %s reached terminal status %v before ACTIVE — a partially-installed "+
					"bundle must fail the spawn outright (all-or-nothing, §5); see staging dump above",
					id, sp.GetStatus())
			}
		}
		if time.Now().After(deadline) {
			dumpArtifactStagingIfContainerFound(ctx, t, id)
			t.Fatalf("spawn %s did not reach ACTIVE within %s — see staging dump above", id, budget)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// dumpArtifactStagingIfContainerFound best-effort locates the spawn's agent container (via docker
// ps, since findAgentContainer itself requires a known generation/registry lookup that may not be
// available on a failed spawn) and dumps artifact staging diagnostics. Failures are logged, not fatal.
func dumpArtifactStagingIfContainerFound(ctx context.Context, t *testing.T, spawnID string) {
	t.Helper()
	out, err := exec.CommandContext(ctx, "docker", "ps", "-q",
		"--filter", "label="+runtime.LabelSpawnID+"="+spawnID,
		"--filter", "label="+runtime.LabelRole+"=agent",
	).Output()
	if err != nil || len(strings.TrimSpace(string(out))) == 0 {
		t.Logf("dumpArtifactStaging: no agent container found for spawn %s (spawn likely never scheduled)", spawnID)
		return
	}
	container := strings.TrimSpace(strings.Split(string(out), "\n")[0])
	dumpArtifactStaging(ctx, t, container)
}

// dumpArtifactStaging dumps the artifact manifest, staged payload tree, and apply-report.json from
// the agent container — the diagnostics block used when a spawn fails to reach ACTIVE on the
// artifact-materialization path. Local to this file (not lifted to a shared helper — see plan).
func dumpArtifactStaging(ctx context.Context, t *testing.T, container string) {
	t.Helper()
	manifest, _ := exec.CommandContext(ctx, "docker", "exec", container,
		"cat", "/run/spawnery/artifacts/manifest.json").CombinedOutput()
	t.Logf("dumpArtifactStaging: manifest.json: %s", strings.TrimSpace(string(manifest)))

	payloads, _ := exec.CommandContext(ctx, "docker", "exec", container,
		"find", "/run/spawnery/artifacts/payloads", "-maxdepth", "3").CombinedOutput()
	t.Logf("dumpArtifactStaging: payloads tree: %s", strings.TrimSpace(string(payloads)))

	report, _ := exec.CommandContext(ctx, "docker", "exec", container,
		"cat", "/run/spawnery/artifacts/report/apply-report.json").CombinedOutput()
	t.Logf("dumpArtifactStaging: apply-report.json: %s", strings.TrimSpace(string(report)))

	logs, _ := exec.CommandContext(ctx, "docker", "logs", container).CombinedOutput()
	var fatalLines []string
	for _, line := range strings.Split(string(logs), "\n") {
		if strings.Contains(line, "SPAWNERY-APPLY-FATAL") {
			fatalLines = append(fatalLines, line)
		}
	}
	if len(fatalLines) > 0 {
		t.Logf("dumpArtifactStaging: SPAWNERY-APPLY-FATAL lines from `docker logs %s`:\n%s",
			container, strings.Join(fatalLines, "\n"))
	} else {
		t.Logf("dumpArtifactStaging: no SPAWNERY-APPLY-FATAL marker in `docker logs %s`", container)
	}
}

// sortedKeys returns the sorted keys of a string set, for deterministic log output and failure messages.
func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// stringSetsEqual reports whether two string sets contain exactly the same elements.
func stringSetsEqual(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}
