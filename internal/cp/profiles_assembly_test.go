package cp

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/internal/agentinstall"
	"spawnery/internal/agentinstall/spec"
	"spawnery/internal/cp/store"
)

// makeTar creates a minimal TAR archive in memory. entries is a map of path → content.
func makeTar(entries map[string][]byte) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, content := range entries {
		_ = tw.WriteHeader(&tar.Header{
			Name: name,
			Mode: 0o644,
			Size: int64(len(content)),
		})
		_, _ = tw.Write(content)
	}
	_ = tw.Close()
	return buf.Bytes()
}

// validSkillTar returns a TAR archive with a top-level SKILL.md.
func validSkillTar() []byte {
	return makeTar(map[string][]byte{"SKILL.md": []byte("# My Skill\nThis is a skill.")})
}

// allTargetNames returns the full set of registered agent names (for use in buildManifestAndPayloads).
func allTargetNames() map[string]bool {
	reg := agentinstall.NewRegistry(agentinstall.MapEnviron{})
	m := make(map[string]bool)
	for _, name := range reg.Names() {
		m[name] = true
	}
	return m
}

// ---- helpers ----------------------------------------------------------------

// createProfile creates a profile named "test" for alice and returns its ID.
func createProfile(t *testing.T, s *Server) string {
	t.Helper()
	resp, err := s.CreateProfile(aliceCtx(), connect.NewRequest(&cpv1.CreateProfileRequest{Name: "test"}))
	if err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}
	return resp.Msg.ProfileId
}

// addEntry adds a profile entry and returns its ID.
func addEntry(t *testing.T, s *Server, profileID string, ver uint64, e *cpv1.ProfileEntry) (string, uint64) {
	t.Helper()
	resp, err := s.AddProfileEntry(aliceCtx(), connect.NewRequest(&cpv1.AddProfileEntryRequest{
		ProfileId:       profileID,
		ExpectedVersion: ver,
		Entry:           e,
	}))
	if err != nil {
		t.Fatalf("AddProfileEntry: %v", err)
	}
	return resp.Msg.EntryId, resp.Msg.Version
}

// loadProfile returns the profile + entries + secrets.
func loadProfile(t *testing.T, s *Server, profileID string) (store.Profile, []store.ProfileEntry) {
	t.Helper()
	p, entries, _, err := s.st.Profiles().Get(context.Background(), profileID)
	if err != nil {
		t.Fatalf("Profiles().Get: %v", err)
	}
	return p, entries
}

// findManifestSpec returns the manifest BYTES artifact spec from the results.
func findManifestSpec(specs []*cpv1.ArtifactSpec) *cpv1.ArtifactSpec {
	for _, s := range specs {
		if s.Id == "manifest" {
			return s
		}
	}
	return nil
}

// unmarshalManifest deserializes the manifest inline bytes.
func unmarshalManifest(t *testing.T, inline []byte) spec.Manifest {
	t.Helper()
	var m spec.Manifest
	if err := json.Unmarshal(inline, &m); err != nil {
		t.Fatalf("unmarshal manifest: %v", err)
	}
	return m
}

// writeSpecsToDir writes artifact specs to a temp staging dir and returns the dir.
// Only BYTES specs are written as files; TAR specs are extracted.
func writeSpecsToDir(t *testing.T, specs []*cpv1.ArtifactSpec) string {
	t.Helper()
	dir := t.TempDir()
	for _, a := range specs {
		if a.ContentType == cpv1.ArtifactContentType_ARTIFACT_CONTENT_TYPE_BYTES {
			dest := filepath.Join(dir, a.DestPath)
			if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(dest, a.Inline, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	return dir
}

// ---- tests ------------------------------------------------------------------

// TestAssemble_MCPEntry_CustomInline: single custom MCP entry → exactly one
// BYTES manifest spec; manifest yields one Artifact with Kind=mcp, Targets
// "all-detected", SecretRefs threaded from MCPSecretRefs.
func TestAssemble_MCPEntry_CustomInline(t *testing.T) {
	s, _, _ := newTestServer(t)

	mcpPayload := spec.MCPPayload{
		Stdio: &spec.MCPTransportStdio{Command: "npx", Args: []string{"my-mcp"}},
	}
	content, _ := json.Marshal(mcpPayload)

	profileID := createProfile(t, s)
	_, _ = addEntry(t, s, profileID, 1, &cpv1.ProfileEntry{
		Kind:          cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_MCP,
		Name:          "my-mcp",
		Source:        cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_CUSTOM,
		CustomInline:  content,
		McpSecretRefs: []string{"MY_TOKEN"},
	})

	p, entries := loadProfile(t, s, profileID)
	got, err := s.assembleProfileArtifacts(context.Background(), p, entries)
	if err != nil {
		t.Fatalf("assembleProfileArtifacts: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 spec (manifest only), got %d: %v", len(got), got)
	}

	ms := findManifestSpec(got)
	if ms == nil {
		t.Fatal("no manifest spec in result")
	}
	if ms.ContentType != cpv1.ArtifactContentType_ARTIFACT_CONTENT_TYPE_BYTES {
		t.Errorf("manifest ContentType = %v, want BYTES", ms.ContentType)
	}
	if ms.DestPath != "manifest.json" {
		t.Errorf("manifest DestPath = %q, want manifest.json", ms.DestPath)
	}
	if ms.Mode != 0o644 {
		t.Errorf("manifest Mode = %o, want 644", ms.Mode)
	}

	m := unmarshalManifest(t, ms.Inline)
	if m.SchemaVersion != spec.CurrentSchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d", m.SchemaVersion, spec.CurrentSchemaVersion)
	}
	if len(m.Artifacts) != 1 {
		t.Fatalf("want 1 artifact in manifest, got %d", len(m.Artifacts))
	}
	a := m.Artifacts[0]
	if a.Kind != spec.KindMCP {
		t.Errorf("Kind = %q, want mcp", a.Kind)
	}
	if a.Name != "my-mcp" {
		t.Errorf("Name = %q, want my-mcp", a.Name)
	}
	if len(a.Targets) != 1 || a.Targets[0] != "all-detected" {
		t.Errorf("Targets = %v, want [all-detected]", a.Targets)
	}
	if a.MCP == nil {
		t.Fatal("MCP payload is nil")
	}
	if len(a.MCP.SecretRefs) != 1 || a.MCP.SecretRefs[0] != "MY_TOKEN" {
		t.Errorf("SecretRefs = %v, want [MY_TOKEN]", a.MCP.SecretRefs)
	}
}

// TestAssemble_SkillEntry: skill entry with valid TAR → manifest spec + one TAR
// payload spec; manifest Artifact.Skill.Dir matches payloads/<entryID>.
func TestAssemble_SkillEntry(t *testing.T) {
	s, _, _ := newTestServer(t)

	profileID := createProfile(t, s)
	entryID, _ := addEntry(t, s, profileID, 1, &cpv1.ProfileEntry{
		Kind:         cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL,
		Name:         "my-skill",
		Source:       cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_CUSTOM,
		CustomInline: validSkillTar(),
	})

	p, entries := loadProfile(t, s, profileID)
	got, err := s.assembleProfileArtifacts(context.Background(), p, entries)
	if err != nil {
		t.Fatalf("assembleProfileArtifacts: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 specs (manifest + skill TAR), got %d: %v", len(got), got)
	}

	ms := findManifestSpec(got)
	if ms == nil {
		t.Fatal("no manifest spec")
	}
	m := unmarshalManifest(t, ms.Inline)
	if len(m.Artifacts) != 1 {
		t.Fatalf("want 1 artifact in manifest, got %d", len(m.Artifacts))
	}
	a := m.Artifacts[0]
	if a.Kind != spec.KindSkill {
		t.Errorf("Kind = %q, want skill", a.Kind)
	}
	expectedDir := "payloads/" + entryID
	if a.Skill == nil || a.Skill.Dir != expectedDir {
		t.Errorf("Skill.Dir = %q, want %q", func() string {
			if a.Skill != nil {
				return a.Skill.Dir
			}
			return "<nil>"
		}(), expectedDir)
	}
	if a.Payload != expectedDir {
		t.Errorf("Artifact.Payload = %q, want %q", a.Payload, expectedDir)
	}

	// Find TAR payload spec.
	var tarSpec *cpv1.ArtifactSpec
	for _, s := range got {
		if s.ContentType == cpv1.ArtifactContentType_ARTIFACT_CONTENT_TYPE_TAR {
			tarSpec = s
			break
		}
	}
	if tarSpec == nil {
		t.Fatal("no TAR payload spec in result")
	}
	if tarSpec.Id != entryID {
		t.Errorf("TAR spec Id = %q, want %q", tarSpec.Id, entryID)
	}
	if tarSpec.DestPath != expectedDir {
		t.Errorf("TAR spec DestPath = %q, want %q", tarSpec.DestPath, expectedDir)
	}
	if len(tarSpec.Inline) == 0 {
		t.Error("TAR spec Inline is empty")
	}
}

// TestAssemble_ConfigEntry_Normalized: normalized keys preserved; forbidden key
// in native → CodeInvalidArgument.
func TestAssemble_ConfigEntry_Normalized(t *testing.T) {
	s, _, _ := newTestServer(t)

	// Valid config with some normalized keys.
	cfg := spec.ConfigPayload{
		Normalized: map[string]interface{}{
			"approvalPosture": "yolo",
		},
	}
	content, _ := json.Marshal(cfg)

	profileID := createProfile(t, s)
	_, _ = addEntry(t, s, profileID, 1, &cpv1.ProfileEntry{
		Kind:         cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_CONFIG,
		Name:         "my-config",
		Source:       cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_CUSTOM,
		CustomInline: content,
	})

	p, entries := loadProfile(t, s, profileID)
	got, err := s.assembleProfileArtifacts(context.Background(), p, entries)
	if err != nil {
		t.Fatalf("assembleProfileArtifacts: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 spec, got %d", len(got))
	}
	m := unmarshalManifest(t, findManifestSpec(got).Inline)
	if len(m.Artifacts) != 1 || m.Artifacts[0].Config == nil {
		t.Fatal("expected 1 config artifact")
	}
}

// TestAssemble_ConfigEntry_ForbiddenNativeKey: a forbidden key in native map → CodeInvalidArgument.
func TestAssemble_ConfigEntry_ForbiddenNativeKey(t *testing.T) {
	s, _, _ := newTestServer(t)

	cfg := spec.ConfigPayload{
		Native: map[string]interface{}{
			"claude": map[string]interface{}{"model": "forbidden-value"},
		},
	}
	content, _ := json.Marshal(cfg)

	profileID := createProfile(t, s)
	_, _ = addEntry(t, s, profileID, 1, &cpv1.ProfileEntry{
		Kind:         cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_CONFIG,
		Name:         "bad-config",
		Source:       cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_CUSTOM,
		CustomInline: content,
	})

	p, entries := loadProfile(t, s, profileID)
	_, err := s.assembleProfileArtifacts(context.Background(), p, entries)
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("want CodeInvalidArgument, got %v", err)
	}
}

// TestAssemble_Targets: bogus target → error; explicit valid target preserved;
// empty/all targets → all-detected.
func TestAssemble_Targets(t *testing.T) {
	targetNames := allTargetNames()

	cases := []struct {
		name    string
		targets []string
		wantErr bool
		wantAll bool
	}{
		{"explicit valid", []string{"claude"}, false, false},
		{"all keyword", []string{"all"}, false, true},
		{"empty", []string{}, false, true},
		{"bogus agent", []string{"claude", "bogus"}, true, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := spec.MCPPayload{Stdio: &spec.MCPTransportStdio{Command: "npx"}}
			content, _ := json.Marshal(cfg)
			items := []resolvedEntry{{
				entry: store.ProfileEntry{
					EntryID: "e1",
					Kind:    store.ProfileEntryMCP,
					Name:    "test-mcp",
					Targets: tc.targets,
				},
				content: content,
			}}
			mani, _, err := buildManifestAndPayloads(items, targetNames)
			if tc.wantErr {
				if connect.CodeOf(err) != connect.CodeInvalidArgument {
					t.Fatalf("want CodeInvalidArgument, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(mani.Artifacts) == 0 {
				t.Fatal("no artifacts in manifest")
			}
			got := mani.Artifacts[0].Targets
			if tc.wantAll {
				if len(got) != 1 || got[0] != "all-detected" {
					t.Errorf("targets = %v, want [all-detected]", got)
				}
			} else {
				if len(got) != len(tc.targets) {
					t.Errorf("targets = %v, want %v", got, tc.targets)
				}
			}
		})
	}
}

// TestAssemble_CatalogRef: catalog_ref entry resolves via the seeded catalog;
// missing catalog id → CodeInvalidArgument.
func TestAssemble_CatalogRef(t *testing.T) {
	s, _, _ := newTestServer(t)

	// Create a catalog entry with MCP content.
	mcpPayload := spec.MCPPayload{HTTP: &spec.MCPTransportHTTP{URL: "https://example.com/mcp"}}
	catContent, _ := json.Marshal(mcpPayload)
	catResp, err := s.CreateCatalogEntry(aliceCtx(), connect.NewRequest(&cpv1.CreateCatalogEntryRequest{
		Kind:    cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_MCP,
		Name:    "cat-mcp",
		Content: catContent,
	}))
	if err != nil {
		t.Fatalf("CreateCatalogEntry: %v", err)
	}
	catalogID := catResp.Msg.CatalogId

	// Add catalog_ref entry.
	profileID := createProfile(t, s)
	_, _ = addEntry(t, s, profileID, 1, &cpv1.ProfileEntry{
		Kind:      cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_MCP,
		Name:      "cat-mcp",
		Source:    cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_CATALOG_REF,
		CatalogId: catalogID,
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
	if len(m.Artifacts) != 1 || m.Artifacts[0].Kind != spec.KindMCP {
		t.Fatalf("unexpected artifacts: %v", m.Artifacts)
	}

	// Missing catalog id — add a second entry with a nonexistent catalog ref.
	// p.Version is already 2 (incremented after the first addEntry), so use it directly.
	_, _ = addEntry(t, s, profileID, p.Version, &cpv1.ProfileEntry{
		Kind:      cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_MCP,
		Name:      "missing",
		Source:    cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_CATALOG_REF,
		CatalogId: "nonexistent-catalog-id",
	})
	p2, entries2 := loadProfile(t, s, profileID)
	_, err = s.assembleProfileArtifacts(context.Background(), p2, entries2)
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("want CodeInvalidArgument for missing catalog ref, got %v", err)
	}
}

// TestAssemble_EmptyProfile: empty-entry profile → (nil, nil).
func TestAssemble_EmptyProfile(t *testing.T) {
	s, _, _ := newTestServer(t)

	profileID := createProfile(t, s)
	p, entries := loadProfile(t, s, profileID)
	got, err := s.assembleProfileArtifacts(context.Background(), p, entries)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Fatalf("want nil, got %v", got)
	}
}

// TestAssemble_SecretsOnlyProfile: profile with only a secret ref (no entries) → (nil, nil).
func TestAssemble_SecretsOnlyProfile(t *testing.T) {
	s, _, _ := newTestServer(t)

	profileID := createProfile(t, s)
	// Add a secret ref (no entries).
	_, err := s.AddProfileSecretRef(aliceCtx(), connect.NewRequest(&cpv1.AddProfileSecretRefRequest{
		ProfileId:       profileID,
		ExpectedVersion: 1,
		SecretId:        "my-secret",
	}))
	if err != nil {
		t.Fatalf("AddProfileSecretRef: %v", err)
	}

	p, entries := loadProfile(t, s, profileID)
	got, err := s.assembleProfileArtifacts(context.Background(), p, entries)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Fatalf("want nil, got %v", got)
	}
}

// TestAssemble_MalformedMCPContent: malformed JSON for MCP → CodeInvalidArgument.
func TestAssemble_MalformedMCPContent(t *testing.T) {
	s, _, _ := newTestServer(t)

	profileID := createProfile(t, s)
	_, _ = addEntry(t, s, profileID, 1, &cpv1.ProfileEntry{
		Kind:         cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_MCP,
		Name:         "bad-mcp",
		Source:       cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_CUSTOM,
		CustomInline: []byte(`not valid json`),
	})

	p, entries := loadProfile(t, s, profileID)
	_, err := s.assembleProfileArtifacts(context.Background(), p, entries)
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("want CodeInvalidArgument, got %v", err)
	}
}

// TestAssemble_SkillTarMissingSkillMD: skill TAR without SKILL.md → CodeInvalidArgument.
func TestAssemble_SkillTarMissingSkillMD(t *testing.T) {
	s, _, _ := newTestServer(t)

	// TAR with a file but not SKILL.md at the top level.
	badTar := makeTar(map[string][]byte{"README.txt": []byte("no skill here")})

	profileID := createProfile(t, s)
	_, _ = addEntry(t, s, profileID, 1, &cpv1.ProfileEntry{
		Kind:         cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL,
		Name:         "bad-skill",
		Source:       cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_CUSTOM,
		CustomInline: badTar,
	})

	p, entries := loadProfile(t, s, profileID)
	_, err := s.assembleProfileArtifacts(context.Background(), p, entries)
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("want CodeInvalidArgument, got %v", err)
	}
}

// TestAssemble_UnknownNormalizedKey: unknown key in normalized → CodeInvalidArgument.
func TestAssemble_UnknownNormalizedKey(t *testing.T) {
	s, _, _ := newTestServer(t)

	cfg := spec.ConfigPayload{
		Normalized: map[string]interface{}{
			"unknownKey": "someValue",
		},
	}
	content, _ := json.Marshal(cfg)

	profileID := createProfile(t, s)
	_, _ = addEntry(t, s, profileID, 1, &cpv1.ProfileEntry{
		Kind:         cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_CONFIG,
		Name:         "bad-config",
		Source:       cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_CUSTOM,
		CustomInline: content,
	})

	p, entries := loadProfile(t, s, profileID)
	_, err := s.assembleProfileArtifacts(context.Background(), p, entries)
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("want CodeInvalidArgument for unknown normalized key, got %v", err)
	}
}

// TestAssemble_ForbiddenNormalizedKey: forbidden key name in normalized → CodeInvalidArgument.
func TestAssemble_ForbiddenNormalizedKey(t *testing.T) {
	s, _, _ := newTestServer(t)

	// "model" is in the forbidden union.
	cfg := spec.ConfigPayload{
		Normalized: map[string]interface{}{
			"model": "gpt-4",
		},
	}
	content, _ := json.Marshal(cfg)

	profileID := createProfile(t, s)
	_, _ = addEntry(t, s, profileID, 1, &cpv1.ProfileEntry{
		Kind:         cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_CONFIG,
		Name:         "bad-config",
		Source:       cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_CUSTOM,
		CustomInline: content,
	})

	p, entries := loadProfile(t, s, profileID)
	_, err := s.assembleProfileArtifacts(context.Background(), p, entries)
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("want CodeInvalidArgument for forbidden normalized key, got %v", err)
	}
}

// TestAssemble_PluginEntry: plugin entry → manifest with plugin artifact.
func TestAssemble_PluginEntry(t *testing.T) {
	s, _, _ := newTestServer(t)

	pl := spec.PluginPayload{Plugin: "my-plugin", Marketplace: "npm"}
	content, _ := json.Marshal(pl)

	profileID := createProfile(t, s)
	_, _ = addEntry(t, s, profileID, 1, &cpv1.ProfileEntry{
		Kind:         cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_PLUGIN,
		Name:         "my-plugin",
		Source:       cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_CUSTOM,
		CustomInline: content,
	})

	p, entries := loadProfile(t, s, profileID)
	got, err := s.assembleProfileArtifacts(context.Background(), p, entries)
	if err != nil {
		t.Fatalf("assembleProfileArtifacts: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 spec, got %d", len(got))
	}
	m := unmarshalManifest(t, findManifestSpec(got).Inline)
	if len(m.Artifacts) != 1 || m.Artifacts[0].Kind != spec.KindPlugin {
		t.Fatalf("expected 1 plugin artifact, got %v", m.Artifacts)
	}
	if m.Artifacts[0].Plugin == nil {
		t.Fatal("Plugin payload is nil")
	}
}

// TestAssemble_ByRef_URLSkill: a catalog skill entry with SHA256 set (Content nil) produces
// a by-ref payload ArtifactSpec (Objectref populated, Inline empty) and an inline manifest.
func TestAssemble_ByRef_URLSkill(t *testing.T) {
	s, _, _ := newTestServer(t)

	// Insert a URL-ingested skill entry directly into the catalog store.
	sha := "aabbccdd1122334455667788991100aabbccdd1122334455667788991100aabb"
	size := int64(4096)
	sourceURL := "https://github.com/example/my-skill"
	if err := s.st.CustomizationCatalog().CreateSkill(aliceCtx(), store.CustomizationCatalogEntry{
		CatalogID:   "url-skill-1",
		CreatorID:   "alice",
		Kind:        string(store.ProfileEntrySkill),
		Name:        "my-url-skill",
		Description: "ingested from github",
		Listed:      true,
		CreatedAt:   1,
		UpdatedAt:   1,
		Content:     nil, // by-ref: no inline bytes
		SourceURL:   &sourceURL,
		SHA256:      &sha,
		Size:        &size,
	}); err != nil {
		t.Fatalf("CreateSkill: %v", err)
	}

	profileID := createProfile(t, s)
	_, _ = addEntry(t, s, profileID, 1, &cpv1.ProfileEntry{
		Kind:      cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL,
		Name:      "my-url-skill",
		Source:    cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_CATALOG_REF,
		CatalogId: "url-skill-1",
	})

	p, entries := loadProfile(t, s, profileID)
	got, err := s.assembleProfileArtifacts(context.Background(), p, entries)
	if err != nil {
		t.Fatalf("assembleProfileArtifacts: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 specs (manifest + skill TAR), got %d: %v", len(got), got)
	}

	// Manifest spec must still be inline BYTES.
	ms := findManifestSpec(got)
	if ms == nil {
		t.Fatal("no manifest spec in result")
	}
	if ms.ContentType != cpv1.ArtifactContentType_ARTIFACT_CONTENT_TYPE_BYTES {
		t.Errorf("manifest ContentType = %v, want BYTES", ms.ContentType)
	}
	if len(ms.Inline) == 0 {
		t.Error("manifest Inline is empty")
	}
	if ms.Objectref != nil {
		t.Error("manifest must not have objectref")
	}

	// Find the TAR payload spec.
	var tarSpec *cpv1.ArtifactSpec
	for _, s := range got {
		if s.ContentType == cpv1.ArtifactContentType_ARTIFACT_CONTENT_TYPE_TAR {
			tarSpec = s
			break
		}
	}
	if tarSpec == nil {
		t.Fatal("no TAR payload spec in result")
	}
	// By-ref: Inline must be empty, Objectref must be set.
	if len(tarSpec.Inline) != 0 {
		t.Errorf("by-ref TAR spec Inline should be empty, got %d bytes", len(tarSpec.Inline))
	}
	if tarSpec.Objectref == nil {
		t.Fatal("by-ref TAR spec must have Objectref")
	}
	wantKey := "skills/" + sha + ".tar.zst"
	if tarSpec.Objectref.ObjectKey != wantKey {
		t.Errorf("ObjectKey = %q, want %q", tarSpec.Objectref.ObjectKey, wantKey)
	}
	if tarSpec.Objectref.Sha256 != sha {
		t.Errorf("Sha256 = %q, want %q", tarSpec.Objectref.Sha256, sha)
	}
	if tarSpec.Objectref.PresignedUrl != "" {
		t.Error("PresignedUrl should be empty at assembly time (filled at start)")
	}

	// Manifest Artifact.Skill.Dir and Payload must still match the payload dest path.
	m := unmarshalManifest(t, ms.Inline)
	if len(m.Artifacts) != 1 || m.Artifacts[0].Kind != spec.KindSkill {
		t.Fatalf("expected 1 skill artifact in manifest, got %v", m.Artifacts)
	}
	a := m.Artifacts[0]
	expectedDir := tarSpec.DestPath
	if a.Skill == nil || a.Skill.Dir != expectedDir {
		t.Errorf("Skill.Dir = %q, want %q", func() string {
			if a.Skill != nil {
				return a.Skill.Dir
			}
			return "<nil>"
		}(), expectedDir)
	}
	if a.Payload != expectedDir {
		t.Errorf("Artifact.Payload = %q, want %q", a.Payload, expectedDir)
	}
}

// TestAssemble_InlineSkill_Preserved: a catalog skill with Content (no SHA256) still uses
// the inline delivery path — regression guard for the legacy catalog skill branch.
func TestAssemble_InlineSkill_Preserved(t *testing.T) {
	s, _, _ := newTestServer(t)

	// Create an inline catalog skill (no SHA256 = legacy inline path).
	if err := s.st.CustomizationCatalog().Create(aliceCtx(), store.CustomizationCatalogEntry{
		CatalogID:   "inline-skill-1",
		CreatorID:   "alice",
		Kind:        string(store.ProfileEntrySkill),
		Name:        "inline-skill",
		Description: "classic inline",
		Listed:      true,
		CreatedAt:   1,
		UpdatedAt:   1,
		Content:     validSkillTar(),
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	profileID := createProfile(t, s)
	_, _ = addEntry(t, s, profileID, 1, &cpv1.ProfileEntry{
		Kind:      cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL,
		Name:      "inline-skill",
		Source:    cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_CATALOG_REF,
		CatalogId: "inline-skill-1",
	})

	p, entries := loadProfile(t, s, profileID)
	got, err := s.assembleProfileArtifacts(context.Background(), p, entries)
	if err != nil {
		t.Fatalf("assembleProfileArtifacts: %v", err)
	}
	// Should be inline — no objectref.
	var tarSpec *cpv1.ArtifactSpec
	for _, s := range got {
		if s.ContentType == cpv1.ArtifactContentType_ARTIFACT_CONTENT_TYPE_TAR {
			tarSpec = s
			break
		}
	}
	if tarSpec == nil {
		t.Fatal("no TAR payload spec in result")
	}
	if tarSpec.Objectref != nil {
		t.Error("inline skill spec must not have Objectref")
	}
	if len(tarSpec.Inline) == 0 {
		t.Error("inline skill spec must have Inline content")
	}
}

// TestAssemble_DuplicateSkillDirName: two skill entries sharing Name in one profile
// → CodeInvalidArgument.
func TestAssemble_DuplicateSkillDirName(t *testing.T) {
	s, _, _ := newTestServer(t)

	profileID := createProfile(t, s)
	_, ver := addEntry(t, s, profileID, 1, &cpv1.ProfileEntry{
		Kind:         cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL,
		Name:         "same-name",
		Source:       cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_CUSTOM,
		CustomInline: validSkillTar(),
	})
	_, _ = addEntry(t, s, profileID, ver, &cpv1.ProfileEntry{
		Kind:         cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL,
		Name:         "same-name", // duplicate!
		Source:       cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_CUSTOM,
		CustomInline: validSkillTar(),
	})

	p, entries := loadProfile(t, s, profileID)
	_, err := s.assembleProfileArtifacts(context.Background(), p, entries)
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("want CodeInvalidArgument for duplicate skill dir name, got %v", err)
	}
}

// TestAssemble_DistinctSkillDirNames: two skill entries with different names → no error.
func TestAssemble_DistinctSkillDirNames(t *testing.T) {
	s, _, _ := newTestServer(t)

	profileID := createProfile(t, s)
	_, ver := addEntry(t, s, profileID, 1, &cpv1.ProfileEntry{
		Kind:         cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL,
		Name:         "skill-a",
		Source:       cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_CUSTOM,
		CustomInline: validSkillTar(),
	})
	_, _ = addEntry(t, s, profileID, ver, &cpv1.ProfileEntry{
		Kind:         cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_SKILL,
		Name:         "skill-b",
		Source:       cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_CUSTOM,
		CustomInline: validSkillTar(),
	})

	p, entries := loadProfile(t, s, profileID)
	got, err := s.assembleProfileArtifacts(context.Background(), p, entries)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 1 manifest + 2 TAR payloads.
	if len(got) != 3 {
		t.Fatalf("want 3 specs, got %d", len(got))
	}
}

// TestAssemble_spec_LoadManifest: end-to-end: write specs to temp dir, use
// spec.LoadManifest to verify the output is spec-compatible.
func TestAssemble_spec_LoadManifest(t *testing.T) {
	s, _, _ := newTestServer(t)

	mcpPayload := spec.MCPPayload{HTTP: &spec.MCPTransportHTTP{URL: "https://mcp.example.com"}}
	content, _ := json.Marshal(mcpPayload)

	profileID := createProfile(t, s)
	_, _ = addEntry(t, s, profileID, 1, &cpv1.ProfileEntry{
		Kind:         cpv1.ProfileEntryKind_PROFILE_ENTRY_KIND_MCP,
		Name:         "mcp-e2e",
		Source:       cpv1.ProfileEntrySource_PROFILE_ENTRY_SOURCE_CUSTOM,
		CustomInline: content,
	})

	p, entries := loadProfile(t, s, profileID)
	got, err := s.assembleProfileArtifacts(context.Background(), p, entries)
	if err != nil {
		t.Fatalf("assembleProfileArtifacts: %v", err)
	}

	// Write to temp dir and use spec.LoadManifest.
	dir := writeSpecsToDir(t, got)
	m, err := spec.LoadManifest(dir)
	if err != nil {
		t.Fatalf("spec.LoadManifest: %v", err)
	}
	if m.SchemaVersion != spec.CurrentSchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d", m.SchemaVersion, spec.CurrentSchemaVersion)
	}
	if len(m.Artifacts) != 1 {
		t.Fatalf("want 1 artifact, got %d", len(m.Artifacts))
	}
}
