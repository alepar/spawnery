import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

// Mock the profiles API — let capabilities through (real module: the fixture is a static JSON)
vi.mock("@/api/profiles", () => ({
  listProfiles: vi.fn().mockResolvedValue([
    { profileId: "p1", name: "My Profile", version: 1 },
    { profileId: "p2", name: "Other Profile", version: 1 },
  ]),
  createProfile: vi.fn().mockResolvedValue({ profileId: "p3", version: 1 }),
  getProfile: vi.fn().mockResolvedValue({
    profileId: "p1",
    name: "My Profile",
    version: 2,
    entries: [
      {
        entryId: "e1",
        kind: "PROFILE_ENTRY_KIND_SKILL",
        name: "My Skill",
        source: "PROFILE_ENTRY_SOURCE_CATALOG_REF",
        catalogId: "cat1",
        targets: [],
      },
    ],
    secretIds: [],
  }),
  updateProfile: vi.fn().mockResolvedValue({ version: 3 }),
  deleteProfile: vi.fn().mockResolvedValue(undefined),
  addProfileEntry: vi.fn().mockResolvedValue({ entryId: "e2", version: 3 }),
  removeProfileEntry: vi.fn().mockResolvedValue({ version: 3 }),
  listCatalogEntries: vi.fn().mockResolvedValue([
    { catalogId: "cat1", kind: "PROFILE_ENTRY_KIND_SKILL", name: "My Skill", description: "A test skill" },
  ]),
  getCatalogEntry: vi.fn().mockResolvedValue({ catalogId: "cat1", kind: "PROFILE_ENTRY_KIND_SKILL", name: "My Skill" }),
  ingestSkillFromURL: vi.fn().mockResolvedValue({ catalogId: "cat-new" }),
  connectErrorMessage: (e: unknown) => (e instanceof Error ? e.message : String(e)),
  KIND_LABEL: {
    PROFILE_ENTRY_KIND_SKILL: "Skill",
    PROFILE_ENTRY_KIND_MCP: "MCP",
    PROFILE_ENTRY_KIND_CONFIG: "Config",
    PROFILE_ENTRY_KIND_PLUGIN: "Plugin",
  },
  kindToCapKind: (kind: string) => {
    switch (kind) {
      case "PROFILE_ENTRY_KIND_SKILL": return "skill";
      case "PROFILE_ENTRY_KIND_MCP": return "mcp";
      case "PROFILE_ENTRY_KIND_CONFIG": return "config";
      case "PROFILE_ENTRY_KIND_PLUGIN": return "plugin";
      default: return "skill";
    }
  },
}));

import {
  listProfiles,
  createProfile,
  getProfile,
  addProfileEntry,
  deleteProfile,
  listCatalogEntries,
  ingestSkillFromURL,
} from "@/api/profiles";

import { ProfilesView } from "./ProfilesView";

describe("ProfilesView", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    vi.mocked(listProfiles).mockResolvedValue([
      { profileId: "p1", name: "My Profile", version: 1 },
      { profileId: "p2", name: "Other Profile", version: 1 },
    ]);
    vi.mocked(createProfile).mockResolvedValue({ profileId: "p3", version: 1 });
    vi.mocked(getProfile).mockResolvedValue({
      profileId: "p1",
      name: "My Profile",
      version: 2,
      entries: [
        {
          entryId: "e1",
          kind: "PROFILE_ENTRY_KIND_SKILL" as const,
          name: "My Skill",
          source: "PROFILE_ENTRY_SOURCE_CATALOG_REF" as const,
          catalogId: "cat1",
          targets: [],
        },
      ],
      secretIds: [],
    });
    vi.mocked(addProfileEntry).mockResolvedValue({ entryId: "e2", version: 3 });
    vi.mocked(deleteProfile).mockResolvedValue(undefined);
  });

  it("renders the profiles view root with data-testid=profiles", async () => {
    render(<ProfilesView />);
    expect(screen.getByTestId("profiles")).toBeTruthy();
  });

  it("loads and renders the profile list", async () => {
    render(<ProfilesView />);
    await waitFor(() => expect(screen.getByTestId("profile-item-p1")).toBeTruthy());
    expect(screen.getByTestId("profile-item-p2")).toBeTruthy();
  });

  it("creates a new profile when name is entered and Create is clicked", async () => {
    render(<ProfilesView />);
    await waitFor(() => screen.getByTestId("profile-name-input"));
    await userEvent.type(screen.getByTestId("profile-name-input"), "New Profile");
    await userEvent.click(screen.getByTestId("profile-create-btn"));
    expect(createProfile).toHaveBeenCalledWith("New Profile");
  });

  it("selecting a profile loads and shows its entries", async () => {
    render(<ProfilesView />);
    await waitFor(() => screen.getByTestId("profile-item-p1"));
    await userEvent.click(screen.getByTestId("profile-item-p1"));
    await waitFor(() => expect(screen.getByTestId("entry-e1")).toBeTruthy());
    expect(screen.getByTestId("entry-kind-e1").textContent).toBe("Skill");
  });

  it("shows Add from catalog and opens catalog picker on click", async () => {
    render(<ProfilesView />);
    await waitFor(() => screen.getByTestId("profile-item-p1"));
    await userEvent.click(screen.getByTestId("profile-item-p1"));
    await waitFor(() => screen.getByTestId("add-catalog-btn"));
    await userEvent.click(screen.getByTestId("add-catalog-btn"));
    await waitFor(() => screen.getByTestId("catalog-picker"));
    expect(screen.getByTestId("add-catalog-entry-cat1")).toBeTruthy();
  });

  it("clicking Add in catalog picker calls addProfileEntry with catalog ref", async () => {
    render(<ProfilesView />);
    await waitFor(() => screen.getByTestId("profile-item-p1"));
    await userEvent.click(screen.getByTestId("profile-item-p1"));
    await waitFor(() => screen.getByTestId("add-catalog-btn"));
    await userEvent.click(screen.getByTestId("add-catalog-btn"));
    await waitFor(() => screen.getByTestId("add-catalog-entry-cat1"));
    await userEvent.click(screen.getByTestId("add-catalog-entry-cat1"));
    expect(addProfileEntry).toHaveBeenCalledWith(
      "p1",
      2,
      expect.objectContaining({ source: "PROFILE_ENTRY_SOURCE_CATALOG_REF", catalogId: "cat1" }),
    );
  });

  it("deletes the selected profile", async () => {
    render(<ProfilesView />);
    await waitFor(() => screen.getByTestId("profile-item-p1"));
    await userEvent.click(screen.getByTestId("profile-item-p1"));
    await waitFor(() => screen.getByTestId("profile-delete-btn"));
    await userEvent.click(screen.getByTestId("profile-delete-btn"));
    expect(deleteProfile).toHaveBeenCalledWith("p1");
  });

  it("CapabilityPreview renders badges for all agents by default (empty targets)", async () => {
    render(<ProfilesView />);
    await waitFor(() => screen.getByTestId("profile-item-p1"));
    await userEvent.click(screen.getByTestId("profile-item-p1"));
    await waitFor(() => screen.getByTestId("cap-preview-e1"));
    // claude supports skill -> should have a badge
    const claudeBadge = screen.getByTestId("cap-badge-e1-claude");
    expect(claudeBadge).toBeTruthy();
    expect(claudeBadge.getAttribute("data-status")).toBe("supported");
  });

  it("opens Add custom form, fills name and inline content, and calls addProfileEntry", async () => {
    render(<ProfilesView />);
    await waitFor(() => screen.getByTestId("profile-item-p1"));
    await userEvent.click(screen.getByTestId("profile-item-p1"));
    await waitFor(() => screen.getByTestId("add-custom-btn"));
    await userEvent.click(screen.getByTestId("add-custom-btn"));
    await waitFor(() => screen.getByTestId("custom-entry-form"));
    await userEvent.type(screen.getByTestId("custom-name-input"), "My Custom MCP");
    // Use plain text — userEvent.type treats { as a special key modifier, so avoid JSON braces
    await userEvent.type(screen.getByTestId("custom-inline-input"), "mcp inline content");
    await userEvent.click(screen.getByTestId("custom-entry-submit"));
    expect(addProfileEntry).toHaveBeenCalledWith(
      "p1",
      2,
      expect.objectContaining({
        source: "PROFILE_ENTRY_SOURCE_CUSTOM",
        name: "My Custom MCP",
        customInline: "mcp inline content",
      }),
    );
  });

  it("CapabilityPreview renders no-op badge for opencode (skill is no-op)", async () => {
    render(<ProfilesView />);
    await waitFor(() => screen.getByTestId("profile-item-p1"));
    await userEvent.click(screen.getByTestId("profile-item-p1"));
    await waitFor(() => screen.getByTestId("cap-preview-e1"));
    const opencodeBadge = screen.getByTestId("cap-badge-e1-opencode");
    expect(opencodeBadge.getAttribute("data-status")).toBe("no-op");
  });

  // --- Add skill from URL dialog ---

  it("shows Add skill from URL button after selecting a profile", async () => {
    render(<ProfilesView />);
    await waitFor(() => screen.getByTestId("profile-item-p1"));
    await userEvent.click(screen.getByTestId("profile-item-p1"));
    await waitFor(() => screen.getByTestId("add-skill-url-btn"));
    expect(screen.getByTestId("add-skill-url-btn")).toBeTruthy();
  });

  it("opens the skill URL dialog when Add skill from URL is clicked", async () => {
    render(<ProfilesView />);
    await waitFor(() => screen.getByTestId("profile-item-p1"));
    await userEvent.click(screen.getByTestId("profile-item-p1"));
    await waitFor(() => screen.getByTestId("add-skill-url-btn"));
    await userEvent.click(screen.getByTestId("add-skill-url-btn"));
    await waitFor(() => screen.getByTestId("add-skill-dialog"));
    expect(screen.getByTestId("skill-url-input")).toBeTruthy();
  });

  it("submit is disabled when URL is blank", async () => {
    render(<ProfilesView />);
    await waitFor(() => screen.getByTestId("profile-item-p1"));
    await userEvent.click(screen.getByTestId("profile-item-p1"));
    await waitFor(() => screen.getByTestId("add-skill-url-btn"));
    await userEvent.click(screen.getByTestId("add-skill-url-btn"));
    await waitFor(() => screen.getByTestId("skill-ingest-submit"));
    expect(screen.getByTestId("skill-ingest-submit")).toBeDisabled();
    // Once URL is typed it becomes enabled
    await userEvent.type(screen.getByTestId("skill-url-input"), "https://github.com/owner/repo");
    expect(screen.getByTestId("skill-ingest-submit")).not.toBeDisabled();
  });

  it("ingests a skill and attaches it to the profile", async () => {
    vi.mocked(ingestSkillFromURL).mockResolvedValue({ catalogId: "cat-new" });
    vi.mocked(listCatalogEntries)
      .mockResolvedValueOnce([
        // Initial load: existing cat1 only
        { catalogId: "cat1", kind: "PROFILE_ENTRY_KIND_SKILL" as const, name: "My Skill", description: "A test skill" },
      ])
      .mockResolvedValueOnce([
        // After ingest refresh: includes the new entry
        { catalogId: "cat1", kind: "PROFILE_ENTRY_KIND_SKILL" as const, name: "My Skill", description: "A test skill" },
        { catalogId: "cat-new", kind: "PROFILE_ENTRY_KIND_SKILL" as const, name: "Deep Research", description: "New" },
      ]);

    render(<ProfilesView />);
    await waitFor(() => screen.getByTestId("profile-item-p1"));
    await userEvent.click(screen.getByTestId("profile-item-p1"));
    await waitFor(() => screen.getByTestId("add-skill-url-btn"));
    await userEvent.click(screen.getByTestId("add-skill-url-btn"));
    await waitFor(() => screen.getByTestId("add-skill-dialog"));

    await userEvent.type(screen.getByTestId("skill-url-input"), "https://github.com/owner/repo");
    await userEvent.type(screen.getByTestId("skill-ref-input"), "main");
    await userEvent.type(screen.getByTestId("skill-subdir-input"), "skills/deep-research");

    await userEvent.click(screen.getByTestId("skill-ingest-submit"));

    await waitFor(() => {
      expect(ingestSkillFromURL).toHaveBeenCalledWith(expect.objectContaining({
        url: "https://github.com/owner/repo",
        ref: "main",
        subdir: "skills/deep-research",
      }));
    });

    await waitFor(() => {
      expect(addProfileEntry).toHaveBeenCalledWith(
        "p1",
        expect.any(Number),
        expect.objectContaining({
          source: "PROFILE_ENTRY_SOURCE_CATALOG_REF",
          catalogId: "cat-new",
        }),
      );
    });
  });

  it("surfaces actionable error toast when ingest fails", async () => {
    vi.mocked(ingestSkillFromURL).mockRejectedValue(
      new Error('IngestSkillFromURL failed: 400 {"code":"invalid_argument","message":"no SKILL.md found at skills/missing"}'),
    );

    // Mock sonner toast
    const { toast } = await import("sonner");
    const errorSpy = vi.spyOn(toast, "error");

    render(<ProfilesView />);
    await waitFor(() => screen.getByTestId("profile-item-p1"));
    await userEvent.click(screen.getByTestId("profile-item-p1"));
    await waitFor(() => screen.getByTestId("add-skill-url-btn"));
    await userEvent.click(screen.getByTestId("add-skill-url-btn"));
    await waitFor(() => screen.getByTestId("add-skill-dialog"));

    await userEvent.type(screen.getByTestId("skill-url-input"), "https://github.com/owner/repo");
    await userEvent.click(screen.getByTestId("skill-ingest-submit"));

    await waitFor(() => {
      expect(errorSpy).toHaveBeenCalledWith(expect.stringContaining("no SKILL.md found at skills/missing"));
    });
    // addProfileEntry must NOT have been called
    expect(addProfileEntry).not.toHaveBeenCalled();

    errorSpy.mockRestore();
  });

  it("surfaces rate-limit error toast", async () => {
    vi.mocked(ingestSkillFromURL).mockRejectedValue(
      new Error('IngestSkillFromURL failed: 429 {"code":"resource_exhausted","message":"ingest rate limit exceeded (max 10 per hour)"}'),
    );

    const { toast } = await import("sonner");
    const errorSpy = vi.spyOn(toast, "error");

    render(<ProfilesView />);
    await waitFor(() => screen.getByTestId("profile-item-p1"));
    await userEvent.click(screen.getByTestId("profile-item-p1"));
    await waitFor(() => screen.getByTestId("add-skill-url-btn"));
    await userEvent.click(screen.getByTestId("add-skill-url-btn"));
    await waitFor(() => screen.getByTestId("add-skill-dialog"));

    await userEvent.type(screen.getByTestId("skill-url-input"), "https://github.com/owner/repo");
    await userEvent.click(screen.getByTestId("skill-ingest-submit"));

    await waitFor(() => {
      const call = errorSpy.mock.calls.find((c) => String(c[0]).includes("rate limit"));
      expect(call).toBeTruthy();
    });

    errorSpy.mockRestore();
  });

  it("regression: SKILL is absent from the custom-kind select (textarea cannot produce a tar)", async () => {
    render(<ProfilesView />);
    await waitFor(() => screen.getByTestId("profile-item-p1"));
    await userEvent.click(screen.getByTestId("profile-item-p1"));
    await waitFor(() => screen.getByTestId("add-custom-btn"));
    await userEvent.click(screen.getByTestId("add-custom-btn"));
    await waitFor(() => screen.getByTestId("custom-kind-select"));
    const select = screen.getByTestId("custom-kind-select") as HTMLSelectElement;
    const optionValues = Array.from(select.options).map((o) => o.value);
    expect(optionValues).not.toContain("PROFILE_ENTRY_KIND_SKILL");
    expect(optionValues).toContain("PROFILE_ENTRY_KIND_MCP");
    expect(optionValues).toContain("PROFILE_ENTRY_KIND_CONFIG");
    expect(optionValues).toContain("PROFILE_ENTRY_KIND_PLUGIN");
  });
});
