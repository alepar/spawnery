import { describe, it, expect, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { BundleCard } from "./BundleCard";
import type { BundleSummary, BundleMember, BundleVersion } from "@/api/bundles";

const summary: BundleSummary = {
  bundleId: "b1",
  name: "superpowers",
  sourceUrl: "https://github.com/obra/superpowers",
  sourceRef: "main",
  sourceSubdir: "",
  latestVersionId: "v3",
  latestSeq: 3,
  memberCount: 2,
};

const versions: BundleVersion[] = [
  { versionId: "v3", seq: 3, sourceCommit: "1111111111111111111111111111111111aaaa", createdAt: "1000" },
];

const members: BundleMember[] = [
  { catalogId: "c1", sourceSubdir: "skills/a", name: "a", sha256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", position: 0 },
  { catalogId: "c2", sourceSubdir: "skills/b", name: "b", sha256: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", position: 1 },
];

describe("BundleCard", () => {
  it("renders source url/ref/subdir and the short source commit with full sha as title", () => {
    render(
      <BundleCard summary={summary} versions={versions} members={members}
        onAttach={vi.fn()} onCheckUpdates={vi.fn()} />,
    );
    const link = screen.getByRole("link", { name: /obra\/superpowers/ });
    expect(link.getAttribute("href")).toBe("https://github.com/obra/superpowers");
    expect(screen.getByText(/main/)).toBeTruthy();
    const commitEl = screen.getByText(/111111111111/);
    expect(commitEl.getAttribute("title")).toBe("1111111111111111111111111111111111aaaa");
  });

  it("hides members until expanded, then lists them", async () => {
    render(
      <BundleCard summary={summary} versions={versions} members={members}
        onAttach={vi.fn()} onCheckUpdates={vi.fn()} />,
    );
    expect(screen.queryByText("skills/a")).toBeNull();
    await userEvent.click(screen.getByText(/2 members/i));
    await waitFor(() => expect(screen.getByText("skills/a")).toBeTruthy());
    expect(screen.getByText("skills/b")).toBeTruthy();
  });

  it("Attach calls onAttach(bundleId)", async () => {
    const onAttach = vi.fn();
    render(
      <BundleCard summary={summary} versions={versions} members={members}
        onAttach={onAttach} onCheckUpdates={vi.fn()} />,
    );
    await userEvent.click(screen.getByTestId("bundle-card-attach-b1"));
    expect(onAttach).toHaveBeenCalledWith("b1");
  });

  it("Check for updates calls onCheckUpdates(bundleId)", async () => {
    const onCheckUpdates = vi.fn();
    render(
      <BundleCard summary={summary} versions={versions} members={members}
        onAttach={vi.fn()} onCheckUpdates={onCheckUpdates} />,
    );
    await userEvent.click(screen.getByTestId("bundle-card-check-updates-b1"));
    expect(onCheckUpdates).toHaveBeenCalledWith("b1");
  });
});
