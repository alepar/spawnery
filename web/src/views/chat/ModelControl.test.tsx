import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import { describe, it, expect, vi, beforeEach } from "vitest";

const setSpawnModelMock = vi.fn(async (..._a: any[]) => ({ model: "x", applied: false }));
vi.mock("@/api/spawnlet", async (orig) => {
  const actual = await orig<typeof import("@/api/spawnlet")>();
  return { ...actual, setSpawnModel: (...a: any[]) => setSpawnModelMock(...a) };
});

import { ModelControl } from "./ModelControl";

beforeEach(() => setSpawnModelMock.mockReset().mockResolvedValue({ model: "x", applied: false }));

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
});
