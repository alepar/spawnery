import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

vi.mock("@/api/bundles", () => ({
  getBundleDiff: vi.fn(),
}));

import { getBundleDiff } from "@/api/bundles";
import { BundleDiffDialog } from "./BundleDiffDialog";

function deferred<T>() {
  let resolve!: (v: T) => void;
  let reject!: (e: unknown) => void;
  const promise = new Promise<T>((res, rej) => { resolve = res; reject = rej; });
  return { promise, resolve, reject };
}

describe("BundleDiffDialog", () => {
  beforeEach(() => vi.clearAllMocks());

  it("Re-pin is disabled while loading and enabled only once the diff has rendered", async () => {
    const d = deferred<any>();
    vi.mocked(getBundleDiff).mockReturnValue(d.promise as any);

    render(
      <BundleDiffDialog
        open bundleId="b1" fromVersionId="v1" toVersionId="v2"
        onOpenChange={vi.fn()} onRepin={vi.fn()}
      />,
    );

    expect(screen.getByTestId("bundle-diff-repin")).toBeDisabled();

    d.resolve({ members: [], fromCommit: "aaaa", toCommit: "bbbb", diffToken: "tok-1" });
    await waitFor(() => expect(screen.getByTestId("bundle-diff-repin")).not.toBeDisabled());
  });

  it("empty diffToken keeps Re-pin disabled with the explanatory note", async () => {
    vi.mocked(getBundleDiff).mockResolvedValue({
      members: [], fromCommit: "aaaa", toCommit: "bbbb", diffToken: "",
    });

    render(
      <BundleDiffDialog
        open bundleId="b1" fromVersionId="v1" toVersionId="v2"
        onOpenChange={vi.fn()} onRepin={vi.fn()}
      />,
    );

    await waitFor(() => expect(getBundleDiff).toHaveBeenCalled());
    expect(await screen.findByText(/check for updates again/i)).toBeTruthy();
    expect(screen.getByTestId("bundle-diff-repin")).toBeDisabled();
  });

  it("renders ADDED/REMOVED/CHANGED members with their change badge, and a line diff for CHANGED", async () => {
    vi.mocked(getBundleDiff).mockResolvedValue({
      members: [
        { sourceSubdir: "skills/new", change: "ADDED", name: "new-skill", newSkillMd: "hello\nworld" },
        { sourceSubdir: "skills/gone", change: "REMOVED", name: "gone-skill", oldSkillMd: "bye" },
        {
          sourceSubdir: "skills/a", change: "CHANGED", name: "a",
          oldSkillMd: "line1\nline2", newSkillMd: "line1\nline2-changed",
        },
      ],
      fromCommit: "aaaa", toCommit: "bbbb", diffToken: "tok-1",
    });

    render(
      <BundleDiffDialog
        open bundleId="b1" fromVersionId="v1" toVersionId="v2"
        onOpenChange={vi.fn()} onRepin={vi.fn()}
      />,
    );

    await waitFor(() => screen.getByTestId("bundle-diff-member-skills/new"));
    expect(screen.getByTestId("bundle-diff-member-skills/new").textContent).toMatch(/ADDED/i);
    expect(screen.getByTestId("bundle-diff-member-skills/gone").textContent).toMatch(/REMOVED/i);
    expect(screen.getByTestId("bundle-diff-member-skills/a").textContent).toMatch(/CHANGED/i);

    // CHANGED member's line diff: "line2" removed, "line2-changed" added.
    const changedPanel = screen.getByTestId("bundle-diff-member-skills/a");
    expect(changedPanel.textContent).toContain("line2-changed");
  });

  it("renders body_unavailable and *_truncated notices instead of a silently empty diff", async () => {
    vi.mocked(getBundleDiff).mockResolvedValue({
      members: [
        { sourceSubdir: "skills/x", change: "CHANGED", name: "x", bodyUnavailable: true },
        { sourceSubdir: "skills/y", change: "CHANGED", name: "y", oldTruncated: true, newTruncated: true, oldSkillMd: "a", newSkillMd: "b" },
      ],
      fromCommit: "aaaa", toCommit: "bbbb", diffToken: "tok-1",
    });

    render(
      <BundleDiffDialog
        open bundleId="b1" fromVersionId="v1" toVersionId="v2"
        onOpenChange={vi.fn()} onRepin={vi.fn()}
      />,
    );

    await waitFor(() => screen.getByTestId("bundle-diff-member-skills/x"));
    expect(screen.getByTestId("bundle-diff-member-skills/x").textContent).toMatch(/unavailable/i);
    expect(screen.getByTestId("bundle-diff-member-skills/y").textContent).toMatch(/truncated/i);
  });

  it("shows from_commit -> to_commit", async () => {
    vi.mocked(getBundleDiff).mockResolvedValue({
      members: [], fromCommit: "1111111111111111111111111111111111aaaa", toCommit: "2222222222222222222222222222222222bbbb", diffToken: "tok-1",
    });

    render(
      <BundleDiffDialog
        open bundleId="b1" fromVersionId="v1" toVersionId="v2"
        onOpenChange={vi.fn()} onRepin={vi.fn()}
      />,
    );

    await waitFor(() => screen.getByTestId("bundle-diff-commits"));
    expect(screen.getByTestId("bundle-diff-commits").textContent).toContain("111111111111");
    expect(screen.getByTestId("bundle-diff-commits").textContent).toContain("222222222222");
  });

  it("clicking Re-pin calls onRepin(toVersionId, diffToken) exactly once", async () => {
    vi.mocked(getBundleDiff).mockResolvedValue({
      members: [], fromCommit: "aaaa", toCommit: "bbbb", diffToken: "tok-1",
    });
    const onRepin = vi.fn().mockResolvedValue(undefined);

    render(
      <BundleDiffDialog
        open bundleId="b1" fromVersionId="v1" toVersionId="v2"
        onOpenChange={vi.fn()} onRepin={onRepin}
      />,
    );

    await waitFor(() => expect(screen.getByTestId("bundle-diff-repin")).not.toBeDisabled());
    await userEvent.click(screen.getByTestId("bundle-diff-repin"));
    expect(onRepin).toHaveBeenCalledTimes(1);
    expect(onRepin).toHaveBeenCalledWith("v2", "tok-1");
  });

  it("onRepin rejecting with a FailedPrecondition Connect error toasts the server's message", async () => {
    vi.mocked(getBundleDiff).mockResolvedValue({
      members: [], fromCommit: "aaaa", toCommit: "bbbb", diffToken: "tok-1",
    });
    const onRepin = vi.fn().mockRejectedValue(
      new Error('RepinProfileBundle failed: 400 {"code":"failed_precondition","message":"diff token expired; check for updates again"}'),
    );

    const { toast } = await import("sonner");
    const errorSpy = vi.spyOn(toast, "error");

    render(
      <BundleDiffDialog
        open bundleId="b1" fromVersionId="v1" toVersionId="v2"
        onOpenChange={vi.fn()} onRepin={onRepin}
      />,
    );

    await waitFor(() => expect(screen.getByTestId("bundle-diff-repin")).not.toBeDisabled());
    await userEvent.click(screen.getByTestId("bundle-diff-repin"));

    await waitFor(() => {
      expect(errorSpy).toHaveBeenCalledWith(expect.stringContaining("diff token expired; check for updates again"));
    });

    errorSpy.mockRestore();
  });
});
