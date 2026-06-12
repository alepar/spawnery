/**
 * Device enrollment modal (Phase 5, [WM4][WM5][WM2][WM7]).
 *
 * Two sides:
 *   A) EnrolleeModal — the new device that wants to be enrolled.
 *      Shows a QR/link and SAS code. Waits for the approver to send back
 *      OwnerRoot + head.
 *
 *   B) ApproverModal — an already-enrolled device approving a new one.
 *      Scans or pastes the enrollment link, displays the SAS, waits for
 *      the human to confirm it matches, then approves.
 *
 * SAS: derived from (genesis hash ‖ head ‖ new pubkeys). NEVER parsed from
 * the link ([WM4]). Both sides independently compute it.
 *
 * After approval, the approver triggers the re-seal sweep ([WM2]).
 * The cascade prompt ([WM7]) surfaces devices enrolled by the approver
 * transitively, offering to remove them in the same sweep.
 */

import { useState, useEffect, useCallback } from "react";
import {
  generateEnrollmentPayload,
  serializeEnrollmentPayload,
  deserializeEnrollmentPayload,
  computeEnrollmentSAS,
  approveEnrollment,
  finalizeEnrollment,
  type EnrollmentPayload,
  type EnrollmentLinkResult,
  type ApprovalResult,
} from "@/keys/enrollment";
import { loadDeviceKeys } from "@/keys/device";
import { fetchDeviceSetLog, type OwnerRoot, type DeviceRef } from "@/keys/deviceset";
import { asHttpUrl } from "@/config/endpoints";

// ── Enrollee side ─────────────────────────────────────────────────────────────

interface EnrolleeModalProps {
  bearerToken: string;
  deviceName?: string;
  onComplete: (persistGranted: boolean) => void;
  onCancel: () => void;
}

type EnrolleeStep = "generating" | "show-link" | "waiting" | "finalizing" | "done" | "error";

export function EnrolleeModal({
  bearerToken,
  deviceName = "browser",
  onComplete,
  onCancel,
}: EnrolleeModalProps) {
  const [step, setStep] = useState<EnrolleeStep>("generating");
  const [linkResult, setLinkResult] = useState<EnrollmentLinkResult | null>(null);
  const [sas, setSas] = useState<string | null>(null);
  const [enrollLink, setEnrollLink] = useState<string>("");
  const [approvalJSON, setApprovalJSON] = useState("");
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (step !== "generating") return;
    generateEnrollmentPayload(deviceName)
      .then(async (result) => {
        setLinkResult(result);
        const encoded = serializeEnrollmentPayload(result.payload);
        setEnrollLink(window.location.origin + "/enroll#" + encoded);

        // Compute SAS — we need the current chain head for this
        try {
          const { log, head } = await fetchDeviceSetLog(asHttpUrl(""), bearerToken);
          const genesisHash = await computeGenesisHash(log);
          const headBytes = new Uint8Array(atob(head).split("").map((c) => c.charCodeAt(0)));
          const code = await computeEnrollmentSAS(genesisHash, headBytes, result.payload);
          setSas(code);
        } catch {
          // SAS derivation failure is non-fatal at this stage
        }
        setStep("show-link");
      })
      .catch((e: Error) => {
        setError(e.message);
        setStep("error");
      });
  }, [step, deviceName, bearerToken]);

  const handleFinalize = useCallback(async () => {
    if (!linkResult) return;
    setError(null);
    try {
      const approval = JSON.parse(approvalJSON);
      setStep("finalizing");
      const { persistGranted } = await finalizeEnrollment({
        pendingKeys: linkResult.keys,
        approval,
        asUrl: asHttpUrl(""),
        bearerToken,
      });
      setStep("done");
      onComplete(persistGranted);
    } catch (e: unknown) {
      setError((e as Error).message);
      setStep("show-link");
    }
  }, [linkResult, approvalJSON, bearerToken, onComplete]);

  return (
    <div
      role="dialog"
      aria-modal="true"
      aria-label="Enroll this device"
      className="fixed inset-0 bg-black/60 flex items-center justify-center z-50"
    >
      <div className="bg-background rounded-lg shadow-xl max-w-lg w-full mx-4 p-6 space-y-4">
        {step === "generating" && (
          <div className="space-y-2">
            <h2 className="text-lg font-semibold">Generating enrollment keys…</h2>
          </div>
        )}

        {step === "show-link" && linkResult && (
          <div className="space-y-4">
            <h2 className="text-lg font-semibold">Enroll this device</h2>
            <p className="text-sm text-muted-foreground">
              Open this link or scan the QR code on an already-enrolled device.
              The link expires in 15 minutes and is single-use.
            </p>
            <div className="rounded border border-input bg-muted p-3 font-mono text-xs break-all select-all">
              {enrollLink}
            </div>
            {sas && (
              <div className="space-y-1">
                <p className="text-sm font-medium">
                  Verify this code matches on both devices before approving:
                </p>
                <div className="rounded border border-primary/50 bg-primary/5 p-3 text-center font-mono text-2xl tracking-widest font-bold">
                  {sas}
                </div>
                <p className="text-xs text-muted-foreground">
                  This code is never transmitted — both sides derive it independently ([WM4]).
                </p>
              </div>
            )}
            <p className="text-sm font-medium mt-4">
              After approval, paste the approval response below:
            </p>
            <textarea
              className="w-full h-20 rounded border border-input bg-background px-3 py-2 font-mono text-xs resize-none"
              placeholder='{"ownerRoot":{"device1_sign_pub":"…","recovery_sign_pub":"…"},"headHash":"…","headVersion":2}'
              value={approvalJSON}
              onChange={(e) => setApprovalJSON(e.target.value)}
            />
            {error && <p className="text-sm text-destructive">{error}</p>}
            <button
              className="w-full px-4 py-2 rounded bg-primary text-primary-foreground font-medium"
              onClick={handleFinalize}
              disabled={!approvalJSON.trim()}
            >
              Complete enrollment
            </button>
            <button
              className="w-full px-4 py-2 rounded bg-muted text-muted-foreground text-sm"
              onClick={onCancel}
            >
              Cancel
            </button>
          </div>
        )}

        {step === "finalizing" && (
          <div className="space-y-2">
            <h2 className="text-lg font-semibold">Finalizing enrollment…</h2>
            <p className="text-sm text-muted-foreground">Verifying chain and storing device keys.</p>
          </div>
        )}

        {step === "done" && (
          <div>
            <h2 className="text-lg font-semibold">Device enrolled</h2>
            <p className="text-sm text-muted-foreground mt-2">
              This browser is now an enrolled device.
            </p>
          </div>
        )}

        {step === "error" && (
          <div className="space-y-3">
            <h2 className="text-lg font-semibold text-destructive">Enrollment failed</h2>
            <p className="text-sm">{error}</p>
            <button className="px-4 py-2 rounded bg-muted" onClick={onCancel}>Close</button>
          </div>
        )}
      </div>
    </div>
  );
}

// ── Approver side ─────────────────────────────────────────────────────────────

interface ApproverModalProps {
  bearerToken: string;
  ownerRoot: OwnerRoot;
  pinnedHeadVersion: number;
  secretIds: string[];
  onComplete: (result: ApprovalResult) => void;
  onCancel: () => void;
}

type ApproverStep = "paste-link" | "show-sas" | "approving" | "done" | "error";

export function ApproverModal({
  bearerToken,
  ownerRoot,
  pinnedHeadVersion,
  secretIds,
  onComplete,
  onCancel,
}: ApproverModalProps) {
  const [step, setStep] = useState<ApproverStep>("paste-link");
  const [linkInput, setLinkInput] = useState("");
  const [payload, setPayload] = useState<EnrollmentPayload | null>(null);
  const [sas, setSas] = useState<string | null>(null);
  const [sasConfirmed, setSasConfirmed] = useState(false);
  const [approvalResult, setApprovalResult] = useState<ApprovalResult | null>(null);
  const [cascadeDevices, setCascadeDevices] = useState<DeviceRef[]>([]);
  const [error, setError] = useState<string | null>(null);

  const handleParseLinkAndSAS = useCallback(async () => {
    setError(null);
    try {
      // Extract the encoded payload from the URL fragment
      const fragment = linkInput.includes("#")
        ? linkInput.split("#")[1]
        : linkInput.trim();
      const parsed = deserializeEnrollmentPayload(fragment);

      // Check expiry
      if (new Date(parsed.expiresAt) < new Date()) {
        throw new Error("This enrollment link has expired. Ask the enrollee to generate a new one.");
      }

      setPayload(parsed);

      // Compute SAS — need current chain state
      const { log, head } = await fetchDeviceSetLog(asHttpUrl(""), bearerToken);
      const genesisHash = await computeGenesisHash(log);
      const headBytes = new Uint8Array(atob(head).split("").map((c) => c.charCodeAt(0)));
      const code = await computeEnrollmentSAS(genesisHash, headBytes, parsed);
      setSas(code);
      setStep("show-sas");
    } catch (e: unknown) {
      setError((e as Error).message);
    }
  }, [linkInput, bearerToken]);

  const handleApprove = useCallback(async () => {
    if (!payload) return;
    setError(null);
    setStep("approving");
    try {
      const approverKeys = await loadDeviceKeys();
      if (!approverKeys) throw new Error("No device keys found — are you enrolled?");

      const result = await approveEnrollment({
        payload,
        asUrl: asHttpUrl(""),
        bearerToken,
        approverKeys,
        ownerRoot,
        secretIds,
        pinnedHeadVersion,
      });
      setApprovalResult(result);
      setCascadeDevices(result.cascadeDevices);
      setStep("done");
      onComplete(result);
    } catch (e: unknown) {
      setError((e as Error).message);
      setStep("error");
    }
  }, [payload, bearerToken, ownerRoot, secretIds, pinnedHeadVersion, onComplete]);

  return (
    <div
      role="dialog"
      aria-modal="true"
      aria-label="Approve device enrollment"
      className="fixed inset-0 bg-black/60 flex items-center justify-center z-50"
    >
      <div className="bg-background rounded-lg shadow-xl max-w-lg w-full mx-4 p-6 space-y-4">
        {step === "paste-link" && (
          <div className="space-y-4">
            <h2 className="text-lg font-semibold">Approve device enrollment</h2>
            <p className="text-sm text-muted-foreground">
              Paste the enrollment link from the new device below.
            </p>
            <input
              className="w-full rounded border border-input bg-background px-3 py-2 text-sm font-mono"
              placeholder="https://app.spawnery.dev/enroll#…"
              value={linkInput}
              onChange={(e) => setLinkInput(e.target.value)}
            />
            {error && <p className="text-sm text-destructive">{error}</p>}
            <button
              className="w-full px-4 py-2 rounded bg-primary text-primary-foreground font-medium"
              onClick={handleParseLinkAndSAS}
              disabled={!linkInput.trim()}
            >
              Verify link
            </button>
            <button
              className="w-full px-4 py-2 rounded bg-muted text-muted-foreground text-sm"
              onClick={onCancel}
            >
              Cancel
            </button>
          </div>
        )}

        {step === "show-sas" && payload && (
          <div className="space-y-4">
            <h2 className="text-lg font-semibold">Verify the code</h2>
            <p className="text-sm text-muted-foreground">
              Confirm this code matches what the device <strong>{payload.deviceName}</strong> is showing.
              If the codes don&apos;t match, do NOT approve — the link may have been tampered.
            </p>
            {sas && (
              <div className="rounded border border-primary/50 bg-primary/5 p-4 text-center font-mono text-2xl tracking-widest font-bold">
                {sas}
              </div>
            )}
            <label className="flex items-center gap-2 text-sm">
              <input
                type="checkbox"
                checked={sasConfirmed}
                onChange={(e) => setSasConfirmed(e.target.checked)}
              />
              The code matches on both devices
            </label>
            {error && <p className="text-sm text-destructive">{error}</p>}
            <button
              className="w-full px-4 py-2 rounded bg-primary text-primary-foreground font-medium"
              onClick={handleApprove}
              disabled={!sasConfirmed}
            >
              Approve enrollment
            </button>
            <button
              className="w-full px-4 py-2 rounded bg-muted text-muted-foreground text-sm"
              onClick={onCancel}
            >
              Cancel
            </button>
          </div>
        )}

        {step === "approving" && (
          <div className="space-y-2">
            <h2 className="text-lg font-semibold">Approving…</h2>
            <p className="text-sm text-muted-foreground">
              Publishing enrollment entry and queuing re-seal sweep.
            </p>
          </div>
        )}

        {step === "done" && approvalResult && (
          <div className="space-y-3">
            <h2 className="text-lg font-semibold">Enrollment approved</h2>
            {cascadeDevices.length > 0 && (
              <div
                role="alert"
                className="rounded border border-yellow-500/50 bg-yellow-500/10 p-3 text-sm"
              >
                <strong>Note ([WM7]):</strong> {cascadeDevices.length} device(s) were enrolled
                by this approver chain. Review them in Device Settings if you want to revoke
                them in the same sweep.
              </div>
            )}
            <p className="text-sm text-muted-foreground">
              Copy the approval response to the enrollee:
            </p>
            <div className="rounded border border-input bg-muted p-3 font-mono text-xs break-all select-all">
              {JSON.stringify({
                ownerRoot: approvalResult.ownerRoot,
                headHash: approvalResult.headHash,
                headVersion: approvalResult.headVersion,
              })}
            </div>
          </div>
        )}

        {step === "error" && (
          <div className="space-y-3">
            <h2 className="text-lg font-semibold text-destructive">Approval failed</h2>
            <p className="text-sm">{error}</p>
            <button className="px-4 py-2 rounded bg-muted" onClick={onCancel}>Close</button>
          </div>
        )}
      </div>
    </div>
  );
}

// ── Shared helpers ────────────────────────────────────────────────────────────

async function computeGenesisHash(log: { entries: Array<{ body: string; sigs: Array<{ signer_pub: string; sig: string }> }> }): Promise<Uint8Array> {
  const { encodeFields, sha256, fromBase64: fb64 } = await import("@/keys/encoding");
  const genesis = log.entries[0];
  const parts = [fb64(genesis.body)];
  for (const s of genesis.sigs) {
    parts.push(fb64(s.signer_pub));
    parts.push(fb64(s.sig));
  }
  return sha256(encodeFields(...parts));
}
