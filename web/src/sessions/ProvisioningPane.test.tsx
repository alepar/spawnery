import { render, screen } from "@testing-library/react";
import { describe, it, expect } from "vitest";
import { ProvisioningPane } from "./ProvisioningPane";
import type { SpawnView } from "@/api/spawnlet";

const base: SpawnView = {
  spawnId: "s",
  name: "",
  appId: "a",
  status: "starting",
  mode: "",
  model: "",
  modelApplied: true,
  journalKeyDeliveryPending: false,
  transitionPhase: "",
  provisionStep: 0,
  provisionTotal: 0,
  provisionStepLabel: "",
  errorStep: "",
  errorDetail: "",
};

describe("ProvisioningPane", () => {
  it("renders progress: k of N, current label, done/running/pending segments", () => {
    render(<ProvisioningPane spawn={{ ...base, status: "starting", provisionStep: 3, provisionTotal: 7, provisionStepLabel: "cloning repo" }} />);
    expect(screen.getByTestId("provisioning-label")).toHaveTextContent("Step 3 of 7: cloning repo");
    const segs = screen.getAllByTestId("provisioning-step");
    expect(segs).toHaveLength(7);
    expect(segs.filter(s => s.dataset.state === "done")).toHaveLength(2);
    expect(segs.filter(s => s.dataset.state === "running")).toHaveLength(1);
    expect(segs.filter(s => s.dataset.state === "pending")).toHaveLength(4);
  });

  it("renders 'Starting…' fallback when total is 0", () => {
    render(<ProvisioningPane spawn={{ ...base, status: "starting", provisionStep: 0, provisionTotal: 0 }} />);
    expect(screen.getByTestId("provisioning-label")).toHaveTextContent("Starting…");
    expect(screen.queryByTestId("provisioning-steps")).toBeNull();
  });

  it("renders failed step + full multi-line error verbatim", () => {
    const detail = "github: create repo failed\n403 [accepted-permissions=administration=write]";
    render(<ProvisioningPane spawn={{ ...base, status: "error", errorStep: "prepare-mounts", errorDetail: detail }} />);
    expect(screen.getByTestId("provisioning-error-step")).toHaveTextContent("prepare-mounts");
    expect(screen.getByTestId("provisioning-error-detail")).toHaveTextContent("accepted-permissions=administration=write");
  });

  it("renders error pane without step or detail when fields are absent", () => {
    render(<ProvisioningPane spawn={{ ...base, status: "error", errorStep: "", errorDetail: "" }} />);
    expect(screen.getByTestId("provisioning-pane")).toHaveAttribute("data-state", "error");
    expect(screen.queryByTestId("provisioning-error-step")).toBeNull();
    expect(screen.queryByTestId("provisioning-error-detail")).toBeNull();
  });
});
