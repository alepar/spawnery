import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { BundleAttachPanel } from "./BundleAttachPanel";
import type { BundleMember } from "@/api/bundles";

const members: BundleMember[] = [
  { catalogId: "c1", sourceSubdir: "skills/a", name: "a", description: "skill a", sha256: "aaa", position: 0 },
  { catalogId: "c2", sourceSubdir: "skills/b", name: "b", description: "skill b", sha256: "bbb", position: 1 },
];

describe("BundleAttachPanel", () => {
  it("renders every member with its subdir", () => {
    render(
      <BundleAttachPanel
        bundleId="b1" name="superpowers" members={members}
        skippedEntries={[]} warnings={[]} onAttach={vi.fn()}
      />,
    );
    expect(screen.getByText("a")).toBeTruthy();
    expect(screen.getByText("skills/a")).toBeTruthy();
    expect(screen.getByText("b")).toBeTruthy();
    expect(screen.getByText("skills/b")).toBeTruthy();
  });

  it("renders the skipped-entries list and each warning", () => {
    render(
      <BundleAttachPanel
        bundleId="b1" name="superpowers" members={members}
        skippedEntries={["AGENTS.md (symlink)"]}
        warnings={["nested skill at skills/a/sub ignored"]}
        onAttach={vi.fn()}
      />,
    );
    expect(screen.getByText(/AGENTS\.md \(symlink\)/)).toBeTruthy();
    expect(screen.getByText(/nested skill at skills\/a\/sub ignored/)).toBeTruthy();
  });

  it("renders a 'no new version' note when changed is false", () => {
    render(
      <BundleAttachPanel
        bundleId="b1" name="superpowers" members={members}
        skippedEntries={[]} warnings={[]} changed={false} onAttach={vi.fn()}
      />,
    );
    expect(screen.getByText(/already ingested/i)).toBeTruthy();
  });

  it("does not render the 'no new version' note when changed is true", () => {
    render(
      <BundleAttachPanel
        bundleId="b1" name="superpowers" members={members}
        skippedEntries={[]} warnings={[]} changed={true} onAttach={vi.fn()}
      />,
    );
    expect(screen.queryByText(/already ingested/i)).toBeNull();
  });

  it("excluding all members disables Attach", async () => {
    render(
      <BundleAttachPanel
        bundleId="b1" name="superpowers" members={members}
        skippedEntries={[]} warnings={[]} onAttach={vi.fn()}
      />,
    );
    await userEvent.click(screen.getByTestId("member-exclude-c1"));
    await userEvent.click(screen.getByTestId("member-exclude-c2"));
    expect(screen.getByTestId("bundle-attach-submit")).toBeDisabled();
  });

  it("excluding fewer than all members keeps Attach enabled", async () => {
    render(
      <BundleAttachPanel
        bundleId="b1" name="superpowers" members={members}
        skippedEntries={[]} warnings={[]} onAttach={vi.fn()}
      />,
    );
    await userEvent.click(screen.getByTestId("member-exclude-c1"));
    expect(screen.getByTestId("bundle-attach-submit")).not.toBeDisabled();
  });

  it("an exclude + a rename produce exactly the expected overrides on Attach", async () => {
    const onAttach = vi.fn();
    render(
      <BundleAttachPanel
        bundleId="b1" name="superpowers" members={members}
        skippedEntries={[]} warnings={[]} onAttach={onAttach}
      />,
    );
    await userEvent.click(screen.getByTestId("member-exclude-c1"));
    await userEvent.type(screen.getByTestId("member-rename-c2"), "b2");
    await userEvent.click(screen.getByTestId("bundle-attach-submit"));
    expect(onAttach).toHaveBeenCalledWith({
      excludedSubdirs: ["skills/a"],
      memberRenames: { "skills/b": "b2" },
    });
  });

  it("a rename that is not a single clean path segment is blocked client-side with a message", async () => {
    const onAttach = vi.fn();
    render(
      <BundleAttachPanel
        bundleId="b1" name="superpowers" members={members}
        skippedEntries={[]} warnings={[]} onAttach={onAttach}
      />,
    );
    await userEvent.type(screen.getByTestId("member-rename-c2"), "skills/b2");
    expect(screen.getByText(/single path segment/i)).toBeTruthy();
    expect(screen.getByTestId("bundle-attach-submit")).toBeDisabled();
    expect(onAttach).not.toHaveBeenCalled();
  });

  it("disables Attach while attaching is true", () => {
    render(
      <BundleAttachPanel
        bundleId="b1" name="superpowers" members={members}
        skippedEntries={[]} warnings={[]} onAttach={vi.fn()} attaching
      />,
    );
    expect(screen.getByTestId("bundle-attach-submit")).toBeDisabled();
  });
});
