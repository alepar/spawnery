/**
 * Tests for MoveToModal rendering — durability banners and ephemeral warning (spec §3).
 *
 * Covers the requirement that migrating an ephemeral spawn shows a data-does-not-travel
 * warning in both the SelectingView and ConfirmingView (review finding: blocker).
 */

import { render, screen } from "@testing-library/react";
import { describe, it, expect, vi } from "vitest";
import { MoveToModal } from "./MoveToModal";
import type { MoveToState, MoveToActions } from "./useMoveTo";
import type { MigrationTarget } from "@/api/migration";

// ── Helpers ───────────────────────────────────────────────────────────────────

const TARGET_A: MigrationTarget = {
  nodeId: "node-a",
  class: "self-hosted",
  yours: true,
  online: true,
  isCurrent: false,
  journalSizeBytes: 0,
};

const STUB_ACTIONS: MoveToActions = {
  open: vi.fn(),
  select: vi.fn(),
  confirm: vi.fn(),
  cancel: vi.fn(),
  minimize: vi.fn(),
  restore: vi.fn(),
  openDeliveryPending: vi.fn(),
  retryDelivery: vi.fn(),
};

function baseState(overrides: Partial<MoveToState> = {}): MoveToState {
  return {
    phase: "idle",
    spawnId: null,
    targets: [],
    entries: [],
    durability: null,
    selectedTarget: null,
    progress: null,
    result: null,
    errorMsg: null,
    minimized: false,
    ...overrides,
  };
}

// ── Ephemeral data-does-not-travel warning ────────────────────────────────────

describe("DurabilityBanner — ephemeral", () => {
  it("SelectingView shows ephemeral warning when durability is ephemeral", () => {
    render(
      <MoveToModal
        state={baseState({
          phase: "selecting",
          spawnId: "spawn-1",
          durability: "ephemeral",
          targets: [TARGET_A],
        })}
        actions={STUB_ACTIONS}
      />,
    );

    const banner = screen.getByTestId("durability-banner-ephemeral");
    expect(banner).toBeInTheDocument();
    expect(banner).toHaveTextContent(/does not travel/i);
  });

  it("ConfirmingView shows ephemeral warning when durability is ephemeral", () => {
    render(
      <MoveToModal
        state={baseState({
          phase: "confirming",
          spawnId: "spawn-1",
          durability: "ephemeral",
          selectedTarget: TARGET_A,
        })}
        actions={STUB_ACTIONS}
      />,
    );

    const banner = screen.getByTestId("durability-banner-ephemeral");
    expect(banner).toBeInTheDocument();
    expect(banner).toHaveTextContent(/does not travel/i);
  });

  it("SelectingView does NOT show ephemeral banner for owner-sealed spawns", () => {
    render(
      <MoveToModal
        state={baseState({
          phase: "selecting",
          spawnId: "spawn-2",
          durability: "owner-sealed",
          targets: [TARGET_A],
        })}
        actions={STUB_ACTIONS}
      />,
    );

    expect(screen.queryByTestId("durability-banner-ephemeral")).not.toBeInTheDocument();
    expect(screen.getByTestId("durability-banner-owner-sealed")).toBeInTheDocument();
  });
});
