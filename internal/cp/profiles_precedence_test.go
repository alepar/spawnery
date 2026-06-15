package cp

// profiles_precedence_test.go — hermetic unit tests for profile assembly ordering
// (Step 0 audit result: genuinely uncovered).
//
// Confirmed gaps (audit of profiles_assembly_test.go + profiles_test.go):
//
// (a) [COVERED HERE] Ordering: no existing test asserts that assembleProfileArtifacts
//     emits artifacts in the recorded insertion order. The entry store returns entries
//     ordered by entry_id ASC (time-sortable UUID v7), so insertion order == emit order.
//
// (b) [NOT TESTED — FEATURE ABSENT] Config-key dedup at save time: design §9 / roast C21
//     specifies that AddProfileEntry with two kind=config entries sharing the SAME
//     normalized config key should return CodeInvalidArgument. This is NOT implemented by
//     the sibling tasks and is therefore not tested here. Track as a follow-up bead.

import (
	"context"
	"encoding/json"
	"testing"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/internal/agentinstall/spec"
)

// TestAssemble_MultiEntry_EmitOrder verifies that assembleProfileArtifacts returns
// artifacts in the same order that entries were inserted into the profile. The store
// returns entries ordered by entry_id ASC (UUID v7 = time-ordered), so the first
// AddProfileEntry call's artifact must appear first in the manifest.
func TestAssemble_MultiEntry_EmitOrder(t *testing.T) {
	s, _, _ := newTestServer(t)

	// Build two distinct MCP payload entries so we can distinguish them by content.
	mcp1, _ := json.Marshal(spec.MCPPayload{Stdio: &spec.MCPTransportStdio{Command: "first-tool"}})
	mcp2, _ := json.Marshal(spec.MCPPayload{Stdio: &spec.MCPTransportStdio{Command: "second-tool"}})

	profileID := createProfile(t, s)

	// Insert first, then second — insertion order must be preserved in the manifest.
	_, ver1 := addEntry(t, s, profileID, 1, &cpv1.ProfileEntry{
		Kind:         cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_MCP,
		Name:         "first-mcp",
		Source:       cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_CUSTOM,
		CustomInline: mcp1,
	})
	_, _ = addEntry(t, s, profileID, ver1, &cpv1.ProfileEntry{
		Kind:         cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_MCP,
		Name:         "second-mcp",
		Source:       cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_CUSTOM,
		CustomInline: mcp2,
	})

	p, entries := loadProfile(t, s, profileID)
	if len(entries) != 2 {
		t.Fatalf("want 2 entries in profile, got %d", len(entries))
	}

	got, err := s.assembleProfileArtifacts(context.Background(), p, entries)
	if err != nil {
		t.Fatalf("assembleProfileArtifacts: %v", err)
	}

	ms := findManifestSpec(got)
	if ms == nil {
		t.Fatal("no manifest spec in result")
	}
	m := unmarshalManifest(t, ms.Inline)

	if len(m.Artifacts) != 2 {
		t.Fatalf("want 2 artifacts in manifest, got %d", len(m.Artifacts))
	}

	// Assert insertion order is preserved: first-mcp must appear before second-mcp.
	if m.Artifacts[0].Name != "first-mcp" {
		t.Errorf("artifact[0].Name = %q, want first-mcp", m.Artifacts[0].Name)
	}
	if m.Artifacts[1].Name != "second-mcp" {
		t.Errorf("artifact[1].Name = %q, want second-mcp", m.Artifacts[1].Name)
	}
	// Verify payloads survive: Command field round-trips correctly.
	if m.Artifacts[0].MCP == nil || m.Artifacts[0].MCP.Stdio == nil || m.Artifacts[0].MCP.Stdio.Command != "first-tool" {
		t.Errorf("artifact[0] MCP payload wrong: %+v", m.Artifacts[0].MCP)
	}
	if m.Artifacts[1].MCP == nil || m.Artifacts[1].MCP.Stdio == nil || m.Artifacts[1].MCP.Stdio.Command != "second-tool" {
		t.Errorf("artifact[1] MCP payload wrong: %+v", m.Artifacts[1].MCP)
	}
}

// TestAssemble_MixedKindOrder verifies that a profile with interleaved MCP and Config
// entries preserves insertion order in the manifest (not grouped by kind).
func TestAssemble_MixedKindOrder(t *testing.T) {
	s, _, _ := newTestServer(t)

	mcpPayload, _ := json.Marshal(spec.MCPPayload{Stdio: &spec.MCPTransportStdio{Command: "my-mcp"}})
	cfgPayload, _ := json.Marshal(spec.ConfigPayload{Normalized: map[string]interface{}{"approvalPosture": "yolo"}})

	profileID := createProfile(t, s)
	_, ver1 := addEntry(t, s, profileID, 1, &cpv1.ProfileEntry{
		Kind:         cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_MCP,
		Name:         "entry-mcp",
		Source:       cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_CUSTOM,
		CustomInline: mcpPayload,
	})
	_, _ = addEntry(t, s, profileID, ver1, &cpv1.ProfileEntry{
		Kind:         cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_CONFIG,
		Name:         "entry-config",
		Source:       cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_CUSTOM,
		CustomInline: cfgPayload,
	})

	p, entries := loadProfile(t, s, profileID)
	got, err := s.assembleProfileArtifacts(context.Background(), p, entries)
	if err != nil {
		t.Fatalf("assembleProfileArtifacts: %v", err)
	}

	ms := findManifestSpec(got)
	if ms == nil {
		t.Fatal("no manifest spec")
	}
	m := unmarshalManifest(t, ms.Inline)
	if len(m.Artifacts) != 2 {
		t.Fatalf("want 2 artifacts, got %d", len(m.Artifacts))
	}
	// MCP was inserted first → must appear first in the manifest (not grouped by kind).
	if m.Artifacts[0].Kind != spec.KindMCP {
		t.Errorf("artifact[0].Kind = %q, want mcp (insertion order)", m.Artifacts[0].Kind)
	}
	if m.Artifacts[1].Kind != spec.KindConfig {
		t.Errorf("artifact[1].Kind = %q, want config (insertion order)", m.Artifacts[1].Kind)
	}
}

// TestAddProfileEntry_DuplicateConfigKey_NotYetEnforced documents the gap:
// two config entries setting the SAME normalized key should eventually be rejected at save
// time (design §9 / roast C21), but the check is NOT yet implemented.
// This test serves as a regression guard: if the feature IS later implemented, update this
// to assert CodeInvalidArgument instead.
func TestAddProfileEntry_DuplicateConfigKey_NotYetEnforced(t *testing.T) {
	s, _, _ := newTestServer(t)

	cfg, _ := json.Marshal(spec.ConfigPayload{Normalized: map[string]interface{}{"approvalPosture": "yolo"}})

	profileID := createProfile(t, s)
	_, ver1 := addEntry(t, s, profileID, 1, &cpv1.ProfileEntry{
		Kind:         cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_CONFIG,
		Name:         "cfg-first",
		Source:       cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_CUSTOM,
		CustomInline: cfg,
	})

	// Second entry also sets "approvalPosture" — design §9 wants CodeInvalidArgument here,
	// but the check is not implemented yet (sp-nrzf.3.12 gap doc).
	_, err := s.AddProfileEntry(aliceCtx(), connect.NewRequest(&cpv1.AddProfileEntryRequest{
		ProfileId:       profileID,
		ExpectedVersion: ver1,
		Entry: &cpv1.ProfileEntry{
			Kind:         cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_CONFIG,
			Name:         "cfg-dup",
			Source:       cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_CUSTOM,
			CustomInline: cfg,
		},
	}))
	if err != nil {
		// If the feature IS implemented, this should be CodeInvalidArgument — update the test.
		if connect.CodeOf(err) == connect.CodeInvalidArgument {
			t.Logf("config-key dedup IS now enforced at save time — update this test to assert the error")
		}
		t.Fatalf("unexpected error (dedup not yet enforced): %v", err)
	}
	// Gap documented: currently accepted silently. Track in bd for follow-up.
	t.Log("KNOWN GAP (design §9/roast C21): duplicate config key accepted — dedup not yet enforced at save time")
}
