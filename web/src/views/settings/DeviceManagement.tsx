/**
 * Device management settings pane (W3).
 *
 * Renders the verified device list from the W2 crypto layer:
 *   - Each device's authenticated name + enrolled-at
 *   - "This device" badge for the current device
 *   - Recovery virtual device in a distinct block with NO revoke button
 *   - Revoke button per non-recovery device
 *   - Recovery-phrase confirmation gate for current/last-non-recovery revocation
 *   - Cascade prompt listing devices enrolled by the revokee
 *   - Persistent revocation-in-progress banner with Resume button
 *   - Unenrolled/anchor-absent state renders a placeholder
 *
 * All crypto work delegates to the W2 layer (keys/). No new crypto here.
 *
 * CP RPC GAP (sp-7h6.1): secrets re-seal uses a no-op stub client until the
 * CP GetSecret/PutSecret Connect RPCs land.
 */

import { useState, useEffect, useCallback } from "react";
import { buildDeviceList, type DeviceListItem } from "@/keys/devicelist";
import { isRevocableByNormalRevoke, requiresRecoveryConfirmation, revokeDevices } from "@/keys/revoke";
import { loadAnchor } from "@/keys/anchor";
import { loadDeviceKeys, exportDeviceRef } from "@/keys/device";
import { deriveDeviceKeysFromMnemonic } from "@/keys/device";
import { httpASTransport } from "@/keys/deviceset";
import { loadSweepProgress, remainingCount, type SweepProgress } from "@/keys/epoch";
import { executeSweep, type SecretsCPClient } from "@/keys/sweep";
import { M8_TRUSTED_DEVICE_WARNING } from "@/keys/recovery";
import { asHttpUrl } from "@/config/endpoints";
import { useSessionStore } from "@/auth/session";
import { toBase64, fromBase64 } from "@/keys/encoding";

// CP RPC GAP (sp-7h6.1): stub client until CP GetSecret/PutSecret land.
const stubCPClient: SecretsCPClient = {
  getEnvelope: () => Promise.reject(new Error("CP RPC GAP sp-7h6.1: secrets client not yet wired")),
  putEnvelope: () => Promise.resolve(),
};

function formatEnrolledAt(nanoStr: string): string {
  try {
    const ms = Number(BigInt(nanoStr) / 1_000_000n);
    return new Date(ms).toLocaleDateString(undefined, {
      year: "numeric",
      month: "short",
      day: "numeric",
    });
  } catch {
    return "unknown date";
  }
}

// ── Cascade confirmation dialog ───────────────────────────────────────────────

interface CascadeDialogProps {
  targetDevice: DeviceListItem;
  cascadeDevices: DeviceListItem[];
  onConfirm: (includeIds: string[]) => void;
  onCancel: () => void;
}

function CascadeDialog({ targetDevice, cascadeDevices, onConfirm, onCancel }: CascadeDialogProps) {
  const [includeCascade, setIncludeCascade] = useState(false);

  const handleConfirm = () => {
    const ids = [targetDevice.x25519Pub];
    if (includeCascade) {
      ids.push(...cascadeDevices.map((d) => d.x25519Pub));
    }
    onConfirm(ids);
  };

  return (
    <div className="fixed inset-0 bg-black/50 flex items-center justify-center z-50" data-testid="cascade-dialog">
      <div className="bg-background border border-border rounded-lg p-6 max-w-md w-full mx-4 space-y-4">
        <h3 className="text-base font-semibold">Revoke device: {targetDevice.name}</h3>
        {cascadeDevices.length > 0 && (
          <div className="space-y-2">
            <p className="text-sm text-muted-foreground">
              This device enrolled the following devices. You can revoke them in the same sweep:
            </p>
            <ul className="text-sm space-y-1 border border-border rounded p-3" data-testid="cascade-list">
              {cascadeDevices.map((d) => (
                <li key={d.x25519Pub} className="text-muted-foreground">
                  {d.name} <span className="text-xs">— enrolled {formatEnrolledAt(d.enrolledAt)}</span>
                </li>
              ))}
            </ul>
            <label className="flex items-center gap-2 text-sm cursor-pointer">
              <input
                type="checkbox"
                checked={includeCascade}
                onChange={(e) => setIncludeCascade(e.target.checked)}
                data-testid="cascade-include"
              />
              Also revoke the {cascadeDevices.length} device{cascadeDevices.length !== 1 ? "s" : ""} listed above
            </label>
          </div>
        )}
        <div className="flex gap-2 justify-end">
          <button
            type="button"
            onClick={onCancel}
            className="px-4 py-2 text-sm border border-border rounded hover:bg-muted"
            data-testid="cascade-cancel"
          >
            Cancel
          </button>
          <button
            type="button"
            onClick={handleConfirm}
            className="px-4 py-2 text-sm bg-destructive text-destructive-foreground rounded hover:bg-destructive/90"
            data-testid="cascade-confirm"
          >
            Revoke
          </button>
        </div>
      </div>
    </div>
  );
}

// ── Recovery phrase confirmation gate ─────────────────────────────────────────

interface RecoveryGateProps {
  onConfirmed: () => void;
  onCancel: () => void;
  recoverySignPub: string;
}

function RecoveryGate({ onConfirmed, onCancel, recoverySignPub }: RecoveryGateProps) {
  const [phrase, setPhrase] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [checking, setChecking] = useState(false);

  const handleCheck = async () => {
    setChecking(true);
    setError(null);
    try {
      const derived = await deriveDeviceKeysFromMnemonic(phrase.trim());
      const ref = await exportDeviceRef(derived);
      const derivedSignPub = toBase64(ref.signPub);
      if (derivedSignPub !== recoverySignPub) {
        setError("Recovery phrase does not match. Check the phrase and try again.");
      } else {
        onConfirmed();
      }
    } catch (e) {
      setError(e instanceof Error ? e.message : "Invalid recovery phrase");
    } finally {
      setChecking(false);
    }
  };

  return (
    <div className="fixed inset-0 bg-black/50 flex items-center justify-center z-50" data-testid="recovery-gate">
      <div className="bg-background border border-border rounded-lg p-6 max-w-md w-full mx-4 space-y-4">
        <h3 className="text-base font-semibold">Recovery phrase required</h3>
        <p className="text-sm bg-amber-50 dark:bg-amber-950 border border-amber-200 dark:border-amber-800 rounded p-3 text-amber-800 dark:text-amber-200">
          {M8_TRUSTED_DEVICE_WARNING}
        </p>
        <p className="text-sm text-muted-foreground">
          You are revoking your current device or the last non-recovery device. Enter your
          recovery phrase to confirm you still have access before proceeding.
        </p>
        <textarea
          rows={3}
          placeholder="Enter recovery phrase (BIP-39 words)"
          value={phrase}
          onChange={(e) => setPhrase(e.target.value)}
          autoComplete="off"
          className="w-full rounded-md border border-input bg-transparent px-3 py-2 text-sm resize-none focus-visible:border-ring outline-none"
          data-testid="recovery-phrase-input"
        />
        {error && (
          <p className="text-sm text-destructive" data-testid="recovery-phrase-error">{error}</p>
        )}
        <div className="flex gap-2 justify-end">
          <button type="button" onClick={onCancel} className="px-4 py-2 text-sm border border-border rounded hover:bg-muted" data-testid="recovery-gate-cancel">
            Cancel
          </button>
          <button
            type="button"
            onClick={handleCheck}
            disabled={checking || !phrase.trim()}
            className="px-4 py-2 text-sm bg-primary text-primary-foreground rounded hover:bg-primary/90 disabled:opacity-50"
            data-testid="recovery-gate-submit"
          >
            {checking ? "Checking…" : "Confirm"}
          </button>
        </div>
      </div>
    </div>
  );
}

// ── In-progress banner ────────────────────────────────────────────────────────

interface RevocationBannerProps {
  progress: SweepProgress;
  onResume: () => void;
}

function RevocationBanner({ progress, onResume }: RevocationBannerProps) {
  const remaining = remainingCount(progress);
  return (
    <div
      className="rounded-lg border border-amber-300 dark:border-amber-700 bg-amber-50 dark:bg-amber-950 p-4 text-sm space-y-2"
      data-testid="revocation-banner"
    >
      <p className="font-medium text-amber-800 dark:text-amber-200">
        Revocation in progress
      </p>
      {remaining > 0 && (
        <p className="text-amber-700 dark:text-amber-300" data-testid="revocation-remaining">
          {remaining} secret{remaining !== 1 ? "s" : ""} still openable by the removed device.
        </p>
      )}
      <button
        type="button"
        onClick={onResume}
        className="text-xs text-amber-700 dark:text-amber-300 underline hover:no-underline"
        data-testid="revocation-resume"
      >
        Resume re-seal
      </button>
    </div>
  );
}

// ── Main component ────────────────────────────────────────────────────────────

export function DeviceManagement() {
  const [devices, setDevices] = useState<DeviceListItem[] | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [sweepProgress, setSweepProgress] = useState<SweepProgress | null>(null);
  const [revoking, setRevoking] = useState(false);
  const [revokingTarget, setRevokingTarget] = useState<DeviceListItem | null>(null);
  const [pendingTargetX25519Pubs, setPendingTargetX25519Pubs] = useState<string[] | null>(null);
  const [showRecoveryGate, setShowRecoveryGate] = useState(false);
  const [showCascadeDialog, setShowCascadeDialog] = useState(false);
  const [cascadeDevices, setCascadeDevices] = useState<DeviceListItem[]>([]);
  const [currentSignPub, setCurrentSignPub] = useState<string | undefined>(undefined);

  const token = useSessionStore((s) => s.getAccessToken());

  const loadDeviceList = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const anchor = loadAnchor();
      if (!anchor) {
        setDevices(null);
        setLoading(false);
        return;
      }

      const deviceKeys = await loadDeviceKeys();
      if (!deviceKeys) {
        setDevices(null);
        setLoading(false);
        return;
      }

      const ref = await exportDeviceRef(deviceKeys);
      const signPub = toBase64(ref.signPub);
      setCurrentSignPub(signPub);

      const transport = httpASTransport(asHttpUrl(""), token);
      const { log } = await transport.fetchLog();

      const list = await buildDeviceList(log, anchor.ownerRoot, {
        currentSignPub: signPub,
        pinnedHeadVersion: anchor.headVersion,
      });
      setDevices(list);

      // Load sweep progress for the in-progress banner
      setSweepProgress(loadSweepProgress());
    } catch (e) {
      setError(e instanceof Error ? e.message : "Failed to load device list");
      setDevices(null);
    } finally {
      setLoading(false);
    }
  }, [token]);

  useEffect(() => {
    void loadDeviceList();
  }, [loadDeviceList]);

  const handleRevoke = async (device: DeviceListItem) => {
    if (!devices) return;
    setRevokingTarget(device);

    // Build cascade devices from the full log
    const anchor = loadAnchor();
    if (!anchor) return;
    const transport = httpASTransport(asHttpUrl(""), token);
    const { log } = await transport.fetchLog();

    // Get transitive cascade as DeviceListItem[] (filter from current list)
    const { cascadeForDevice } = await import("@/keys/devicelist");
    const cascadeRefs = cascadeForDevice(log, device.signPub);
    const cascadeItems = cascadeRefs
      .map((r) => devices.find((d) => d.signPub === r.sign_pub))
      .filter((d): d is DeviceListItem => d !== undefined);
    setCascadeDevices(cascadeItems);

    // Check if recovery confirmation is needed
    const needsRecovery = requiresRecoveryConfirmation(devices, [device.signPub]);
    if (needsRecovery) {
      setShowRecoveryGate(true);
    } else {
      setShowCascadeDialog(true);
    }
  };

  const handleRecoveryGateConfirmed = () => {
    setShowRecoveryGate(false);
    setShowCascadeDialog(true);
  };

  const handleCascadeConfirm = async (targetX25519Pubs: string[]) => {
    setShowCascadeDialog(false);
    setPendingTargetX25519Pubs(targetX25519Pubs);
    await performRevoke(targetX25519Pubs);
  };

  const performRevoke = async (targetX25519Pubs: string[]) => {
    if (!devices || !currentSignPub) return;

    const anchor = loadAnchor();
    if (!anchor) {
      setError("No local anchor — cannot revoke without a pinned trust root");
      return;
    }

    const deviceKeys = await loadDeviceKeys();
    if (!deviceKeys) {
      setError("Device keys not found");
      return;
    }

    const ref = await exportDeviceRef(deviceKeys);
    const signerRef = {
      x25519_pub: toBase64(ref.x25519Pub),
      sign_pub: toBase64(ref.signPub),
    };

    const targetSet = new Set(targetX25519Pubs);
    const survivorX25519Pubs = devices
      .filter((d) => !targetSet.has(d.x25519Pub))
      .map((d) => d.x25519Pub);

    setRevoking(true);
    setError(null);
    try {
      const transport = httpASTransport(asHttpUrl(""), token);
      const progress = await revokeDevices({
        transport,
        signerKeys: deviceKeys,
        signerRef,
        ownerRoot: anchor.ownerRoot,
        pinnedHeadVersion: anchor.headVersion,
        targetX25519Pubs,
        survivorX25519Pubs,
        secretIds: [], // CP RPC GAP sp-7h6.1: no secrets to re-seal yet
        cpClient: stubCPClient,
        onProgress: (p) => setSweepProgress(p),
      });
      setSweepProgress(progress);
      await loadDeviceList();
    } catch (e) {
      setError(e instanceof Error ? e.message : "Revocation failed");
    } finally {
      setRevoking(false);
      setPendingTargetX25519Pubs(null);
      setRevokingTarget(null);
    }
  };

  const handleResumeRevocation = async () => {
    const progress = loadSweepProgress();
    if (!progress) return;

    const deviceKeys = await loadDeviceKeys();
    if (!deviceKeys) {
      setError("Device keys not found — cannot resume sweep");
      return;
    }

    const anchor = loadAnchor();
    if (!anchor) return;

    const transport = httpASTransport(asHttpUrl(""), token);
    const { log } = await transport.fetchLog();
    const { members } = await import("@/keys/deviceset").then(m => m.verifyDeviceSet(log, anchor.ownerRoot));

    const newMemberPubs = members.map((m) => fromBase64(m.x25519_pub));

    setRevoking(true);
    try {
      const final = await executeSweep({
        progress,
        deviceKeys,
        newMemberPubs,
        cpClient: stubCPClient,
        onProgress: (p) => setSweepProgress(p),
      });
      setSweepProgress(final);
    } catch (e) {
      setError(e instanceof Error ? e.message : "Resume failed");
    } finally {
      setRevoking(false);
    }
  };

  // ── Render ────────────────────────────────────────────────────────────────

  const recoveryDevice = devices?.find((d) => d.isRecovery);
  const realDevices = devices?.filter((d) => !d.isRecovery) ?? [];
  const anchorRecoverySignPub = loadAnchor()?.ownerRoot.recovery_sign_pub ?? "";

  return (
    <div className="space-y-6" data-testid="device-management">
      {/* Sweep progress banner */}
      {sweepProgress && remainingCount(sweepProgress) > 0 && (
        <RevocationBanner progress={sweepProgress} onResume={handleResumeRevocation} />
      )}

      {loading && (
        <p className="text-sm text-muted-foreground" data-testid="device-list-loading">Loading devices…</p>
      )}

      {error && (
        <p className="text-sm text-destructive" data-testid="device-list-error">{error}</p>
      )}

      {!loading && !error && devices === null && (
        <div className="space-y-2" data-testid="device-list-unenrolled">
          <p className="text-sm font-medium">Not enrolled</p>
          <p className="text-sm text-muted-foreground">
            Complete the key ceremony or enroll this device from another enrolled device to manage keys.
          </p>
        </div>
      )}

      {!loading && !error && devices !== null && (
        <>
          {/* Real devices */}
          <div className="space-y-3" data-testid="device-list">
            <h3 className="text-sm font-semibold">Enrolled devices</h3>
            {realDevices.length === 0 && (
              <p className="text-sm text-muted-foreground">No enrolled devices.</p>
            )}
            {realDevices.map((device) => (
              <DeviceRow
                key={device.x25519Pub}
                device={device}
                onRevoke={isRevocableByNormalRevoke(device) ? () => void handleRevoke(device) : undefined}
                revoking={revoking && revokingTarget?.x25519Pub === device.x25519Pub}
              />
            ))}
          </div>

          {/* Recovery virtual device — distinct block, no revoke */}
          {recoveryDevice && (
            <div className="space-y-2 border border-border rounded-lg p-4" data-testid="recovery-device-block">
              <h3 className="text-sm font-semibold">Recovery phrase device</h3>
              <p className="text-xs text-muted-foreground">
                Virtual device backed by your BIP-39 recovery phrase. Cannot be revoked from this panel — use the recovery flow to rotate it.
              </p>
              <div className="text-sm">
                <span className="font-medium">{recoveryDevice.name}</span>
                <span className="ml-2 text-xs text-muted-foreground">
                  enrolled {formatEnrolledAt(recoveryDevice.enrolledAt)}
                </span>
              </div>
            </div>
          )}
        </>
      )}

      {/* Recovery gate dialog */}
      {showRecoveryGate && revokingTarget && (
        <RecoveryGate
          recoverySignPub={anchorRecoverySignPub}
          onConfirmed={handleRecoveryGateConfirmed}
          onCancel={() => {
            setShowRecoveryGate(false);
            setRevokingTarget(null);
          }}
        />
      )}

      {/* Cascade confirmation dialog */}
      {showCascadeDialog && revokingTarget && (
        <CascadeDialog
          targetDevice={revokingTarget}
          cascadeDevices={cascadeDevices}
          onConfirm={(ids) => void handleCascadeConfirm(ids)}
          onCancel={() => {
            setShowCascadeDialog(false);
            setRevokingTarget(null);
          }}
        />
      )}

      {/* Suppress unused import warning */}
      {void pendingTargetX25519Pubs}
    </div>
  );
}

// ── Device row ────────────────────────────────────────────────────────────────

interface DeviceRowProps {
  device: DeviceListItem;
  onRevoke?: () => void;
  revoking?: boolean;
}

function DeviceRow({ device, onRevoke, revoking }: DeviceRowProps) {
  return (
    <div
      className="flex items-center justify-between border border-border rounded-lg px-4 py-3"
      data-testid={`device-row-${device.x25519Pub.slice(0, 8)}`}
    >
      <div className="space-y-0.5">
        <div className="flex items-center gap-2">
          <span className="text-sm font-medium" data-testid="device-name">{device.name}</span>
          {device.isCurrent && (
            <span
              className="text-xs bg-primary/10 text-primary rounded px-1.5 py-0.5"
              data-testid="current-device-badge"
            >
              This device
            </span>
          )}
        </div>
        <p className="text-xs text-muted-foreground" data-testid="device-enrolled-at">
          Enrolled {formatEnrolledAt(device.enrolledAt)}
        </p>
        {device.enrolledBySignPub && (
          <p className="text-xs text-muted-foreground">
            by {device.enrolledBySignPub.slice(0, 12)}…
          </p>
        )}
      </div>
      {onRevoke && (
        <button
          type="button"
          onClick={onRevoke}
          disabled={revoking}
          className="text-xs text-destructive hover:underline disabled:opacity-50"
          data-testid="revoke-button"
        >
          {revoking ? "Revoking…" : "Revoke"}
        </button>
      )}
    </div>
  );
}
