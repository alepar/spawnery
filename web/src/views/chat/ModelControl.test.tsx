import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import { describe, it, expect, vi, beforeEach } from "vitest";

const setSpawnModelMock = vi.fn(async (..._a: any[]) => ({ model: "x", applied: false }));
vi.mock("@/api/spawnlet", async (orig) => {
  const actual = await orig<typeof import("@/api/spawnlet")>();
  return { ...actual, setSpawnModel: (...a: any[]) => setSpawnModelMock(...a) };
});

vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));

import { ModelControl } from "./ModelControl";
import { toast } from "sonner";

beforeEach(() => {
  setSpawnModelMock.mockReset().mockResolvedValue({ model: "x", applied: false });
  vi.mocked(toast.error).mockReset();
});

describe("ModelControl", () => {
  it("shows the record model in the input", () => {
    render(<ModelControl spawnId="s1" model="openai/gpt-4o" modelApplied={true} />);
    expect((screen.getByLabelText("Spawn model") as HTMLInputElement).value).toBe("openai/gpt-4o");
  });

  it("does not show the pending badge when modelApplied is true", () => {
    render(<ModelControl spawnId="s1" model="a" modelApplied={true} />);
    expect(screen.queryByTestId("model-pending")).toBeNull();
  });

  it("shows the pending badge when modelApplied is false", () => {
    render(<ModelControl spawnId="s1" model="a" modelApplied={false} />);
    expect(screen.getByTestId("model-pending")).toBeInTheDocument();
  });

  it("calls setSpawnModel with the entered id on Set", async () => {
    render(<ModelControl spawnId="s1" model="a" modelApplied={true} />);
    fireEvent.change(screen.getByLabelText("Spawn model"), { target: { value: "anthropic/claude-3.5" } });
    fireEvent.click(screen.getByRole("button", { name: "Set model" }));
    await waitFor(() => expect(setSpawnModelMock).toHaveBeenCalledWith("s1", "anthropic/claude-3.5"));
  });

  it("optimistically shows pending right after submit even if currently applied", async () => {
    render(<ModelControl spawnId="s1" model="a" modelApplied={true} />);
    fireEvent.change(screen.getByLabelText("Spawn model"), { target: { value: "b" } });
    fireEvent.click(screen.getByRole("button", { name: "Set model" }));
    await waitFor(() => expect(screen.getByTestId("model-pending")).toBeInTheDocument());
  });

  it("does not call the RPC for an unchanged or empty value", () => {
    render(<ModelControl spawnId="s1" model="a" modelApplied={true} />);
    fireEvent.click(screen.getByRole("button", { name: "Set model" })); // unchanged
    fireEvent.change(screen.getByLabelText("Spawn model"), { target: { value: "   " } });
    fireEvent.click(screen.getByRole("button", { name: "Set model" })); // empty
    expect(setSpawnModelMock).not.toHaveBeenCalled();
  });

  it("does not clobber typed input on a poll tick with the same model prop", () => {
    const { rerender } = render(<ModelControl spawnId="s1" model="a" modelApplied={true} />);
    fireEvent.change(screen.getByLabelText("Spawn model"), { target: { value: "typed-in-progress" } });
    // Poll tick: record unchanged, same prop value re-passed.
    rerender(<ModelControl spawnId="s1" model="a" modelApplied={true} />);
    expect((screen.getByLabelText("Spawn model") as HTMLInputElement).value).toBe("typed-in-progress");
  });

  it("clears the optimistic pending once the record reflects the submitted model", async () => {
    const { rerender } = render(<ModelControl spawnId="s1" model="a" modelApplied={true} />);
    fireEvent.change(screen.getByLabelText("Spawn model"), { target: { value: "b" } });
    fireEvent.click(screen.getByRole("button", { name: "Set model" }));
    await waitFor(() => expect(screen.getByTestId("model-pending")).toBeInTheDocument());
    // Poll catches up: record now reports the new model, applied.
    rerender(<ModelControl spawnId="s1" model="b" modelApplied={true} />);
    await waitFor(() => expect(screen.queryByTestId("model-pending")).toBeNull());
  });

  it("clears optimistic pending and toasts on a rejected submit", async () => {
    setSpawnModelMock.mockRejectedValueOnce(new Error("boom"));
    render(<ModelControl spawnId="s1" model="a" modelApplied={true} />);
    fireEvent.change(screen.getByLabelText("Spawn model"), { target: { value: "b" } });
    fireEvent.click(screen.getByRole("button", { name: "Set model" }));
    await waitFor(() => expect(toast.error).toHaveBeenCalledWith(expect.stringContaining("boom")));
    // modelApplied is true and no successful optimistic remains -> no pending badge.
    expect(screen.queryByTestId("model-pending")).toBeNull();
  });

  it("disables the Set button while a submit is in flight", async () => {
    let resolve!: (v: any) => void;
    setSpawnModelMock.mockReturnValueOnce(new Promise((r) => { resolve = r; }));
    render(<ModelControl spawnId="s1" model="a" modelApplied={true} />);
    fireEvent.change(screen.getByLabelText("Spawn model"), { target: { value: "b" } });
    const btn = screen.getByRole("button", { name: "Set model" }) as HTMLButtonElement;
    fireEvent.click(btn);
    await waitFor(() => expect(btn).toBeDisabled());
    resolve({ model: "b", applied: false });
    await waitFor(() => expect(btn).not.toBeDisabled());
  });
});
