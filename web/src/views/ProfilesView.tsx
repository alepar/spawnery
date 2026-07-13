import { useEffect, useState } from "react";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
  DialogDescription,
  DialogFooter,
  DialogClose,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import {
  listProfiles,
  createProfile,
  getProfile,
  updateProfile,
  deleteProfile,
  addProfileEntry,
  removeProfileEntry,
  listCatalogEntries,
  ingestSkillFromURL,
  connectErrorMessage,
  KIND_LABEL,
  kindToCapKind,
  type ProfileSummary,
  type Profile,
  type ProfileEntry,
  type CatalogEntrySummary,
  type ProfileEntryKind,
  type ProfileEntrySource,
} from "@/api/profiles";
import {
  listBundles,
  getBundle,
  reingestBundle,
  repinProfileBundle,
  type BundleSummary,
  type BundleVersion,
  type BundleMember,
} from "@/api/bundles";
import { AGENTS, capabilityFor, type CapabilityStatus } from "@/api/capabilities";
import { BundleCard } from "@/views/profiles/BundleCard";
import { BundleAttachPanel, type BundleAttachOverrides } from "@/views/profiles/BundleAttachPanel";
import { BundleEntryChip } from "@/views/profiles/BundleEntryChip";

// --- Capability preview -------------------------------------------------------

const STATUS_COLOR: Record<CapabilityStatus, string> = {
  supported:    "bg-green-100 text-green-800 dark:bg-green-900/40 dark:text-green-300",
  "no-op":      "bg-zinc-100 text-zinc-500 dark:bg-zinc-800 dark:text-zinc-400",
  "best-effort":"bg-amber-100 text-amber-700 dark:bg-amber-900/40 dark:text-amber-300",
};

function CapabilityPreview({ entry }: { entry: ProfileEntry }) {
  const capKind = kindToCapKind(entry.kind);
  // targets: empty → all AGENTS
  const targets = (entry.targets && entry.targets.length > 0) ? entry.targets : AGENTS;
  return (
    <div data-testid={`cap-preview-${entry.entryId}`} className="flex flex-wrap gap-1 mt-1">
      {targets.map((agent) => {
        const status = capabilityFor(capKind, agent);
        return (
          <span
            key={agent}
            data-testid={`cap-badge-${entry.entryId}-${agent}`}
            data-status={status}
            className={`rounded px-1.5 py-0.5 text-xs font-medium ${STATUS_COLOR[status]}`}
          >
            {agent}
          </span>
        );
      })}
    </div>
  );
}

// --- Bundle attach panel state (shared by the ingest flow and the catalog "Attach" flow) -----

interface AttachPanelState {
  bundleId: string;
  name: string;
  members: BundleMember[];
  skippedEntries: string[];
  warnings: string[];
  changed?: boolean;
}

// --- ProfilesView -------------------------------------------------------------

export function ProfilesView() {
  const [profiles, setProfiles] = useState<ProfileSummary[]>([]);
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [profile, setProfile] = useState<Profile | null>(null);
  const [loadingList, setLoadingList] = useState(false);
  const [loadingProfile, setLoadingProfile] = useState(false);

  // Create form state
  const [createName, setCreateName] = useState("");
  const [creating, setCreating] = useState(false);

  // Rename state
  const [renameDraft, setRenameDraft] = useState("");
  const [renaming, setRenaming] = useState(false);

  // Add entry form state
  const [catalogEntries, setCatalogEntries] = useState<CatalogEntrySummary[]>([]);
  const [showAddCatalog, setShowAddCatalog] = useState(false);
  const [showAddCustom, setShowAddCustom] = useState(false);
  // SKILL is excluded from the custom form: the CP validates custom inline SKILL entries
  // as a tar archive, which a plain textarea cannot produce. Skills get the URL ingest path.
  const [customKind, setCustomKind] = useState<ProfileEntryKind>("PROFILE_ENTRY_KIND_MCP");
  const [customName, setCustomName] = useState("");
  const [customInline, setCustomInline] = useState("");
  const [addingEntry, setAddingEntry] = useState(false);

  // Bundles (sp-mwco.1.9 §4.7): the catalog picker's "Bundles" section — one BundleCard per
  // bundle, fed by ListBundles + a GetBundle per card for provenance/members.
  const [bundles, setBundles] = useState<BundleSummary[]>([]);
  const [bundleDetails, setBundleDetails] = useState<Record<string, { versions: BundleVersion[]; members: BundleMember[] }>>({});

  // Add skill from URL dialog state — also doubles as the "Attach an existing bundle" dialog
  // (opened from a BundleCard's Attach button): when attachPanel is set, the dialog shows
  // BundleAttachPanel instead of the URL form (D2 — attach is an explicit second step, an
  // ingest that discovers 40 skills must not silently attach 40).
  const [showAddSkill, setShowAddSkill] = useState(false);
  const [skillUrl, setSkillUrl] = useState("");
  const [skillRef, setSkillRef] = useState("");
  const [skillSubdir, setSkillSubdir] = useState("");
  const [skillName, setSkillName] = useState("");
  const [skillDescription, setSkillDescription] = useState("");
  const [ingestingSkill, setIngestingSkill] = useState(false);
  const [attachPanel, setAttachPanel] = useState<AttachPanelState | null>(null);
  const [attachingBundle, setAttachingBundle] = useState(false);

  const refreshList = async () => {
    setLoadingList(true);
    try {
      const ps = await listProfiles();
      setProfiles(ps);
    } catch (e: any) {
      toast.error("Failed to load profiles: " + e.message);
    } finally {
      setLoadingList(false);
    }
  };

  const refreshProfile = async (id: string) => {
    setLoadingProfile(true);
    try {
      const p = await getProfile(id);
      setProfile(p);
      setRenameDraft(p.name);
    } catch (e: any) {
      toast.error("Failed to load profile: " + e.message);
    } finally {
      setLoadingProfile(false);
    }
  };

  const refreshBundles = async () => {
    try {
      const list = await listBundles();
      setBundles(list);
      const entries = await Promise.all(list.map(async (b) => {
        try {
          const d = await getBundle(b.bundleId);
          return [b.bundleId, { versions: d.versions, members: d.members }] as const;
        } catch {
          return [b.bundleId, { versions: [], members: [] }] as const;
        }
      }));
      setBundleDetails(Object.fromEntries(entries));
    } catch {
      // best-effort, same posture as listCatalogEntries below
    }
  };

  useEffect(() => { refreshList(); }, []);

  useEffect(() => {
    if (selectedId) {
      refreshProfile(selectedId);
      listCatalogEntries().then(setCatalogEntries).catch(() => {});
      refreshBundles();
    } else {
      setProfile(null);
    }
  }, [selectedId]);

  const handleCreate = async () => {
    const name = createName.trim();
    if (!name) return;
    setCreating(true);
    try {
      const r = await createProfile(name);
      setCreateName("");
      await refreshList();
      setSelectedId(r.profileId);
    } catch (e: any) {
      toast.error("Create failed: " + e.message);
    } finally {
      setCreating(false);
    }
  };

  const handleClone = async () => {
    if (!profile) return;
    setCreating(true);
    try {
      const r = await createProfile(profile.name + " copy");
      // Replay all entries from the source profile
      let version = r.version;
      let pinnedNewer = false;
      for (const entry of profile.entries) {
        if (entry.source === "PROFILE_ENTRY_SOURCE_BUNDLE_REF") {
          // Clone replays a BUNDLE_REF entry with no versionId ⇒ the new profile pins LATEST
          // (sp-mwco.1.9 D7). Dropping bundle entries from a clone would be worse than pinning
          // newer, so we do the latter and flag it if the source was behind.
          if ((entry.latestSeq ?? 0) > (entry.pinnedSeq ?? 0)) pinnedNewer = true;
          const entryInput: Omit<ProfileEntry, "entryId"> = {
            kind: entry.kind,
            name: entry.name,
            source: entry.source,
            bundleId: entry.bundleId,
            excludedSubdirs: entry.excludedSubdirs,
            memberRenames: entry.memberRenames,
            targets: entry.targets,
          };
          const added = await addProfileEntry(r.profileId, version, entryInput);
          version = added.version;
          continue;
        }
        const entryInput: Omit<ProfileEntry, "entryId"> = {
          kind: entry.kind,
          name: entry.name,
          source: entry.source,
          catalogId: entry.catalogId,
          customInline: entry.customInline,
          targets: entry.targets,
          mcpSecretRefs: entry.mcpSecretRefs,
        };
        const added = await addProfileEntry(r.profileId, version, entryInput);
        version = added.version;
      }
      if (pinnedNewer) {
        toast.success("Profile cloned — one or more bundles were pinned to their newer version");
      }
      await refreshList();
      setSelectedId(r.profileId);
    } catch (e: any) {
      toast.error("Clone failed: " + e.message);
    } finally {
      setCreating(false);
    }
  };

  const handleDelete = async () => {
    if (!selectedId) return;
    try {
      await deleteProfile(selectedId);
      setSelectedId(null);
      await refreshList();
    } catch (e: any) {
      toast.error("Delete failed: " + e.message);
    }
  };

  const handleRename = async () => {
    if (!profile || !renameDraft.trim() || renameDraft.trim() === profile.name) return;
    setRenaming(true);
    try {
      const r = await updateProfile(profile.profileId, profile.version, renameDraft.trim());
      setProfile((p) => p ? { ...p, name: renameDraft.trim(), version: r.version } : p);
      await refreshList();
    } catch (e: any) {
      // CAS error: refetch
      toast.error("Rename failed: " + e.message);
      if (selectedId) refreshProfile(selectedId);
    } finally {
      setRenaming(false);
    }
  };

  const handleAddCatalogEntry = async (catalogEntry: CatalogEntrySummary) => {
    if (!profile) return;
    setAddingEntry(true);
    try {
      const r = await addProfileEntry(profile.profileId, profile.version, {
        kind: catalogEntry.kind,
        name: catalogEntry.name,
        source: "PROFILE_ENTRY_SOURCE_CATALOG_REF" as ProfileEntrySource,
        catalogId: catalogEntry.catalogId,
      });
      setProfile((p) => p ? { ...p, version: r.version } : p);
      setShowAddCatalog(false);
      if (selectedId) await refreshProfile(selectedId);
    } catch (e: any) {
      toast.error("Add entry failed: " + e.message);
      if (selectedId) refreshProfile(selectedId);
    } finally {
      setAddingEntry(false);
    }
  };

  const handleAddCustomEntry = async () => {
    if (!profile || !customName.trim()) return;
    setAddingEntry(true);
    try {
      const r = await addProfileEntry(profile.profileId, profile.version, {
        kind: customKind,
        name: customName.trim(),
        source: "PROFILE_ENTRY_SOURCE_CUSTOM" as ProfileEntrySource,
        customInline: customInline,
      });
      setProfile((p) => p ? { ...p, version: r.version } : p);
      setCustomName("");
      setCustomInline("");
      setShowAddCustom(false);
      if (selectedId) await refreshProfile(selectedId);
    } catch (e: any) {
      toast.error("Add entry failed: " + e.message);
      if (selectedId) refreshProfile(selectedId);
    } finally {
      setAddingEntry(false);
    }
  };

  const handleRemoveEntry = async (entryId: string) => {
    if (!profile) return;
    try {
      const r = await removeProfileEntry(profile.profileId, profile.version, entryId);
      setProfile((p) => p ? { ...p, version: r.version, entries: p.entries.filter((e) => e.entryId !== entryId) } : p);
    } catch (e: any) {
      toast.error("Remove entry failed: " + e.message);
      if (selectedId) refreshProfile(selectedId);
    }
  };

  // handleUpdateTargets is remove+re-add (there is no UpdateProfileEntry RPC yet). It is never
  // invoked for a BUNDLE_REF entry — those render BundleEntryChip, which has no target toggles
  // (sp-mwco.1.9 D6): a re-add would silently pin the bundle to LATEST, exactly the un-diffed
  // update channel §4.9 forbids.
  const handleUpdateTargets = async (entry: ProfileEntry, targets: string[]) => {
    if (!profile) return;
    try {
      // Remove the old entry and re-add with updated targets.
      // Two-step CAS: if the add leg fails after remove succeeds, attempt to restore
      // the original entry to avoid silent data loss.
      const removeResult = await removeProfileEntry(profile.profileId, profile.version, entry.entryId);
      const entryInput: Omit<ProfileEntry, "entryId"> = {
        kind: entry.kind,
        name: entry.name,
        source: entry.source,
        catalogId: entry.catalogId,
        customInline: entry.customInline,
        targets,
        mcpSecretRefs: entry.mcpSecretRefs,
      };
      try {
        await addProfileEntry(profile.profileId, removeResult.version, entryInput);
      } catch (addErr: any) {
        // Add failed after remove succeeded — attempt to restore the original entry.
        const restoreInput: Omit<ProfileEntry, "entryId"> = { ...entryInput, targets: entry.targets };
        try {
          await addProfileEntry(profile.profileId, removeResult.version, restoreInput);
          toast.error("Update targets failed (entry restored): " + addErr.message);
        } catch {
          toast.error("Update targets failed and restore failed — entry may be lost: " + addErr.message);
        }
        if (selectedId) await refreshProfile(selectedId);
        return;
      }
      if (selectedId) await refreshProfile(selectedId);
    } catch (e: any) {
      toast.error("Update targets failed: " + e.message);
      if (selectedId) refreshProfile(selectedId);
    }
  };

  const resetAddSkillDialog = () => {
    setShowAddSkill(false);
    setSkillUrl("");
    setSkillRef("");
    setSkillSubdir("");
    setSkillName("");
    setSkillDescription("");
    setAttachPanel(null);
  };

  const handleIngestSkill = async () => {
    if (!profile || !skillUrl.trim()) return;
    setIngestingSkill(true);
    try {
      const r = await ingestSkillFromURL({
        url: skillUrl.trim(),
        ref: skillRef.trim() || undefined,
        subdir: skillSubdir.trim() || undefined,
        name: skillName.trim() || undefined,
        description: skillDescription.trim() || undefined,
      });
      // Every ingest yields a bundle (bundle-of-one for a single-skill repo). Attach is always
      // BUNDLE_REF (sp-mwco.1.9 D1) and always an explicit second step (D2) — swap the dialog to
      // the attach-result panel rather than auto-attaching, so an ingest that discovers 40
      // skills can't silently attach 40.
      const detail = await getBundle(r.bundleId);
      setAttachPanel({
        bundleId: r.bundleId,
        name: detail.bundle.name,
        members: detail.members,
        skippedEntries: r.skippedEntries,
        warnings: r.warnings,
        changed: r.changed,
      });
    } catch (e: unknown) {
      toast.error(connectErrorMessage(e));
    } finally {
      setIngestingSkill(false);
    }
  };

  const handleOpenAttachForBundle = (bundleId: string) => {
    const summary = bundles.find((b) => b.bundleId === bundleId);
    const detail = bundleDetails[bundleId];
    if (!summary || !detail) return;
    setAttachPanel({
      bundleId,
      name: summary.name,
      members: detail.members,
      skippedEntries: [],
      warnings: [],
      changed: undefined,
    });
    setShowAddSkill(true);
  };

  const handleAttachBundle = async (overrides: BundleAttachOverrides) => {
    if (!profile || !attachPanel) return;
    setAttachingBundle(true);
    try {
      const r = await addProfileEntry(profile.profileId, profile.version, {
        kind: "PROFILE_ENTRY_KIND_SKILL" as ProfileEntryKind,
        name: attachPanel.name,
        source: "PROFILE_ENTRY_SOURCE_BUNDLE_REF" as ProfileEntrySource,
        bundleId: attachPanel.bundleId,
        excludedSubdirs: overrides.excludedSubdirs,
        memberRenames: overrides.memberRenames,
      });
      setProfile((p) => p ? { ...p, version: r.version } : p);
      if (r.warnings.length > 0) {
        for (const w of r.warnings) toast.warning(w);
      } else {
        toast.success("Bundle attached");
      }
      resetAddSkillDialog();
      if (selectedId) {
        await refreshProfile(selectedId);
        await refreshBundles();
      }
    } catch (e: unknown) {
      toast.error("Attach failed: " + connectErrorMessage(e));
    } finally {
      setAttachingBundle(false);
    }
  };

  const handleBundleCheckUpdates = async (bundleId: string) => {
    try {
      const r = await reingestBundle(bundleId);
      if (r.notModified || !r.changed) {
        toast.success("Bundle is up to date");
      } else {
        toast.success(
          `Bundle updated: ${r.addedSubdirs.length} added, ${r.updatedSubdirs.length} updated, ${r.removedSubdirs.length} removed`,
        );
      }
      await refreshBundles();
    } catch (e: unknown) {
      toast.error("Check for updates failed: " + connectErrorMessage(e));
    }
  };

  const handleRepinEntry = async (entryId: string, versionId: string, diffToken: string) => {
    if (!profile) return;
    const r = await repinProfileBundle(profile.profileId, entryId, versionId, diffToken, profile.version);
    setProfile((p) => p ? { ...p, version: r.version } : p);
    if (r.warnings.length > 0) {
      for (const w of r.warnings) toast.warning(w);
    } else {
      toast.success("Bundle re-pinned");
    }
    if (selectedId) await refreshProfile(selectedId);
  };

  const visibleCatalogEntries = catalogEntries.filter((ce) => !ce.bundleMember);

  return (
    <div data-testid="profiles" className="flex h-full">
      {/* Left panel: profile list */}
      <div className="flex w-64 flex-col gap-2 border-r border-border p-4">
        <div className="text-sm font-semibold">Profiles</div>

        {/* Create new */}
        <div className="flex gap-1">
          <input
            data-testid="profile-name-input"
            className="flex-1 rounded border border-input bg-background px-2 py-1 text-sm"
            placeholder="New profile name"
            value={createName}
            onChange={(e) => setCreateName(e.target.value)}
            onKeyDown={(e) => { if (e.key === "Enter") handleCreate(); }}
          />
          <Button
            data-testid="profile-create-btn"
            size="sm"
            disabled={creating || !createName.trim()}
            onClick={handleCreate}
          >
            Create
          </Button>
        </div>

        {loadingList && <div className="text-xs text-muted-foreground">Loading…</div>}

        {/* Profile list */}
        <div className="flex flex-col gap-1">
          {profiles.map((p) => (
            <div
              key={p.profileId}
              data-testid={`profile-item-${p.profileId}`}
              className={`flex items-center justify-between rounded px-2 py-1 cursor-pointer text-sm ${selectedId === p.profileId ? "bg-secondary" : "hover:bg-accent"}`}
              onClick={() => setSelectedId(p.profileId)}
            >
              <span className="truncate">{p.name}</span>
            </div>
          ))}
        </div>
      </div>

      {/* Right panel: profile editor */}
      <div className="flex-1 overflow-auto p-4">
        {!selectedId && (
          <div className="text-sm text-muted-foreground">Select a profile to edit.</div>
        )}

        {selectedId && loadingProfile && (
          <div className="text-sm text-muted-foreground">Loading…</div>
        )}

        {selectedId && profile && !loadingProfile && (
          <div className="flex flex-col gap-4">
            {/* Header: rename + clone + delete */}
            <div className="flex items-center gap-2">
              <input
                data-testid="profile-rename-input"
                className="flex-1 rounded border border-input bg-background px-2 py-1 text-sm font-medium"
                value={renameDraft}
                onChange={(e) => setRenameDraft(e.target.value)}
                onKeyDown={(e) => { if (e.key === "Enter") handleRename(); }}
              />
              <Button
                data-testid="profile-rename-btn"
                size="sm"
                variant="outline"
                disabled={renaming || !renameDraft.trim() || renameDraft.trim() === profile.name}
                onClick={handleRename}
              >
                Rename
              </Button>
              <Button
                data-testid="profile-clone-btn"
                size="sm"
                variant="outline"
                disabled={creating}
                onClick={handleClone}
              >
                Clone
              </Button>
              <Button
                data-testid="profile-delete-btn"
                size="sm"
                variant="outline"
                onClick={handleDelete}
              >
                Delete
              </Button>
            </div>

            {/* Entries */}
            <div className="flex flex-col gap-2">
              <div className="text-sm font-semibold">Entries</div>

              {profile.entries.length === 0 && (
                <div className="text-xs text-muted-foreground">No entries yet.</div>
              )}

              {profile.entries.map((entry) => (
                entry.source === "PROFILE_ENTRY_SOURCE_BUNDLE_REF" ? (
                  <BundleEntryChip
                    key={entry.entryId}
                    entry={entry}
                    onRemove={handleRemoveEntry}
                    onRepin={handleRepinEntry}
                  />
                ) : (
                <div
                  key={entry.entryId}
                  data-testid={`entry-${entry.entryId}`}
                  className="rounded border border-border p-3 flex flex-col gap-1"
                >
                  <div className="flex items-center justify-between gap-2">
                    <div className="flex items-center gap-2">
                      <span
                        data-testid={`entry-kind-${entry.entryId}`}
                        className="rounded bg-muted px-1.5 py-0.5 text-xs font-medium"
                      >
                        {KIND_LABEL[entry.kind]}
                      </span>
                      <span className="text-sm font-medium">{entry.name}</span>
                    </div>
                    <Button
                      data-testid={`entry-remove-${entry.entryId}`}
                      size="sm"
                      variant="ghost"
                      onClick={() => handleRemoveEntry(entry.entryId)}
                    >
                      Remove
                    </Button>
                  </div>

                  {/* Targets multi-select */}
                  <div className="flex flex-wrap gap-1 mt-1">
                    {AGENTS.map((agent) => {
                      const active = !entry.targets || entry.targets.length === 0 || entry.targets.includes(agent);
                      return (
                        <button
                          key={agent}
                          data-testid={`target-${entry.entryId}-${agent}`}
                          data-active={active}
                          className={`rounded px-1.5 py-0.5 text-xs border ${active ? "border-primary bg-primary/10 text-primary" : "border-border text-muted-foreground"}`}
                          onClick={() => {
                            const current = (entry.targets && entry.targets.length > 0) ? entry.targets : AGENTS;
                            const next = current.includes(agent)
                              ? current.filter((a) => a !== agent)
                              : [...current, agent];
                            // If all agents selected, use empty (= all)
                            const targets = next.length === AGENTS.length ? [] : next;
                            handleUpdateTargets(entry, targets);
                          }}
                        >
                          {agent}
                        </button>
                      );
                    })}
                  </div>

                  {/* Capability preview */}
                  <CapabilityPreview entry={entry} />
                </div>
                )
              ))}
            </div>

            {/* Add from catalog */}
            <div className="flex gap-2">
              <Button
                data-testid="add-catalog-btn"
                size="sm"
                variant="outline"
                onClick={() => { setShowAddCatalog((v) => !v); setShowAddCustom(false); }}
              >
                Add from catalog
              </Button>
              <Button
                data-testid="add-custom-btn"
                size="sm"
                variant="outline"
                onClick={() => { setShowAddCustom((v) => !v); setShowAddCatalog(false); }}
              >
                Add custom
              </Button>
              <Button
                data-testid="add-skill-url-btn"
                size="sm"
                variant="outline"
                onClick={() => setShowAddSkill(true)}
              >
                Add skill from URL
              </Button>
            </div>

            {/* Catalog picker */}
            {showAddCatalog && (
              <div data-testid="catalog-picker" className="flex flex-col gap-2 rounded border border-border p-3">
                <div className="text-sm font-semibold">Catalog entries</div>
                {visibleCatalogEntries.length === 0 && (
                  <div className="text-xs text-muted-foreground">No catalog entries.</div>
                )}
                {visibleCatalogEntries.map((ce) => (
                  <div key={ce.catalogId} className="flex items-center justify-between gap-2">
                    <div>
                      <span className="text-sm font-medium">{ce.name}</span>
                      {ce.description && (
                        <span className="ml-2 text-xs text-muted-foreground">{ce.description}</span>
                      )}
                      {ce.sourceUrl && (
                        <div
                          data-testid={`catalog-provenance-${ce.catalogId}`}
                          className="mt-0.5 flex flex-wrap items-center gap-x-2 text-xs text-muted-foreground"
                        >
                          {ce.sourceUrl.startsWith("https://") || ce.sourceUrl.startsWith("http://") ? (
                            <a
                              href={ce.sourceUrl}
                              target="_blank"
                              rel="noopener noreferrer"
                              className="underline"
                            >
                              {ce.sourceUrl}
                            </a>
                          ) : (
                            <span>{ce.sourceUrl}</span>
                          )}
                          {ce.sourceRef && <span>ref {ce.sourceRef}</span>}
                          {ce.sourceSubdir && <span>subdir {ce.sourceSubdir}</span>}
                          {ce.sourceCommit && (
                            <span title={ce.sourceCommit}>commit {ce.sourceCommit.slice(0, 12)}</span>
                          )}
                          {ce.sha256 && (
                            <span title={ce.sha256}>content sha256 {ce.sha256.slice(0, 12)}</span>
                          )}
                          {ce.size && <span>{ce.size} bytes</span>}
                          {ce.bundleMember && (
                            <span className="rounded bg-muted px-1">bundle member</span>
                          )}
                        </div>
                      )}
                    </div>
                    <Button
                      data-testid={`add-catalog-entry-${ce.catalogId}`}
                      size="sm"
                      disabled={addingEntry}
                      onClick={() => handleAddCatalogEntry(ce)}
                    >
                      Add
                    </Button>
                  </div>
                ))}

                {bundles.length > 0 && (
                  <div data-testid="catalog-bundles" className="mt-2 flex flex-col gap-2 border-t border-border pt-2">
                    <div className="text-sm font-semibold">Bundles</div>
                    {bundles.map((b) => (
                      <BundleCard
                        key={b.bundleId}
                        summary={b}
                        versions={bundleDetails[b.bundleId]?.versions ?? []}
                        members={bundleDetails[b.bundleId]?.members ?? []}
                        onAttach={handleOpenAttachForBundle}
                        onCheckUpdates={handleBundleCheckUpdates}
                      />
                    ))}
                  </div>
                )}
              </div>
            )}

            {/* Custom entry form */}
            {showAddCustom && (
              <div data-testid="custom-entry-form" className="flex flex-col gap-2 rounded border border-border p-3">
                <div className="text-sm font-semibold">Add custom entry</div>
                <select
                  data-testid="custom-kind-select"
                  aria-label="Entry kind"
                  value={customKind}
                  onChange={(e) => setCustomKind(e.target.value as ProfileEntryKind)}
                  className="rounded border border-input bg-background px-2 py-1 text-sm"
                >
                  {/* SKILL is omitted: CP validates custom inline Skills as a tar archive,
                      which a free-text textarea cannot produce. Use "Add skill from URL" for Skills. */}
                  <option value="PROFILE_ENTRY_KIND_MCP">MCP</option>
                  <option value="PROFILE_ENTRY_KIND_CONFIG">Config</option>
                  <option value="PROFILE_ENTRY_KIND_PLUGIN">Plugin</option>
                </select>
                <input
                  data-testid="custom-name-input"
                  className="rounded border border-input bg-background px-2 py-1 text-sm"
                  placeholder="Entry name"
                  value={customName}
                  onChange={(e) => setCustomName(e.target.value)}
                />
                <textarea
                  data-testid="custom-inline-input"
                  className="rounded border border-input bg-background px-2 py-1 text-sm font-mono"
                  placeholder="Inline content (optional)"
                  rows={4}
                  value={customInline}
                  onChange={(e) => setCustomInline(e.target.value)}
                />
                <Button
                  data-testid="custom-entry-submit"
                  size="sm"
                  disabled={addingEntry || !customName.trim()}
                  onClick={handleAddCustomEntry}
                >
                  Add
                </Button>
              </div>
            )}

            {/* Add skill from URL dialog — swaps to BundleAttachPanel once ingested, or when
                opened directly from a BundleCard's Attach button (attachPanel set, D2). */}
            <Dialog open={showAddSkill} onOpenChange={(open) => { if (!open) resetAddSkillDialog(); }}>
              <DialogContent data-testid="add-skill-dialog">
                {!attachPanel ? (
                  <>
                    <DialogHeader>
                      <DialogTitle>Add skill from GitHub URL</DialogTitle>
                      <DialogDescription>
                        Enter the GitHub repository URL for the skill. The repository must contain a SKILL.md file.
                      </DialogDescription>
                    </DialogHeader>
                    <div className="flex flex-col gap-3">
                      <div className="flex flex-col gap-1">
                        <label className="text-sm font-medium" htmlFor="skill-url-input">URL <span className="text-destructive">*</span></label>
                        <Input
                          id="skill-url-input"
                          data-testid="skill-url-input"
                          placeholder="https://github.com/owner/repo"
                          value={skillUrl}
                          onChange={(e) => setSkillUrl(e.target.value)}
                        />
                      </div>
                      <div className="flex flex-col gap-1">
                        <label className="text-sm font-medium" htmlFor="skill-ref-input">Ref (branch / tag / commit)</label>
                        <Input
                          id="skill-ref-input"
                          data-testid="skill-ref-input"
                          placeholder="main"
                          value={skillRef}
                          onChange={(e) => setSkillRef(e.target.value)}
                        />
                      </div>
                      <div className="flex flex-col gap-1">
                        <label className="text-sm font-medium" htmlFor="skill-subdir-input">Subdirectory</label>
                        <Input
                          id="skill-subdir-input"
                          data-testid="skill-subdir-input"
                          placeholder="skills/my-skill"
                          value={skillSubdir}
                          onChange={(e) => setSkillSubdir(e.target.value)}
                        />
                      </div>
                      <div className="flex flex-col gap-1">
                        <label className="text-sm font-medium" htmlFor="skill-name-input">Name override</label>
                        <Input
                          id="skill-name-input"
                          data-testid="skill-name-input"
                          placeholder="Derived from SKILL.md if blank"
                          value={skillName}
                          onChange={(e) => setSkillName(e.target.value)}
                        />
                      </div>
                      <div className="flex flex-col gap-1">
                        <label className="text-sm font-medium" htmlFor="skill-description-input">Description override</label>
                        <Input
                          id="skill-description-input"
                          data-testid="skill-description-input"
                          placeholder="Derived from SKILL.md if blank"
                          value={skillDescription}
                          onChange={(e) => setSkillDescription(e.target.value)}
                        />
                      </div>
                    </div>
                    <DialogFooter>
                      <DialogClose asChild>
                        <Button variant="outline" type="button">Cancel</Button>
                      </DialogClose>
                      <Button
                        data-testid="skill-ingest-submit"
                        disabled={!skillUrl.trim() || ingestingSkill}
                        onClick={handleIngestSkill}
                      >
                        {ingestingSkill ? "Ingesting…" : "Ingest skill"}
                      </Button>
                    </DialogFooter>
                  </>
                ) : (
                  <>
                    <DialogHeader>
                      <DialogTitle>Attach bundle to profile</DialogTitle>
                      <DialogDescription>
                        Review the discovered members, then attach. Excluded members and renames
                        are set now — changing them later means Remove + Attach again.
                      </DialogDescription>
                    </DialogHeader>
                    <BundleAttachPanel
                      bundleId={attachPanel.bundleId}
                      name={attachPanel.name}
                      members={attachPanel.members}
                      skippedEntries={attachPanel.skippedEntries}
                      warnings={attachPanel.warnings}
                      changed={attachPanel.changed}
                      attaching={attachingBundle}
                      onAttach={handleAttachBundle}
                    />
                  </>
                )}
              </DialogContent>
            </Dialog>
          </div>
        )}
      </div>
    </div>
  );
}
