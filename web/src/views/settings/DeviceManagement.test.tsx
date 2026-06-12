/**
 * Tests for DeviceManagement (Phase 4, W3).
 *
 * Mocks all keys/ modules and useSessionStore to keep tests fast and hermetic.
 */

import { render, screen, waitFor, act } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, it, expect, beforeEach, afterEach, vi } from "vitest";

// ── Module mocks ──────────────────────────────────────────────────────────────

const mockBuildDeviceList = vi.fn();
const mockCascadeForDevice = vi.fn();

vi.mock("@/keys/devicelist", () => ({
  buildDeviceList: (...args: unknown[]) => mockBuildDeviceList(...args),
  cascadeForDevice: (...args: unknown[]) => mockCascadeForDevice(...args),
}));

const mockLoadAnchor = vi.fn();
const mockClearAnchor = vi.fn();

vi.mock("@/keys/anchor", () => ({
  loadAnchor: () => mockLoadAnchor(),
  clearAnchor: () => mockClearAnchor(),
  saveAnchor: vi.fn(),
  bumpPinnedHead: vi.fn(),
}));

const mockLoadDeviceKeys = vi.fn();
const mockExportDeviceRef = vi.fn();
const mockDeriveDeviceKeysFromMnemonic = vi.fn();

vi.mock("@/keys/device", () => ({
  loadDeviceKeys: () => mockLoadDeviceKeys(),
  exportDeviceRef: (...args: unknown[]) => mockExportDeviceRef(...args),
  deriveDeviceKeysFromMnemonic: (...args: unknown[]) => mockDeriveDeviceKeysFromMnemonic(...args),
  clearDeviceKeys: vi.fn(),
}));

const mockFetchLog = vi.fn();
const mockAppend = vi.fn();
const mockVerifyDeviceSet = vi.fn().mockResolvedValue({ members: [], headHash: new Uint8Array(32), headVersion: 1 });

vi.mock("@/keys/deviceset", () => ({
  httpASTransport: () => ({
    fetchLog: () => mockFetchLog(),
    append: () => mockAppend(),
  }),
  verifyDeviceSet: (...args: unknown[]) => mockVerifyDeviceSet(...args),
}));

const mockLoadSweepProgress = vi.fn();
const mockRemainingCount = vi.fn();

vi.mock("@/keys/epoch", () => ({
  loadSweepProgress: () => mockLoadSweepProgress(),
  remainingCount: (...args: unknown[]) => mockRemainingCount(...args),
  clearSweepProgress: vi.fn(),
  saveSweepProgress: vi.fn(),
  initSweep: vi.fn(),
  isSweepComplete: vi.fn(),
  markSecretsCompleted: vi.fn(),
  markSecretFailed: vi.fn(),
}));

const mockExecuteSweep = vi.fn();

vi.mock("@/keys/sweep", () => ({
  executeSweep: (...args: unknown[]) => mockExecuteSweep(...args),
}));

const mockRevokeDevices = vi.fn();
const mockIsRevocableByNormalRevoke = vi.fn();
const mockRequiresRecoveryConfirmation = vi.fn();

vi.mock("@/keys/revoke", () => ({
  revokeDevices: (...args: unknown[]) => mockRevokeDevices(...args),
  isRevocableByNormalRevoke: (...args: unknown[]) => mockIsRevocableByNormalRevoke(...args),
  requiresRecoveryConfirmation: (...args: unknown[]) => mockRequiresRecoveryConfirmation(...args),
}));

vi.mock("@/keys/recovery", () => ({
  M8_TRUSTED_DEVICE_WARNING: "approve from your phone / enter recovery code only on a trusted device",
}));

vi.mock("@/keys/encoding", () => ({
  toBase64: (bytes: Uint8Array) => Buffer.from(bytes).toString("base64"),
  fromBase64: (s: string) => new Uint8Array(Buffer.from(s, "base64")),
}));

vi.mock("@/config/endpoints", () => ({
  asHttpUrl: (path: string) => "/as" + path,
}));

vi.mock("@/auth/session", () => ({
  useSessionStore: (selector: (s: { getAccessToken: () => string }) => unknown) =>
    selector({ getAccessToken: () => "test-token" }),
}));

// ── Import under test ─────────────────────────────────────────────────────────

import { DeviceManagement } from "./DeviceManagement";
import type { DeviceListItem } from "@/keys/devicelist";

// ── Test helpers ──────────────────────────────────────────────────────────────

const sampleAnchor = {
  ownerRoot: {
    device1_sign_pub: "d1signpub",
    recovery_sign_pub: "recsignpub",
  },
  headVersion: 2,
};

const sampleSignPub = "currentdevicesignpub";
const sampleRef = {
  x25519Pub: new Uint8Array([1, 2, 3, 4]),
  signPub: new Uint8Array(Buffer.from(sampleSignPub)),
};

function makeDevice(overrides: Partial<DeviceListItem>): DeviceListItem {
  return {
    x25519Pub: "x25519-a",
    signPub: "signa",
    name: "My Browser",
    enrolledAt: "1700000000000000000",
    enrolledBySignPub: null,
    isCurrent: false,
    isRecovery: false,
    ...overrides,
  };
}

function setupDefaultMocks() {
  mockLoadAnchor.mockReturnValue(sampleAnchor);
  mockLoadDeviceKeys.mockResolvedValue({ ecdsaPrivate: {}, ecdsaPublic: {} });
  mockExportDeviceRef.mockResolvedValue(sampleRef);
  mockFetchLog.mockResolvedValue({
    log: { entries: [] },
    head: "head-b64",
    version: 2,
  });
  mockLoadSweepProgress.mockReturnValue(null);
  mockRemainingCount.mockReturnValue(0);
  mockCascadeForDevice.mockReturnValue([]);
  mockIsRevocableByNormalRevoke.mockImplementation((item: DeviceListItem) => !item.isRecovery);
  mockRequiresRecoveryConfirmation.mockReturnValue(false);
  mockVerifyDeviceSet.mockResolvedValue({ members: [], headHash: new Uint8Array(32), headVersion: 2 });
  mockExecuteSweep.mockResolvedValue({ total: 0, done: 0, secretIds: [], completed: [], failed: [], isRevocation: true, targetVersion: 2, updatedAt: Date.now() });
  mockRevokeDevices.mockResolvedValue({ total: 0, done: 0, secretIds: [], completed: [], failed: [], isRevocation: true, targetVersion: 2, updatedAt: Date.now() });
}

// ── Tests ─────────────────────────────────────────────────────────────────────

describe("DeviceManagement — unenrolled state", () => {
  beforeEach(() => {
    localStorage.clear();
    vi.clearAllMocks();
    mockLoadAnchor.mockReturnValue(null);
    mockLoadSweepProgress.mockReturnValue(null);
    mockRemainingCount.mockReturnValue(0);
  });

  it("renders unenrolled placeholder when no anchor", async () => {
    render(<DeviceManagement />);
    await waitFor(() => {
      expect(screen.getByTestId("device-list-unenrolled")).toBeTruthy();
    });
  });
});

describe("DeviceManagement — enrolled, device list", () => {
  beforeEach(() => {
    localStorage.clear();
    vi.clearAllMocks();
    setupDefaultMocks();
  });

  afterEach(() => localStorage.clear());

  it("renders enrolled devices with authenticated names and enrolled-at dates", async () => {
    const deviceA = makeDevice({ name: "Laptop", enrolledAt: "1700000000000000000", isCurrent: true });
    const deviceB = makeDevice({ x25519Pub: "x25519-b", signPub: "signb", name: "Mobile" });
    mockBuildDeviceList.mockResolvedValue([deviceA, deviceB]);

    render(<DeviceManagement />);
    await waitFor(() => {
      const names = screen.getAllByTestId("device-name").map((el) => el.textContent);
      expect(names).toContain("Laptop");
      expect(names).toContain("Mobile");
    });

    // Enrolled-at dates rendered
    const dates = screen.getAllByTestId("device-enrolled-at");
    expect(dates.length).toBeGreaterThanOrEqual(2);
  });

  it("shows 'This device' badge on the current device only", async () => {
    const current = makeDevice({ name: "Current", isCurrent: true });
    const other = makeDevice({ x25519Pub: "x25519-b", signPub: "signb", name: "Other" });
    mockBuildDeviceList.mockResolvedValue([current, other]);

    render(<DeviceManagement />);
    await waitFor(() => {
      expect(screen.getByTestId("current-device-badge")).toBeTruthy();
    });
    const badge = screen.getByTestId("current-device-badge");
    expect(badge.textContent).toBe("This device");
  });

  it("renders recovery device in distinct block with no revoke button", async () => {
    const normal = makeDevice({ name: "My Laptop", isCurrent: true });
    const recovery = makeDevice({
      x25519Pub: "x25519-rec",
      signPub: "recsignpub",
      name: "recovery",
      isRecovery: true,
    });
    mockBuildDeviceList.mockResolvedValue([normal, recovery]);
    mockIsRevocableByNormalRevoke.mockImplementation((item: DeviceListItem) => !item.isRecovery);

    render(<DeviceManagement />);
    await waitFor(() => {
      expect(screen.getByTestId("recovery-device-block")).toBeTruthy();
    });

    // Recovery block rendered distinctly
    expect(screen.getByTestId("recovery-device-block")).toBeTruthy();

    // Only one revoke button (for the normal device), not for recovery
    const revokeButtons = screen.getAllByTestId("revoke-button");
    expect(revokeButtons).toHaveLength(1);
  });

  it("does not show revoke button for recovery device", async () => {
    const recovery = makeDevice({ x25519Pub: "x25519-rec", signPub: "recsignpub", name: "recovery", isRecovery: true });
    mockBuildDeviceList.mockResolvedValue([recovery]);
    mockIsRevocableByNormalRevoke.mockReturnValue(false);

    render(<DeviceManagement />);
    await waitFor(() => {
      expect(screen.getByTestId("recovery-device-block")).toBeTruthy();
    });

    expect(screen.queryByTestId("revoke-button")).toBeNull();
  });
});

describe("DeviceManagement — revocation flow", () => {
  beforeEach(() => {
    localStorage.clear();
    vi.clearAllMocks();
    setupDefaultMocks();
  });

  afterEach(() => localStorage.clear());

  it("opens cascade dialog on revoke click (no recovery gate needed)", async () => {
    const current = makeDevice({ name: "Current", isCurrent: true });
    const other = makeDevice({ x25519Pub: "x25519-b", signPub: "signb", name: "Other" });
    mockBuildDeviceList.mockResolvedValue([current, other]);
    mockRequiresRecoveryConfirmation.mockReturnValue(false);
    mockCascadeForDevice.mockReturnValue([]);

    render(<DeviceManagement />);
    await waitFor(() => screen.getByText("Other"));

    const revokeButtons = screen.getAllByTestId("revoke-button");
    // Click the revoke button of the 'Other' device (second one)
    await act(async () => {
      await userEvent.click(revokeButtons[revokeButtons.length - 1]);
    });

    await waitFor(() => {
      expect(screen.getByTestId("cascade-dialog")).toBeTruthy();
    });
  });

  it("opens recovery gate before cascade dialog when current/last-non-recovery", async () => {
    const current = makeDevice({ name: "Only Device", isCurrent: true });
    const recovery = makeDevice({ x25519Pub: "x25519-rec", signPub: "recsignpub", name: "recovery", isRecovery: true });
    mockBuildDeviceList.mockResolvedValue([current, recovery]);
    mockIsRevocableByNormalRevoke.mockImplementation((item: DeviceListItem) => !item.isRecovery);
    mockRequiresRecoveryConfirmation.mockReturnValue(true);

    render(<DeviceManagement />);
    await waitFor(() => screen.getByText("Only Device"));

    const revokeBtn = screen.getByTestId("revoke-button");
    await act(async () => {
      await userEvent.click(revokeBtn);
    });

    await waitFor(() => {
      expect(screen.getByTestId("recovery-gate")).toBeTruthy();
    });
  });

  it("shows cascade prompt listing enrolled devices", async () => {
    const current = makeDevice({ name: "Current", isCurrent: true });
    const other = makeDevice({ x25519Pub: "x25519-b", signPub: "signb", name: "Other" });
    const cascaded = makeDevice({ x25519Pub: "x25519-c", signPub: "signc", name: "CascadeChild", enrolledBySignPub: "signb" });
    mockBuildDeviceList.mockResolvedValue([current, other, cascaded]);
    mockRequiresRecoveryConfirmation.mockReturnValue(false);
    // Return the cascade device when cascadeForDevice is called with other.signPub
    mockCascadeForDevice.mockReturnValue([{ sign_pub: "signc", x25519_pub: "x25519-c" }]);

    render(<DeviceManagement />);
    await waitFor(() => screen.getByText("Other"));

    // Click revoke on "Other" device (has sign_pub "signb")
    const revokeButtons = screen.getAllByTestId("revoke-button");
    await act(async () => {
      await userEvent.click(revokeButtons[0]);
    });

    await waitFor(() => {
      expect(screen.getByTestId("cascade-dialog")).toBeTruthy();
      expect(screen.getByTestId("cascade-list")).toBeTruthy();
    });
    expect(screen.getByTestId("cascade-list").textContent).toContain("CascadeChild");
  });

  it("shows in-progress banner with remaining count from SweepProgress", async () => {
    const device = makeDevice({ name: "My Device", isCurrent: true });
    mockBuildDeviceList.mockResolvedValue([device]);
    const progress = {
      targetVersion: 3,
      total: 5,
      done: 2,
      secretIds: ["s1", "s2", "s3", "s4", "s5"],
      completed: ["s1", "s2"],
      failed: [],
      isRevocation: true,
      updatedAt: Date.now(),
    };
    mockLoadSweepProgress.mockReturnValue(progress);
    mockRemainingCount.mockReturnValue(3);

    render(<DeviceManagement />);
    await waitFor(() => {
      expect(screen.getByTestId("revocation-banner")).toBeTruthy();
    });
    expect(screen.getByTestId("revocation-remaining").textContent).toContain("3");
  });

  it("Resume button is present when revocation is in progress", async () => {
    const device = makeDevice({ name: "My Device", isCurrent: true });
    mockBuildDeviceList.mockResolvedValue([device]);
    const progress = {
      targetVersion: 3, total: 2, done: 0,
      secretIds: ["s1", "s2"], completed: [], failed: [],
      isRevocation: true, updatedAt: Date.now(),
    };
    mockLoadSweepProgress.mockReturnValue(progress);
    mockRemainingCount.mockReturnValue(2);

    render(<DeviceManagement />);
    await waitFor(() => {
      expect(screen.getByTestId("revocation-resume")).toBeTruthy();
    });
  });
});

describe("DeviceManagement — error handling", () => {
  beforeEach(() => {
    localStorage.clear();
    vi.clearAllMocks();
  });

  afterEach(() => localStorage.clear());

  it("shows error message on buildDeviceList failure", async () => {
    mockLoadAnchor.mockReturnValue(sampleAnchor);
    mockLoadDeviceKeys.mockResolvedValue({ ecdsaPrivate: {}, ecdsaPublic: {} });
    mockExportDeviceRef.mockResolvedValue(sampleRef);
    mockFetchLog.mockResolvedValue({ log: { entries: [] }, head: "h", version: 2 });
    mockLoadSweepProgress.mockReturnValue(null);
    mockRemainingCount.mockReturnValue(0);
    mockBuildDeviceList.mockRejectedValue(new Error("chain verification failed"));

    render(<DeviceManagement />);
    await waitFor(() => {
      expect(screen.getByTestId("device-list-error")).toBeTruthy();
    });
    expect(screen.getByTestId("device-list-error").textContent).toContain("chain verification failed");
  });
});

// ── Security / correctness regression tests ───────────────────────────────────

describe("DeviceManagement — [WM6] resume re-seal passes pinned head version", () => {
  beforeEach(() => {
    localStorage.clear();
    vi.clearAllMocks();
    setupDefaultMocks();
  });

  afterEach(() => localStorage.clear());

  it("verifyDeviceSet is called with max(anchor.headVersion, progress.targetVersion)", async () => {
    // Simulates the interrupted-sweep case: anchor was never bumped (headVersion=3)
    // but the removal entries were already appended (targetVersion=7). The floor
    // must be 7, not 3 — otherwise a stale-prefix AS can serve the pre-removal
    // chain and the resume re-seals to the revoked device ([WM6]).
    const anchorWithVersion = { ...sampleAnchor, headVersion: 3 };
    mockLoadAnchor.mockReturnValue(anchorWithVersion);

    const device = makeDevice({ name: "My Device", isCurrent: true });
    mockBuildDeviceList.mockResolvedValue([device]);

    const progress = {
      targetVersion: 7, total: 2, done: 0,
      secretIds: ["s1", "s2"], completed: [], failed: [],
      isRevocation: true, revokedX25519Pub: "revokedpub==",
      updatedAt: Date.now(),
    };
    mockLoadSweepProgress.mockReturnValue(progress);
    mockRemainingCount.mockReturnValue(2);
    // Simulate AS serving the post-removal chain (revoked device absent).
    mockVerifyDeviceSet.mockResolvedValue({ members: [], headHash: new Uint8Array(32), headVersion: 7 });

    render(<DeviceManagement />);
    await waitFor(() => screen.getByTestId("revocation-resume"));

    await act(async () => {
      await userEvent.click(screen.getByTestId("revocation-resume"));
    });

    // Floor must be progress.targetVersion (7), not anchor.headVersion (3).
    await waitFor(() => {
      expect(mockVerifyDeviceSet).toHaveBeenCalledWith(
        expect.anything(),
        anchorWithVersion.ownerRoot,
        7,
      );
    });
  });

  it("resume aborts when AS chain still contains the revoked device", async () => {
    // Verifies the fail-closed guard: if the AS serves a chain where the
    // revoked device is still a member, the resume throws rather than
    // re-sealing to the revoked device.
    const anchorWithVersion = { ...sampleAnchor, headVersion: 3 };
    mockLoadAnchor.mockReturnValue(anchorWithVersion);

    const device = makeDevice({ name: "My Device", isCurrent: true });
    mockBuildDeviceList.mockResolvedValue([device]);

    const revokedPub = "revokedX25519pub==";
    const progress = {
      targetVersion: 7, total: 2, done: 0,
      secretIds: ["s1", "s2"], completed: [], failed: [],
      isRevocation: true, revokedX25519Pub: revokedPub,
      updatedAt: Date.now(),
    };
    mockLoadSweepProgress.mockReturnValue(progress);
    mockRemainingCount.mockReturnValue(2);
    // AS serves a stale chain: revoked device is still a member.
    mockVerifyDeviceSet.mockResolvedValue({
      members: [{ x25519_pub: revokedPub, sign_pub: "revokedsignpub==" }],
      headHash: new Uint8Array(32),
      headVersion: 7,
    });

    render(<DeviceManagement />);
    await waitFor(() => screen.getByTestId("revocation-resume"));

    await act(async () => {
      await userEvent.click(screen.getByTestId("revocation-resume"));
    });

    // executeSweep must NOT be called; an error must be shown instead.
    await waitFor(() => {
      expect(mockExecuteSweep).not.toHaveBeenCalled();
      expect(screen.getByTestId("device-list-error").textContent).toContain(
        "stale-prefix",
      );
    });
  });
});

describe("DeviceManagement — [WM15] cascade guard bypass prevention", () => {
  beforeEach(() => {
    localStorage.clear();
    vi.clearAllMocks();
    setupDefaultMocks();
  });

  afterEach(() => localStorage.clear());

  it("shows recovery gate when cascade expands target set to all non-recovery devices", async () => {
    // current = non-recovery device that also happens to be isCurrent
    // other   = target for revoke (non-current, non-last by itself)
    // recovery = recovery device (not counted for non-recovery guard)
    //
    // Revoking [other] alone: 1 survivor (current) → requiresRecoveryConfirmation = false
    // Cascade includes [current]: 0 survivors → requiresRecoveryConfirmation = true
    // The pre-cascade check fires only on [other.signPub]; the post-cascade check must
    // catch this and show the recovery gate before performing the revoke.
    const current = makeDevice({ x25519Pub: "x-curr", signPub: "sign-curr", name: "Current", isCurrent: true });
    const other = makeDevice({ x25519Pub: "x-other", signPub: "sign-other", name: "Other", isCurrent: false });
    const recovery = makeDevice({ x25519Pub: "x-rec", signPub: "sign-rec", name: "recovery", isRecovery: true });
    mockBuildDeviceList.mockResolvedValue([current, other, recovery]);
    mockIsRevocableByNormalRevoke.mockImplementation((item: DeviceListItem) => !item.isRecovery);

    // Pre-cascade: revoking [other] alone does not need recovery (current survives)
    // Post-cascade: revoking [other, current] removes all non-recovery → needs recovery
    mockRequiresRecoveryConfirmation.mockImplementation((_: DeviceListItem[], signPubs: string[]) => {
      return signPubs.length >= 2; // true only when cascade is included
    });

    // "other" device has "current" as a cascade child
    mockCascadeForDevice.mockImplementation((_, targetSignPub: string) => {
      if (targetSignPub === "sign-other") {
        return [{ sign_pub: "sign-curr", x25519_pub: "x-curr" }];
      }
      return [];
    });

    render(<DeviceManagement />);
    await waitFor(() => screen.getByText("Other"));

    // The device rows have testids based on x25519Pub prefix.
    // "other" has x25519Pub="x-other" → row testid = "device-row-x-other"
    const otherRow = screen.getByTestId("device-row-x-other");
    const otherRevokeBtn = otherRow.querySelector("[data-testid='revoke-button']") as HTMLElement;
    expect(otherRevokeBtn).toBeTruthy();
    await act(async () => {
      await userEvent.click(otherRevokeBtn);
    });

    // Cascade dialog should appear (pre-cascade check was false)
    await waitFor(() => {
      expect(screen.getByTestId("cascade-dialog")).toBeTruthy();
    });

    // Include cascade (adds "Current" to targets)
    await act(async () => {
      await userEvent.click(screen.getByTestId("cascade-include"));
    });

    // Confirm cascade
    await act(async () => {
      await userEvent.click(screen.getByTestId("cascade-confirm"));
    });

    // Recovery gate must appear because cascade pushes full set over the threshold
    await waitFor(() => {
      expect(screen.getByTestId("recovery-gate")).toBeTruthy();
    });
  });
});

describe("DeviceManagement — [WM15] post-rotation recovery gate uses current signPub", () => {
  beforeEach(() => {
    localStorage.clear();
    vi.clearAllMocks();
    setupDefaultMocks();
  });

  afterEach(() => localStorage.clear());

  it("RecoveryGate validates against current recovery member signPub, not genesis anchor", async () => {
    // The recovery device in the verified list has a ROTATED signPub that differs from
    // the genesis anchor's recovery_sign_pub. After recovery rotation, the user's valid
    // phrase derives to the new pubkey. The gate must use the current member's signPub.
    const genesisRecoverySignPub = "recsignpub"; // what's in sampleAnchor.ownerRoot

    // A distinct base64 string that represents the rotated recovery pubkey bytes.
    // The bytes are Buffer.from(rotatedRecoverySignPub, "base64") so that
    // toBase64(bytes) === rotatedRecoverySignPub (the mock uses Buffer base64 round-trip).
    const rotatedRecoverySignPub = "cm90YXRlZC1yZWNvdmVyeQ=="; // base64 of "rotated-recovery"
    const rotatedSignPubBytes = new Uint8Array(Buffer.from(rotatedRecoverySignPub, "base64"));

    const normalDevice = makeDevice({ x25519Pub: "x-curr", signPub: "sign-curr", name: "Only Device", isCurrent: true });
    const recoveryDevice = makeDevice({
      x25519Pub: "x-rec",
      signPub: rotatedRecoverySignPub, // the ROTATED signPub, not the genesis one
      name: "recovery",
      isRecovery: true,
    });
    mockBuildDeviceList.mockResolvedValue([normalDevice, recoveryDevice]);
    mockIsRevocableByNormalRevoke.mockImplementation((item: DeviceListItem) => !item.isRecovery);
    mockRequiresRecoveryConfirmation.mockReturnValue(true); // triggers gate

    render(<DeviceManagement />);
    await waitFor(() => screen.getByText("Only Device"));

    await act(async () => {
      await userEvent.click(screen.getByTestId("revoke-button"));
    });

    // Recovery gate is shown
    await waitFor(() => {
      expect(screen.getByTestId("recovery-gate")).toBeTruthy();
    });

    // Simulate entering a phrase that derives to the ROTATED pubkey (the correct current one).
    // deriveDeviceKeysFromMnemonic → keys, exportDeviceRef → { signPub: rotatedSignPubBytes }
    // toBase64(rotatedSignPubBytes) === rotatedRecoverySignPub → gate passes.
    mockDeriveDeviceKeysFromMnemonic.mockResolvedValue({ ecdsaPrivate: {}, ecdsaPublic: {} });
    mockExportDeviceRef.mockResolvedValue({ x25519Pub: new Uint8Array(4), signPub: rotatedSignPubBytes });

    await act(async () => {
      await userEvent.type(screen.getByTestId("recovery-phrase-input"), "some phrase words here");
    });
    await act(async () => {
      await userEvent.click(screen.getByTestId("recovery-gate-submit"));
    });

    // If the gate used the rotated pubkey (correct), the phrase passes and cascade appears.
    // If it used the genesis pubkey (bug), it would show an error instead.
    await waitFor(() => {
      expect(screen.queryByTestId("recovery-phrase-error")).toBeNull();
      expect(screen.getByTestId("cascade-dialog")).toBeTruthy();
    });

    // Sanity-check: the genesis pubkey is different from the rotated one.
    expect(genesisRecoverySignPub).not.toBe(rotatedRecoverySignPub);
  });
});
