import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import { describe, it, expect, vi, beforeEach } from "vitest";
import type { SpawnView } from "@/api/spawnlet";

const setSpawnModelMock = vi.fn(async (..._a: any[]) => ({ model: "x", applied: false }));
vi.mock("@/api/spawnlet", async (orig) => {
  const actual = await orig<typeof import("@/api/spawnlet")>();
  return { ...actual, setSpawnModel: (...a: any[]) => setSpawnModelMock(...a) };
});

vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));

import { SetModelModal } from "./SetModelModal";
import { toast } from "sonner";

function spawn(overrides: Partial<SpawnView> = {}): SpawnView {
  return {
    spawnId: "s1",
    name: "Spawn",
    appId: "spawnery/wiki",
    status: "active",
    mode: "",
    model: "a",
    modelApplied: true,
    journalKeyDeliveryPending: false,
    ...overrides,
  };
}

beforeEach(() => {
  setSpawnModelMock.mockReset().mockResolvedValue({ model: "x", applied: false });
  vi.mocked(toast.error).mockReset();
});

describe("SetModelModal", () => {
  it("does not render content when spawn is null (closed)", () => {
    render(<SetModelModal spawn={null} onClose={() => {}} />);
    expect(screen.queryByTestId("set-model-modal")).toBeNull();
  });

  it("seeds the input with the record model and shows it as current", () => {
    render(<SetModelModal spawn={spawn({ model: "openai/gpt-4o" })} onClose={() => {}} />);
    expect((screen.getByLabelText("Spawn model") as HTMLInputElement).value).toBe("openai/gpt-4o");
    expect(screen.getByTestId("set-model-current")).toHaveTextContent("openai/gpt-4o");
  });

  it("does not show the pending badge when modelApplied is true", () => {
    render(<SetModelModal spawn={spawn({ modelApplied: true })} onClose={() => {}} />);
    expect(screen.queryByTestId("model-pending")).toBeNull();
  });

  it("shows the pending badge when modelApplied is false", () => {
    render(<SetModelModal spawn={spawn({ modelApplied: false })} onClose={() => {}} />);
    expect(screen.getByTestId("model-pending")).toBeInTheDocument();
  });

  it("calls setSpawnModel with the entered id on Set", async () => {
    render(<SetModelModal spawn={spawn({ model: "a" })} onClose={() => {}} />);
    fireEvent.change(screen.getByLabelText("Spawn model"), { target: { value: "anthropic/claude-3.5" } });
    fireEvent.click(screen.getByRole("button", { name: "Set model" }));
    await waitFor(() => expect(setSpawnModelMock).toHaveBeenCalledWith("s1", "anthropic/claude-3.5"));
  });

  it("optimistically shows pending right after submit even if currently applied", async () => {
    render(<SetModelModal spawn={spawn({ model: "a", modelApplied: true })} onClose={() => {}} />);
    fireEvent.change(screen.getByLabelText("Spawn model"), { target: { value: "b" } });
    fireEvent.click(screen.getByRole("button", { name: "Set model" }));
    await waitFor(() => expect(screen.getByTestId("model-pending")).toBeInTheDocument());
  });

  it("disables Set for an unchanged or empty value (no RPC)", () => {
    render(<SetModelModal spawn={spawn({ model: "a" })} onClose={() => {}} />);
    const btn = screen.getByRole("button", { name: "Set model" }) as HTMLButtonElement;
    expect(btn).toBeDisabled(); // unchanged
    fireEvent.change(screen.getByLabelText("Spawn model"), { target: { value: "   " } });
    expect(btn).toBeDisabled(); // empty after trim
    fireEvent.click(btn);
    expect(setSpawnModelMock).not.toHaveBeenCalled();
  });

  it("does not clobber typed input on a poll tick with the same model prop", () => {
    const { rerender } = render(<SetModelModal spawn={spawn({ model: "a" })} onClose={() => {}} />);
    fireEvent.change(screen.getByLabelText("Spawn model"), { target: { value: "typed-in-progress" } });
    rerender(<SetModelModal spawn={spawn({ model: "a" })} onClose={() => {}} />);
    expect((screen.getByLabelText("Spawn model") as HTMLInputElement).value).toBe("typed-in-progress");
  });

  it("clears the optimistic pending once the record reflects the submitted model", async () => {
    const { rerender } = render(<SetModelModal spawn={spawn({ model: "a", modelApplied: true })} onClose={() => {}} />);
    fireEvent.change(screen.getByLabelText("Spawn model"), { target: { value: "b" } });
    fireEvent.click(screen.getByRole("button", { name: "Set model" }));
    await waitFor(() => expect(screen.getByTestId("model-pending")).toBeInTheDocument());
    rerender(<SetModelModal spawn={spawn({ model: "b", modelApplied: true })} onClose={() => {}} />);
    await waitFor(() => expect(screen.queryByTestId("model-pending")).toBeNull());
  });

  it("clears optimistic pending and toasts on a rejected submit", async () => {
    setSpawnModelMock.mockRejectedValueOnce(new Error("boom"));
    render(<SetModelModal spawn={spawn({ model: "a", modelApplied: true })} onClose={() => {}} />);
    fireEvent.change(screen.getByLabelText("Spawn model"), { target: { value: "b" } });
    fireEvent.click(screen.getByRole("button", { name: "Set model" }));
    await waitFor(() => expect(toast.error).toHaveBeenCalledWith(expect.stringContaining("boom")));
    expect(screen.queryByTestId("model-pending")).toBeNull();
  });

  it("Cancel calls onClose", () => {
    const onClose = vi.fn();
    render(<SetModelModal spawn={spawn()} onClose={onClose} />);
    fireEvent.click(screen.getByTestId("set-model-cancel"));
    expect(onClose).toHaveBeenCalled();
  });
});
