import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

vi.mock("@/api/bundles", () => ({
  reingestBundle: vi.fn(),
  // BundleDiffDialog (rendered by BundleEntryChip once "changed") calls getBundleDiff itself;
  // stub it so that path doesn't crash — its own behavior is covered by BundleDiffDialog.test.tsx.
  getBundleDiff: vi.fn().mockResolvedValue({ members: [], fromCommit: "", toCommit: "", diffToken: "" }),
}));

import { reingestBundle } from "@/api/bundles";
import { BundleEntryChip } from "./BundleEntryChip";
import type { ProfileEntry } from "@/api/profiles";

function makeEntry(overrides: Partial<ProfileEntry> = {}): ProfileEntry {
  return {
    entryId: "e1",
    kind: "PROFILE_ENTRY_KIND_SKILL",
    name: "superpowers",
    source: "PROFILE_ENTRY_SOURCE_BUNDLE_REF",
    bundleId: "b1",
    versionId: "v1",
    excludedSubdirs: [],
    memberRenames: {},
    pinnedSeq: 1,
    latestSeq: 1,
    memberCount: 3,
    ...overrides,
  };
}

describe("BundleEntryChip", () => {
  beforeEach(() => vi.clearAllMocks());

  it("shows member count and v{pinnedSeq}", () => {
    render(
      <BundleEntryChip entry={makeEntry()} onRemove={vi.fn()} onRepin={vi.fn()} />,
    );
    expect(screen.getByText(/3 members/)).toBeTruthy();
    expect(screen.getByText("v1")).toBeTruthy();
  });

  it("shows an Update available badge iff latestSeq > pinnedSeq", () => {
    const { rerender } = render(
      <BundleEntryChip entry={makeEntry({ pinnedSeq: 1, latestSeq: 1 })} onRemove={vi.fn()} onRepin={vi.fn()} />,
    );
    expect(screen.queryByText(/update available/i)).toBeNull();

    rerender(
      <BundleEntryChip entry={makeEntry({ pinnedSeq: 1, latestSeq: 2 })} onRemove={vi.fn()} onRepin={vi.fn()} />,
    );
    expect(screen.getByText(/update available/i)).toBeTruthy();
  });

  it("lists excluded/renamed overrides read-only", () => {
    render(
      <BundleEntryChip
        entry={makeEntry({ excludedSubdirs: ["skills/x"], memberRenames: { "skills/y": "y2" } })}
        onRemove={vi.fn()} onRepin={vi.fn()}
      />,
    );
    expect(screen.getByText(/skills\/x/)).toBeTruthy();
    expect(screen.getByText(/skills\/y/)).toBeTruthy();
    expect(screen.getByText(/y2/)).toBeTruthy();
  });

  it("renders no target toggles (D6)", () => {
    render(<BundleEntryChip entry={makeEntry()} onRemove={vi.fn()} onRepin={vi.fn()} />);
    expect(screen.queryByRole("button", { name: "claude" })).toBeNull();
    expect(screen.getByText(/per-agent targets: all/i)).toBeTruthy();
  });

  it("Check for updates calls reingestBundle", async () => {
    vi.mocked(reingestBundle).mockResolvedValue({
      versionId: "v1", changed: false, addedSubdirs: [], updatedSubdirs: [], removedSubdirs: [],
      diffToken: "", fromVersionId: "", notModified: true, warnings: [],
    });
    render(<BundleEntryChip entry={makeEntry()} onRemove={vi.fn()} onRepin={vi.fn()} />);
    await userEvent.click(screen.getByTestId("bundle-chip-check-updates-e1"));
    expect(reingestBundle).toHaveBeenCalledWith("b1");
  });

  it("notModified shows an 'up to date' toast and does not open the diff dialog", async () => {
    vi.mocked(reingestBundle).mockResolvedValue({
      versionId: "v1", changed: false, addedSubdirs: [], updatedSubdirs: [], removedSubdirs: [],
      diffToken: "", fromVersionId: "", notModified: true, warnings: [],
    });
    const { toast } = await import("sonner");
    const successSpy = vi.spyOn(toast, "success");

    render(<BundleEntryChip entry={makeEntry()} onRemove={vi.fn()} onRepin={vi.fn()} />);
    await userEvent.click(screen.getByTestId("bundle-chip-check-updates-e1"));

    await waitFor(() => expect(successSpy).toHaveBeenCalledWith(expect.stringMatching(/up to date/i)));
    expect(screen.queryByTestId("bundle-diff-dialog")).toBeNull();
    successSpy.mockRestore();
  });

  it("not changed and not behind ⇒ 'up to date' toast and no dialog", async () => {
    vi.mocked(reingestBundle).mockResolvedValue({
      versionId: "v1", changed: false, addedSubdirs: [], updatedSubdirs: [], removedSubdirs: [],
      diffToken: "", fromVersionId: "", notModified: false, warnings: [],
    });
    const { toast } = await import("sonner");
    const successSpy = vi.spyOn(toast, "success");

    render(<BundleEntryChip entry={makeEntry({ pinnedSeq: 2, latestSeq: 2 })} onRemove={vi.fn()} onRepin={vi.fn()} />);
    await userEvent.click(screen.getByTestId("bundle-chip-check-updates-e1"));

    await waitFor(() => expect(successSpy).toHaveBeenCalledWith(expect.stringMatching(/up to date/i)));
    expect(screen.queryByTestId("bundle-diff-dialog")).toBeNull();
    successSpy.mockRestore();
  });

  it("changed ⇒ opens the diff dialog with from=entry.versionId, to=res.versionId", async () => {
    vi.mocked(reingestBundle).mockResolvedValue({
      versionId: "v2", changed: true, addedSubdirs: [], updatedSubdirs: ["skills/a"], removedSubdirs: [],
      diffToken: "tok-1", fromVersionId: "v1", notModified: false, warnings: [],
    });

    render(<BundleEntryChip entry={makeEntry({ versionId: "v1" })} onRemove={vi.fn()} onRepin={vi.fn()} />);
    await userEvent.click(screen.getByTestId("bundle-chip-check-updates-e1"));

    await waitFor(() => expect(screen.getByTestId("bundle-diff-dialog")).toBeTruthy());
  });

  it("Remove calls onRemove(entryId)", async () => {
    const onRemove = vi.fn();
    render(<BundleEntryChip entry={makeEntry()} onRemove={onRemove} onRepin={vi.fn()} />);
    await userEvent.click(screen.getByTestId("bundle-chip-remove-e1"));
    expect(onRemove).toHaveBeenCalledWith("e1");
  });
});
