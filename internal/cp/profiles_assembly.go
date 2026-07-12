package cp

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"

	"connectrpc.com/connect"

	cpv1 "spawnery/gen/cp/v1"
	"spawnery/internal/agentinstall"
	"spawnery/internal/agentinstall/spec"
	"spawnery/internal/cp/skillstore"
	"spawnery/internal/cp/store"
)

// resolvedItem is one emitted artifact: for a catalog_ref/custom entry there is exactly one
// resolvedItem per ProfileEntry; for a bundle_ref entry there is one resolvedItem per pinned
// bundle member (sp-mwco.1.5 §4.4/§4.5). For URL-ingested skills (by-ref delivery), sha256 is
// the hex sha256 of the canonical plain tar and content is nil. For inline skills, sha256 is
// empty and content holds bytes.
type resolvedItem struct {
	entry      store.ProfileEntry // owning entry: Kind, Targets, EntryID
	artifactID string             // entry.EntryID, or entry.EntryID+"/"+catalogID for a bundle member
	name       string             // manifest Artifact.Name — the on-disk skill directory name
	content    []byte
	sha256     string // non-empty => by-ref skill (catalog entry with SHA256 provenance)
}

// assembleProfileArtifacts resolves a profile's non-secret entries into wire ArtifactSpecs:
// a manifest.json BYTES artifact + one TAR payload artifact per skill (a bundle_ref entry expands
// to one payload per pinned bundle member). Sensitive/secret artifacts are OUT of scope
// (sp-nrzf.4). Returns (nil, nil) when the profile has no non-secret entries (secrets-only / empty
// profile => no manifest, which is valid).
func (s *Server) assembleProfileArtifacts(ctx context.Context, _ store.Profile, entries []store.ProfileEntry) ([]*cpv1.ArtifactSpec, error) {
	if len(entries) == 0 {
		return nil, nil
	}

	// Build target names from the registered agents.
	reg := agentinstall.NewRegistry(agentinstall.MapEnviron{})
	targetNames := make(map[string]bool)
	for _, name := range reg.Names() {
		targetNames[name] = true
	}

	// Expand each entry into one or more resolvedItems — a bundle_ref entry yields one item per
	// pinned bundle member; catalog_ref/custom entries yield exactly one.
	var items []resolvedItem
	for _, entry := range entries {
		expanded, err := s.resolveProfileEntry(ctx, entry)
		if err != nil {
			return nil, err
		}
		items = append(items, expanded...)
	}

	// Duplicate skill directory name check (sp-nrzf.3.14.5 §4.10; sp-mwco.1.5 §4.5). Runs over
	// the FULLY EXPANDED item set, on item.name — the on-disk directory name a skill will land
	// in (~/.claude/skills/<name>/). For a bundle member this is the member's own catalog Name,
	// NOT the owning bundle entry's display Name; checking entry.Name here would let a colliding
	// bundle sail through assembly and merge payloads on the node at install (unpackTar does not
	// wipe its destination).
	skillDirNames := make(map[string]bool)
	for _, item := range items {
		if item.entry.Kind != store.ProfileEntrySkill {
			continue
		}
		if skillDirNames[item.name] {
			return nil, connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("profile has duplicate skill directory name %q", item.name))
		}
		skillDirNames[item.name] = true
	}

	manifest, payloadSpecs, err := buildManifestAndPayloads(items, targetNames)
	if err != nil {
		return nil, err
	}

	// Encode the manifest and prepend it as a BYTES ArtifactSpec.
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("marshal manifest: %w", err))
	}
	manifestSpec := &cpv1.ArtifactSpec{
		Id:              "manifest",
		Inline:          manifestJSON,
		ContentType:     cpv1.ArtifactContentType_ARTIFACT_CONTENT_TYPE_BYTES,
		TargetContainer: cpv1.ArtifactTarget_ARTIFACT_TARGET_AGENT,
		DestPath:        "manifest.json",
		Mode:            0o644,
	}
	return append([]*cpv1.ArtifactSpec{manifestSpec}, payloadSpecs...), nil
}

// resolveProfileEntry expands one ProfileEntry into its resolvedItems: exactly one for
// catalog_ref/custom, one per pinned bundle member for bundle_ref.
func (s *Server) resolveProfileEntry(ctx context.Context, entry store.ProfileEntry) ([]resolvedItem, error) {
	switch entry.SourceKind {
	case store.ProfileSourceCatalog:
		ce, err := s.st.CustomizationCatalog().Get(ctx, entry.CatalogID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil, connect.NewError(connect.CodeInvalidArgument,
					fmt.Errorf("entry %q: unknown catalog ref %q", entry.Name, entry.CatalogID))
			}
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		content, sha256 := catalogContent(ce)
		return []resolvedItem{{entry: entry, artifactID: entry.EntryID, name: entry.Name, content: content, sha256: sha256}}, nil

	case store.ProfileSourceCustom:
		return []resolvedItem{{entry: entry, artifactID: entry.EntryID, name: entry.Name, content: entry.CustomInline}}, nil

	case store.ProfileSourceBundle:
		return s.resolveBundleEntry(ctx, entry)

	default:
		return nil, connect.NewError(connect.CodeInternal,
			fmt.Errorf("entry %q: unknown source kind %q", entry.Name, entry.SourceKind))
	}
}

// resolveBundleEntry expands a bundle_ref entry into one resolvedItem per member of its pinned
// skill_bundle_version (sp-mwco.1.5 §4.4).
func (s *Server) resolveBundleEntry(ctx context.Context, entry store.ProfileEntry) ([]resolvedItem, error) {
	if entry.VersionID == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("entry %q: bundle_ref entry has no pinned version", entry.Name))
	}
	v, err := s.st.SkillBundles().GetVersion(ctx, entry.VersionID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("entry %q: unknown bundle version %q", entry.Name, entry.VersionID))
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if entry.BundleID != "" && entry.BundleID != v.BundleID {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("entry %q: pinned version %q does not belong to bundle %q", entry.Name, entry.VersionID, entry.BundleID))
	}

	members, err := s.st.SkillBundles().Members(ctx, v.VersionID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if len(members) == 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("entry %q: pinned bundle version has no members", entry.Name))
	}

	items := make([]resolvedItem, 0, len(members))
	for _, m := range members {
		ce, err := s.st.CustomizationCatalog().Get(ctx, m.CatalogID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil, connect.NewError(connect.CodeInvalidArgument,
					fmt.Errorf("entry %q: bundle member %q: unknown catalog ref %q", entry.Name, m.SourceSubdir, m.CatalogID))
			}
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		if store.ProfileEntryKind(ce.Kind) != store.ProfileEntrySkill {
			return nil, connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("entry %q: bundle member %q: not a skill (kind %q)", entry.Name, m.SourceSubdir, ce.Kind))
		}
		content, sha256 := catalogContent(ce)
		items = append(items, resolvedItem{
			entry:      entry,
			artifactID: entry.EntryID + "/" + ce.CatalogID,
			name:       memberDirName(entry, m, ce),
			content:    content,
			sha256:     sha256,
		})
	}
	return items, nil
}

// memberDirName returns the on-disk skill directory name for a bundle member. Today it is simply
// the member's own catalog Name (post-rename-override, this is the on-disk skill dir); this is
// the single seam sp-mwco.1.8 extends with the per-member rename override.
func memberDirName(_ store.ProfileEntry, _ store.SkillBundleMember, ce store.CustomizationCatalogEntry) string {
	return ce.Name
}

// catalogContent picks content vs. by-ref sha256 for a catalog entry: URL-ingested skills carry
// SHA256 provenance (sp-nrzf.3.14.4) and nil Content; inline catalog entries carry Content and a
// nil/empty SHA256.
func catalogContent(ce store.CustomizationCatalogEntry) (content []byte, sha256 string) {
	if ce.SHA256 != nil && *ce.SHA256 != "" {
		return nil, *ce.SHA256
	}
	return ce.Content, ""
}

// buildManifestAndPayloads turns resolvedItems into the canonical Manifest + TAR payload specs;
// targetNames is agentinstall.NewRegistry(env).Names(). Returns the Manifest struct and only the
// skill TAR payload ArtifactSpecs (the caller is responsible for JSON-encoding the manifest and
// prepending it as a BYTES spec).
func buildManifestAndPayloads(items []resolvedItem, targetNames map[string]bool) (spec.Manifest, []*cpv1.ArtifactSpec, error) {
	// Build the union of forbidden config keys across all agent layouts.
	reg := agentinstall.NewRegistry(agentinstall.MapEnviron{})
	forbiddenKeys := make(map[string]bool)
	for _, layout := range reg.Layouts() {
		for _, k := range layout.ForbiddenConfigKeys {
			forbiddenKeys[k] = true
		}
	}

	var artifacts []spec.Artifact
	var payloadSpecs []*cpv1.ArtifactSpec
	// Disjointness assertion (sp-mwco.1.5 §4.5): the last line of defence in front of
	// unpackTar, which does not wipe its destination — two payloads sharing a DestPath would
	// silently merge on the node. A collision here is an assembly invariant violation, not a
	// caller input error (the duplicate-name check above should have already caught any
	// same-named skills; this also catches a same-content-dedup catalog_id reused by two
	// distinct bundle members with different display names).
	seenIDs := make(map[string]bool)
	seenDest := make(map[string]bool)

	for _, item := range items {
		entry := item.entry
		content := item.content

		// Translate targets.
		targets, err := translateTargets(entry.Targets, targetNames)
		if err != nil {
			return spec.Manifest{}, nil, connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("entry %q: %w", item.name, err))
		}

		a := spec.Artifact{
			Name:    item.name,
			Kind:    spec.Kind(entry.Kind),
			Targets: targets,
		}
		if entry.SourceKind == store.ProfileSourceBundle {
			a.Bundle = entry.EntryID
		}

		switch entry.Kind {
		case store.ProfileEntrySkill:
			payloadPath := "payloads/" + item.artifactID
			if seenIDs[item.artifactID] || seenDest[payloadPath] {
				return spec.Manifest{}, nil, connect.NewError(connect.CodeInternal,
					fmt.Errorf("assembly produced duplicate artifact destination %q", payloadPath))
			}
			seenIDs[item.artifactID] = true
			seenDest[payloadPath] = true

			a.Skill = &spec.SkillPayload{Dir: payloadPath}
			a.Payload = payloadPath
			if item.sha256 != "" {
				// By-ref delivery path: URL-ingested skill stored in Garage (sp-nrzf.3.14.5).
				// Skip validateSkillTar — validation was done at ingest time.
				// PresignedUrl is left empty here; the CP fills it at StartSpawn via presignNodeArtifacts.
				payloadSpecs = append(payloadSpecs, &cpv1.ArtifactSpec{
					Id:              item.artifactID,
					ContentType:     cpv1.ArtifactContentType_ARTIFACT_CONTENT_TYPE_TAR,
					TargetContainer: cpv1.ArtifactTarget_ARTIFACT_TARGET_AGENT,
					DestPath:        payloadPath,
					Objectref: &cpv1.ObjectRef{
						ObjectKey: skillstore.ObjectKey(item.sha256),
						Sha256:    item.sha256,
					},
				})
			} else {
				// Inline delivery path: legacy catalog skill with Content bytes, or custom inline.
				if err := validateSkillTar(content, item.name); err != nil {
					return spec.Manifest{}, nil, err
				}
				payloadSpecs = append(payloadSpecs, &cpv1.ArtifactSpec{
					Id:              item.artifactID,
					Inline:          content,
					ContentType:     cpv1.ArtifactContentType_ARTIFACT_CONTENT_TYPE_TAR,
					TargetContainer: cpv1.ArtifactTarget_ARTIFACT_TARGET_AGENT,
					DestPath:        payloadPath,
				})
			}

		case store.ProfileEntryMCP:
			var payload spec.MCPPayload
			if err := json.Unmarshal(content, &payload); err != nil {
				return spec.Manifest{}, nil, connect.NewError(connect.CodeInvalidArgument,
					fmt.Errorf("entry %q: malformed MCP content: %w", item.name, err))
			}
			// Thread MCPSecretRefs from the entry metadata.
			payload.SecretRefs = entry.MCPSecretRefs
			a.MCP = &payload

		case store.ProfileEntryConfig:
			var payload spec.ConfigPayload
			if err := json.Unmarshal(content, &payload); err != nil {
				return spec.Manifest{}, nil, connect.NewError(connect.CodeInvalidArgument,
					fmt.Errorf("entry %q: malformed Config content: %w", item.name, err))
			}
			if err := validateConfigPayload(item.name, &payload, forbiddenKeys); err != nil {
				return spec.Manifest{}, nil, err
			}
			a.Config = &payload

		case store.ProfileEntryPlugin:
			var payload spec.PluginPayload
			if err := json.Unmarshal(content, &payload); err != nil {
				return spec.Manifest{}, nil, connect.NewError(connect.CodeInvalidArgument,
					fmt.Errorf("entry %q: malformed Plugin content: %w", item.name, err))
			}
			a.Plugin = &payload

		default:
			return spec.Manifest{}, nil, connect.NewError(connect.CodeInternal,
				fmt.Errorf("entry %q: unknown kind %q", item.name, entry.Kind))
		}

		artifacts = append(artifacts, a)
	}

	manifest := spec.Manifest{
		SchemaVersion: spec.CurrentSchemaVersion,
		Artifacts:     artifacts,
	}
	return manifest, payloadSpecs, nil
}

// translateTargets normalizes the entry's target list:
//   - empty list or list containing "all" → ["all-detected"]
//   - explicit list: each name must appear in targetNames (else error)
func translateTargets(targets []string, targetNames map[string]bool) ([]string, error) {
	if len(targets) == 0 {
		return []string{"all-detected"}, nil
	}
	for _, t := range targets {
		if t == "all" {
			return []string{"all-detected"}, nil
		}
	}
	// Validate each explicit name.
	for _, t := range targets {
		if !targetNames[t] {
			return nil, fmt.Errorf("unknown target agent %q", t)
		}
	}
	out := make([]string, len(targets))
	copy(out, targets)
	return out, nil
}

// validateSkillTar checks that content is a readable TAR archive containing a
// top-level SKILL.md entry. Returns a Connect CodeInvalidArgument error on failure.
func validateSkillTar(content []byte, entryName string) error {
	tr := tar.NewReader(bytes.NewReader(content))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("entry %q: unreadable skill TAR: %w", entryName, err))
		}
		// Normalize: strip "./" prefix and clean the path to match how spawnlet unpacks.
		name := path.Clean(hdr.Name)
		if name == "SKILL.md" {
			return nil
		}
	}
	return connect.NewError(connect.CodeInvalidArgument,
		fmt.Errorf("entry %q: skill TAR missing top-level SKILL.md", entryName))
}

// validateConfigPayload enforces defense-in-depth config key rules on the CP side:
//   - no forbidden key in normalized (those are launcher-reserved)
//   - no unknown key in normalized (catches typos + injection attempts)
//   - no forbidden key in any agent's native fragment
func validateConfigPayload(entryName string, cfg *spec.ConfigPayload, forbiddenKeys map[string]bool) error {
	for k := range cfg.Normalized {
		if forbiddenKeys[k] {
			return connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("entry %q: forbidden config key %q in normalized (launcher-reserved)", entryName, k))
		}
		if !agentinstall.ValidNormalizedConfigKeys[k] {
			return connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("entry %q: unknown normalized config key %q", entryName, k))
		}
	}
	// Check native passthrough for forbidden keys across all agents.
	for agentName, rawFragment := range cfg.Native {
		fragment, ok := rawFragment.(map[string]interface{})
		if !ok {
			continue // skip non-map values — engine will also skip them
		}
		for k := range fragment {
			if forbiddenKeys[k] {
				return connect.NewError(connect.CodeInvalidArgument,
					fmt.Errorf("entry %q: forbidden config key %q in native[%q] (launcher-reserved)", entryName, k, agentName))
			}
		}
	}
	return nil
}
