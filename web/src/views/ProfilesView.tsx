import { useEffect, useState } from "react";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";
import {
  listProfiles,
  createProfile,
  getProfile,
  updateProfile,
  deleteProfile,
  addProfileEntry,
  removeProfileEntry,
  listCatalogEntries,
  KIND_LABEL,
  kindToCapKind,
  type ProfileSummary,
  type Profile,
  type ProfileEntry,
  type CatalogEntrySummary,
  type ProfileEntryKind,
  type ProfileEntrySource,
} from "@/api/profiles";
import { AGENTS, capabilityFor, type CapabilityStatus } from "@/api/capabilities";

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
  const [customKind, setCustomKind] = useState<ProfileEntryKind>("PROFILE_ENTRY_KIND_SKILL");
  const [customName, setCustomName] = useState("");
  const [customInline, setCustomInline] = useState("");
  const [addingEntry, setAddingEntry] = useState(false);

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

  useEffect(() => { refreshList(); }, []);

  useEffect(() => {
    if (selectedId) {
      refreshProfile(selectedId);
      listCatalogEntries().then(setCatalogEntries).catch(() => {});
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
      for (const entry of profile.entries) {
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

  const handleUpdateTargets = async (entry: ProfileEntry, targets: string[]) => {
    if (!profile) return;
    try {
      // Remove the old entry and re-add with updated targets
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
      await addProfileEntry(profile.profileId, removeResult.version, entryInput);
      if (selectedId) await refreshProfile(selectedId);
    } catch (e: any) {
      toast.error("Update targets failed: " + e.message);
      if (selectedId) refreshProfile(selectedId);
    }
  };

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
            </div>

            {/* Catalog picker */}
            {showAddCatalog && (
              <div data-testid="catalog-picker" className="flex flex-col gap-2 rounded border border-border p-3">
                <div className="text-sm font-semibold">Catalog entries</div>
                {catalogEntries.length === 0 && (
                  <div className="text-xs text-muted-foreground">No catalog entries.</div>
                )}
                {catalogEntries.map((ce) => (
                  <div key={ce.catalogId} className="flex items-center justify-between gap-2">
                    <div>
                      <span className="text-sm font-medium">{ce.name}</span>
                      {ce.description && (
                        <span className="ml-2 text-xs text-muted-foreground">{ce.description}</span>
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
                  <option value="PROFILE_ENTRY_KIND_SKILL">Skill</option>
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
          </div>
        )}
      </div>
    </div>
  );
}
