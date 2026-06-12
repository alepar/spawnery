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

vi.mock("@/keys/deviceset", () => ({
  httpASTransport: () => ({
    fetchLog: () => mockFetchLog(),
    append: () => mockAppend(),
  }),
  verifyDeviceSet: vi.fn().mockResolvedValue({ members: [], headHash: new Uint8Array(32), headVersion: 1 }),
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

vi.mock("@/keys/sweep", () => ({
  executeSweep: vi.fn(),
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
