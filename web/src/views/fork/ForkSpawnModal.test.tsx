import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import { ForkSpawnModal, type ForkSpawnModalActions } from "./ForkSpawnModal";
import type { ForkSpawnState } from "./useForkSpawn";
import type { MigrationTarget } from "@/api/migration";

const CURRENT_TARGET: MigrationTarget = {
  nodeId: "node-current",
  class: "self-hosted",
  yours: true,
  online: true,
  isCurrent: true,
  journalSizeBytes: 0,
};

const OTHER_TARGET: MigrationTarget = {
  nodeId: "node-b",
  class: "self-hosted",
  yours: true,
  online: true,
  isCurrent: false,
  journalSizeBytes: 0,
};

const actions: ForkSpawnModalActions = {
  open: vi.fn(),
  select: vi.fn(),
  setName: vi.fn(),
  confirm: vi.fn(),
  cancel: vi.fn(),
  openDeliveryPending: vi.fn(),
  retryDelivery: vi.fn(),
  openFork: vi.fn(),
};

function baseState(overrides: Partial<ForkSpawnState> = {}): ForkSpawnState {
  return {
    phase: "idle",
    sourceSpawnId: null,
    forkSpawnId: null,
    targets: [],
    selectedTarget: null,
    name: "",
    progress: null,
    result: null,
    errorMsg: null,
    ...overrides,
  };
}

describe("ForkSpawnModal", () => {
  it("renders current node as the default target", () => {
    render(
      <ForkSpawnModal
        state={baseState({
          phase: "selecting",
          targets: [CURRENT_TARGET, OTHER_TARGET],
          selectedTarget: CURRENT_TARGET,
        })}
        actions={actions}
      />,
    );

    expect(screen.getByTestId("fork-target-node-current")).toBeInTheDocument();
    expect(screen.getByTestId("fork-target-node-current").textContent).toContain("current");
  });

  it("submits an optional fork name", async () => {
    render(
      <ForkSpawnModal
        state={baseState({
          phase: "confirming",
          sourceSpawnId: "source-1",
          selectedTarget: CURRENT_TARGET,
          name: "Trial",
        })}
        actions={actions}
      />,
    );

    expect(screen.getByTestId("fork-name-input")).toHaveValue("Trial");
    await userEvent.click(screen.getByTestId("fork-confirm"));
    expect(actions.confirm).toHaveBeenCalled();
  });

  it("renders delivery-pending retry for owner-sealed fork", () => {
    render(
      <ForkSpawnModal
        state={baseState({
          phase: "delivery-pending",
          forkSpawnId: "fork-1",
          errorMsg: "delivery failed",
        })}
        actions={actions}
      />,
    );

    expect(screen.getByTestId("fork-delivery-pending")).toBeInTheDocument();
    expect(screen.getByTestId("fork-delivery-retry")).toBeInTheDocument();
  });
});
