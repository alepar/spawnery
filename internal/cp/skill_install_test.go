package cp

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	nodev1 "spawnery/gen/node/v1"
	"spawnery/internal/cp/auth"
)

// --- skillInstalls (pure map type) ---

func TestSkillInstalls_SetGetClear(t *testing.T) {
	si := newSkillInstalls()
	if _, ok := si.get("sp1"); ok {
		t.Fatal("expected no entry before set")
	}
	si.set("sp1", 1, "ok", []skillInstallEntry{{Kind: "skill", Name: "s1", Status: "applied"}})
	entries, ok := si.get("sp1")
	if !ok || len(entries) != 1 || entries[0].Name != "s1" {
		t.Fatalf("get after set: %+v, ok=%v", entries, ok)
	}
	si.clear("sp1")
	if _, ok := si.get("sp1"); ok {
		t.Fatal("expected no entry after clear")
	}
}

func TestSkillInstalls_StaleGenerationDropped(t *testing.T) {
	si := newSkillInstalls()
	si.set("sp1", 5, "ok", []skillInstallEntry{{Name: "newer"}})
	si.set("sp1", 3, "ok", []skillInstallEntry{{Name: "older"}}) // stale — must be dropped
	entries, ok := si.get("sp1")
	if !ok || len(entries) != 1 || entries[0].Name != "newer" {
		t.Fatalf("stale generation was not dropped: %+v", entries)
	}
	si.set("sp1", 5, "warn", []skillInstallEntry{{Name: "same-gen-overwrites"}}) // equal gen: not stale, overwrites
	entries, _ = si.get("sp1")
	if len(entries) != 1 || entries[0].Name != "same-gen-overwrites" {
		t.Fatalf("equal-generation update should overwrite: %+v", entries)
	}
	if outcome, ok := si.getOutcome("sp1"); !ok || outcome != "warn" {
		t.Fatalf("equal-generation update should overwrite outcome too: %q, ok=%v", outcome, ok)
	}
}

func TestSkillInstalls_NilSafe(t *testing.T) {
	var si *skillInstalls
	si.set("sp1", 1, "ok", []skillInstallEntry{{Name: "x"}}) // must not panic
	if _, ok := si.get("sp1"); ok {
		t.Fatal("nil skillInstalls.get should report not-found")
	}
	if _, ok := si.getOutcome("sp1"); ok {
		t.Fatal("nil skillInstalls.getOutcome should report not-found")
	}
	si.clear("sp1") // must not panic
}

func TestSkillInstallStatusProtoRoundTrip(t *testing.T) {
	cases := []struct {
		str  string
		wire cpv1.SkillInstallStatus
	}{
		{"applied", cpv1.SkillInstallStatus_SKILL_INSTALL_STATUS_APPLIED},
		{"skipped", cpv1.SkillInstallStatus_SKILL_INSTALL_STATUS_SKIPPED},
		{"failed", cpv1.SkillInstallStatus_SKILL_INSTALL_STATUS_FAILED},
		{"unknown", cpv1.SkillInstallStatus_SKILL_INSTALL_STATUS_UNKNOWN},
		{"", cpv1.SkillInstallStatus_SKILL_INSTALL_STATUS_UNKNOWN},
	}
	for _, c := range cases {
		if got := skillInstallStatusToProto(c.str); got != c.wire {
			t.Errorf("skillInstallStatusToProto(%q) = %v, want %v", c.str, got, c.wire)
		}
	}

	nodeCases := []struct {
		wire nodev1.SkillInstallStatus
		str  string
	}{
		{nodev1.SkillInstallStatus_SKILL_INSTALL_STATUS_APPLIED, "applied"},
		{nodev1.SkillInstallStatus_SKILL_INSTALL_STATUS_SKIPPED, "skipped"},
		{nodev1.SkillInstallStatus_SKILL_INSTALL_STATUS_FAILED, "failed"},
		{nodev1.SkillInstallStatus_SKILL_INSTALL_STATUS_UNKNOWN, "unknown"},
		{nodev1.SkillInstallStatus_SKILL_INSTALL_STATUS_UNSPECIFIED, "unknown"},
	}
	for _, c := range nodeCases {
		if got := skillInstallStatusFromProto(c.wire); got != c.str {
			t.Errorf("skillInstallStatusFromProto(%v) = %q, want %q", c.wire, got, c.str)
		}
	}
}

// --- Integration: visible on ListSpawns, evicted on delete ---

// TestListSpawns_SkillInstallsVisible simulates the node having sent a SkillInstallReport
// (directly seeding the map, as server.go's handler would) and asserts ListSpawns surfaces it.
func TestListSpawns_SkillInstallsVisible(t *testing.T) {
	s, _, _ := newTestServer(t)
	makeSpawn(t, s, "sp1", "alice")

	s.skillInstalls.set("sp1", 1, "warn", []skillInstallEntry{
		{Agent: "claude", Kind: "skill", Name: "s1", Status: "applied", Bundle: "b1"},
		{Agent: "claude", Kind: "skill", Name: "s2", Status: "failed", Reason: "boom", Bundle: "b1"},
	})

	ctx := auth.WithOwner(context.Background(), "alice")
	resp, err := s.ListSpawns(ctx, connect.NewRequest(&cpv1.ListSpawnsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	var sp1 *cpv1.SpawnSummary
	for _, sm := range resp.Msg.Spawns {
		if sm.SpawnId == "sp1" {
			sp1 = sm
		}
	}
	if sp1 == nil {
		t.Fatal("sp1 not in ListSpawns response")
	}
	if len(sp1.SkillInstalls) != 2 {
		t.Fatalf("SkillInstalls: got %d entries, want 2: %+v", len(sp1.SkillInstalls), sp1.SkillInstalls)
	}
	byName := map[string]*cpv1.SkillInstall{}
	for _, e := range sp1.SkillInstalls {
		byName[e.Name] = e
	}
	if byName["s1"].Status != cpv1.SkillInstallStatus_SKILL_INSTALL_STATUS_APPLIED || byName["s1"].Bundle != "b1" {
		t.Errorf("s1: %+v", byName["s1"])
	}
	if byName["s2"].Status != cpv1.SkillInstallStatus_SKILL_INSTALL_STATUS_FAILED || byName["s2"].Reason != "boom" {
		t.Errorf("s2: %+v", byName["s2"])
	}
	if sp1.SkillInstallOutcome != cpv1.SkillInstallOutcome_SKILL_INSTALL_OUTCOME_WARN {
		t.Errorf("SkillInstallOutcome: got %v, want WARN", sp1.SkillInstallOutcome)
	}
}

// TestListSpawns_NoSkillInstalls_EmptyField verifies a spawn with no recorded report leaves
// SkillInstalls nil/empty (no spurious entries).
func TestListSpawns_NoSkillInstalls_EmptyField(t *testing.T) {
	s, _, _ := newTestServer(t)
	makeSpawn(t, s, "sp1", "alice")

	ctx := auth.WithOwner(context.Background(), "alice")
	resp, err := s.ListSpawns(ctx, connect.NewRequest(&cpv1.ListSpawnsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Msg.Spawns) != 1 || len(resp.Msg.Spawns[0].SkillInstalls) != 0 {
		t.Fatalf("expected empty SkillInstalls, got %+v", resp.Msg.Spawns)
	}
	if got := resp.Msg.Spawns[0].SkillInstallOutcome; got != cpv1.SkillInstallOutcome_SKILL_INSTALL_OUTCOME_UNSPECIFIED {
		t.Fatalf("expected UNSPECIFIED SkillInstallOutcome with no report recorded, got %v", got)
	}
}

// TestDeleteSpawn_EvictsSkillInstalls verifies no map leak: deleting a spawn drops its
// skillInstalls entry.
func TestDeleteSpawn_EvictsSkillInstalls(t *testing.T) {
	s, _, _ := newTestServer(t)
	makeSpawn(t, s, "sp1", "alice")
	s.skillInstalls.set("sp1", 1, "ok", []skillInstallEntry{{Name: "s1", Status: "applied"}})

	ctx := auth.WithOwner(context.Background(), "alice")
	if _, err := s.DeleteSpawn(ctx, connect.NewRequest(&cpv1.DeleteSpawnRequest{SpawnId: "sp1"})); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.skillInstalls.get("sp1"); ok {
		t.Error("skillInstalls entry survived DeleteSpawn (map leak)")
	}
}

// --- Wire-level: the actual NodeMessage_SkillInstallReport handler in server.go's runNode loop ---

// TestSkillInstallReportHandler_UpdatesMapAndListSpawns feeds a real SkillInstallReport
// NodeMessage through runNode's receive loop and asserts it lands in the skillInstalls map and
// is visible on ListSpawns.
func TestSkillInstallReportHandler_UpdatesMapAndListSpawns(t *testing.T) {
	s, reg, _ := newTestServer(t)
	makeSpawn(t, s, "sp1", "alice")

	in := make(chan *nodev1.NodeMessage, 8)
	go s.runNode(context.Background(), &capSender{}, recvFromChan(in))
	feedRegister(in, "n1", "")
	waitNodeClass(t, reg, "n1", "cloud")

	in <- &nodev1.NodeMessage{Msg: &nodev1.NodeMessage_SkillInstallReport{SkillInstallReport: &nodev1.SkillInstallReport{
		SpawnId: "sp1", Generation: 1, Outcome: nodev1.SkillInstallOutcome_SKILL_INSTALL_OUTCOME_OK,
		Entries: []*nodev1.SkillInstallEntry{
			{Agent: "claude", Kind: "skill", Name: "s1", Status: nodev1.SkillInstallStatus_SKILL_INSTALL_STATUS_APPLIED},
		},
	}}}

	waitCondition(t, "skillInstalls map set", func() bool {
		entries, ok := s.skillInstalls.get("sp1")
		return ok && len(entries) == 1 && entries[0].Name == "s1" && entries[0].Status == "applied"
	})

	ctx := auth.WithOwner(context.Background(), "alice")
	resp, err := s.ListSpawns(ctx, connect.NewRequest(&cpv1.ListSpawnsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	var found *cpv1.SpawnSummary
	for _, sm := range resp.Msg.Spawns {
		if sm.SpawnId == "sp1" {
			found = sm
		}
	}
	if found == nil || len(found.SkillInstalls) != 1 || found.SkillInstalls[0].Name != "s1" {
		t.Fatalf("SkillInstalls not surfaced on ListSpawns: %+v", found)
	}
}

// TestSkillInstallReportHandler_StaleGenerationIgnored feeds a newer-generation report followed
// by an older one and verifies the older is dropped (through the real wire handler, not just the
// map type directly).
func TestSkillInstallReportHandler_StaleGenerationIgnored(t *testing.T) {
	s, reg, _ := newTestServer(t)
	makeSpawn(t, s, "sp1", "alice")

	in := make(chan *nodev1.NodeMessage, 8)
	go s.runNode(context.Background(), &capSender{}, recvFromChan(in))
	feedRegister(in, "n1", "")
	waitNodeClass(t, reg, "n1", "cloud")

	in <- &nodev1.NodeMessage{Msg: &nodev1.NodeMessage_SkillInstallReport{SkillInstallReport: &nodev1.SkillInstallReport{
		SpawnId: "sp1", Generation: 5,
		Entries: []*nodev1.SkillInstallEntry{{Name: "newer"}},
	}}}
	waitCondition(t, "gen-5 report set", func() bool {
		entries, ok := s.skillInstalls.get("sp1")
		return ok && len(entries) == 1 && entries[0].Name == "newer"
	})

	in <- &nodev1.NodeMessage{Msg: &nodev1.NodeMessage_SkillInstallReport{SkillInstallReport: &nodev1.SkillInstallReport{
		SpawnId: "sp1", Generation: 3, // stale
		Entries: []*nodev1.SkillInstallEntry{{Name: "stale"}},
	}}}
	time.Sleep(20 * time.Millisecond) // give the loop a chance to (wrongly) apply it
	entries, ok := s.skillInstalls.get("sp1")
	if !ok || len(entries) != 1 || entries[0].Name != "newer" {
		t.Fatalf("stale-generation report was applied: %+v", entries)
	}
}

// TestSkillInstallReportHandler_UnspecifiedOutcomeFlagged is the sp-mwco.2.14 acceptance test:
// an UNSPECIFIED envelope outcome must never arrive (sp-mwco.2.12/.2.13 floors it to WARN on the
// node side before send), so if it somehow does, the CP must not accept it as a passing/neutral
// verdict. It is fed through the real wire handler (not the map type directly) and asserted to be
// normalized to "error" in the stored state and surfaced as ERROR on ListSpawns — not silently
// stored/surfaced as UNSPECIFIED (which SpawnSummary otherwise reserves for "no report yet").
func TestSkillInstallReportHandler_UnspecifiedOutcomeFlagged(t *testing.T) {
	s, reg, _ := newTestServer(t)
	makeSpawn(t, s, "sp1", "alice")

	in := make(chan *nodev1.NodeMessage, 8)
	go s.runNode(context.Background(), &capSender{}, recvFromChan(in))
	feedRegister(in, "n1", "")
	waitNodeClass(t, reg, "n1", "cloud")

	in <- &nodev1.NodeMessage{Msg: &nodev1.NodeMessage_SkillInstallReport{SkillInstallReport: &nodev1.SkillInstallReport{
		SpawnId: "sp1", Generation: 1,
		Outcome: nodev1.SkillInstallOutcome_SKILL_INSTALL_OUTCOME_UNSPECIFIED, // must never arrive; must be flagged
		Entries: []*nodev1.SkillInstallEntry{{Name: "s1", Status: nodev1.SkillInstallStatus_SKILL_INSTALL_STATUS_APPLIED}},
	}}}

	waitCondition(t, "outcome normalized to error", func() bool {
		outcome, ok := s.skillInstalls.getOutcome("sp1")
		return ok && outcome == "error"
	})

	ctx := auth.WithOwner(context.Background(), "alice")
	resp, err := s.ListSpawns(ctx, connect.NewRequest(&cpv1.ListSpawnsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	var sp1 *cpv1.SpawnSummary
	for _, sm := range resp.Msg.Spawns {
		if sm.SpawnId == "sp1" {
			sp1 = sm
		}
	}
	if sp1 == nil {
		t.Fatal("sp1 not in ListSpawns response")
	}
	if sp1.SkillInstallOutcome != cpv1.SkillInstallOutcome_SKILL_INSTALL_OUTCOME_ERROR {
		t.Fatalf("UNSPECIFIED envelope outcome must be flagged as ERROR, got %v", sp1.SkillInstallOutcome)
	}
}

// TestSkillInstallOutcomeProtoRoundTrip exercises the outcome string<->proto conversions directly
// (mirrors TestSkillInstallStatusProtoRoundTrip), including the UNSPECIFIED-arrival guard.
func TestSkillInstallOutcomeProtoRoundTrip(t *testing.T) {
	cases := []struct {
		str  string
		wire cpv1.SkillInstallOutcome
	}{
		{"ok", cpv1.SkillInstallOutcome_SKILL_INSTALL_OUTCOME_OK},
		{"warn", cpv1.SkillInstallOutcome_SKILL_INSTALL_OUTCOME_WARN},
		{"bundle_failed", cpv1.SkillInstallOutcome_SKILL_INSTALL_OUTCOME_BUNDLE_FAILED},
		{"error", cpv1.SkillInstallOutcome_SKILL_INSTALL_OUTCOME_ERROR},
		{"", cpv1.SkillInstallOutcome_SKILL_INSTALL_OUTCOME_UNSPECIFIED},
	}
	for _, c := range cases {
		if got := skillInstallOutcomeToProto(c.str); got != c.wire {
			t.Errorf("skillInstallOutcomeToProto(%q) = %v, want %v", c.str, got, c.wire)
		}
	}

	nodeCases := []struct {
		wire nodev1.SkillInstallOutcome
		str  string
	}{
		{nodev1.SkillInstallOutcome_SKILL_INSTALL_OUTCOME_OK, "ok"},
		{nodev1.SkillInstallOutcome_SKILL_INSTALL_OUTCOME_WARN, "warn"},
		{nodev1.SkillInstallOutcome_SKILL_INSTALL_OUTCOME_BUNDLE_FAILED, "bundle_failed"},
		{nodev1.SkillInstallOutcome_SKILL_INSTALL_OUTCOME_ERROR, "error"},
		{nodev1.SkillInstallOutcome_SKILL_INSTALL_OUTCOME_UNSPECIFIED, "error"}, // must never arrive; flagged+normalized
	}
	for _, c := range nodeCases {
		if got := skillInstallOutcomeFromProto("sp-test", c.wire); got != c.str {
			t.Errorf("skillInstallOutcomeFromProto(%v) = %q, want %q", c.wire, got, c.str)
		}
	}
}
