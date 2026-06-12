/**
 * Key ceremony modal (Phase 4, [M14][WM11][WM10]).
 *
 * Opened by the first secret-bearing action. Guides the user through:
 *   1. Key generation (device + recovery keypairs)
 *   2. Recovery phrase display with mandatory loss-disclosure copy
 *   3. Phrase re-entry confirmation step ([WM11])
 *   4. navigator.storage.persist() call + surfaced result ([WM11])
 *   5. Round-trip check: re-derives recovery pubkeys from the confirmed phrase,
 *      asserts they equal the enrolled recovery DeviceRef ([WM10])
 *   6. Atomic genesis publication to the AS
 *
 * Abandoning mid-ceremony persists nothing ([M14]).
 */

import { useState, useEffect, useCallback } from "react";
import {
  startCeremony,
  ceremonyRoundTripCheck,
  completeCeremony,
  LOSS_DISCLOSURE,
  BROWSER_EVICTION_WARNING,
  type CeremonyContext,
} from "@/keys/ceremony";
import { featureDetectX25519 } from "@/keys/device";
import { asHttpUrl } from "@/config/endpoints";

export type CeremonyStep =
  | "feature-check"
  | "generating"
  | "show-phrase"
  | "confirm-phrase"
  | "publishing"
  | "done"
  | "error"
  | "unsupported";

interface CeremonyModalProps {
  /** Bearer token for the AS API. */
  bearerToken: string;
  /** Called when the ceremony completes successfully. The parked action resumes. */
  onComplete: (persistGranted: boolean) => void;
  /** Called when the user cancels or the ceremony cannot proceed. */
  onCancel: () => void;
  /** Optional device name (defaults to "device1"). */
  deviceName?: string;
}

export function CeremonyModal({
  bearerToken,
  onComplete,
  onCancel,
  deviceName = "device1",
}: CeremonyModalProps) {
  const [step, setStep] = useState<CeremonyStep>("feature-check");
  const [ctx, setCtx] = useState<CeremonyContext | null>(null);
  const [confirmedPhrase, setConfirmedPhrase] = useState("");
  const [persistGranted, setPersistGranted] = useState<boolean | null>(null);
  const [error, setError] = useState<string | null>(null);

  // Feature detection on mount
  useEffect(() => {
    featureDetectX25519().then((msg) => {
      if (msg) {
        setError(msg);
        setStep("unsupported");
      } else {
        setStep("generating");
      }
    });
  }, []);

  // Auto-generate keys once we reach the generating step
  useEffect(() => {
    if (step !== "generating") return;
    startCeremony({ deviceName })
      .then((c) => {
        setCtx(c);
        setStep("show-phrase");
      })
      .catch((e: Error) => {
        setError(e.message);
        setStep("error");
      });
  }, [step, deviceName]);

  const handlePhraseConfirm = useCallback(async () => {
    if (!ctx) return;
    setError(null);
    try {
      await ceremonyRoundTripCheck(ctx, confirmedPhrase.trim());
      setStep("publishing");
    } catch (e: unknown) {
      setError((e as Error).message);
    }
  }, [ctx, confirmedPhrase]);

  // Publish once we reach the publishing step
  useEffect(() => {
    if (step !== "publishing" || !ctx) return;
    completeCeremony(ctx, asHttpUrl(""), bearerToken)
      .then(({ persistGranted: granted }) => {
        setPersistGranted(granted);
        setStep("done");
        onComplete(granted);
      })
      .catch((e: Error) => {
        setError(e.message);
        setStep("error");
      });
  }, [step, ctx, bearerToken, onComplete]);

  return (
    <div
      role="dialog"
      aria-modal="true"
      aria-label="Key ceremony"
      className="fixed inset-0 bg-black/60 flex items-center justify-center z-50"
    >
      <div className="bg-background rounded-lg shadow-xl max-w-lg w-full mx-4 p-6 space-y-4">
        {step === "feature-check" && (
          <p className="text-muted-foreground text-sm">Checking browser support…</p>
        )}

        {step === "unsupported" && (
          <div className="space-y-3">
            <h2 className="text-lg font-semibold text-destructive">Browser not supported</h2>
            <p className="text-sm">{error}</p>
            <button
              className="px-4 py-2 rounded bg-muted text-muted-foreground"
              onClick={onCancel}
            >
              Close
            </button>
          </div>
        )}

        {step === "generating" && (
          <div className="space-y-2">
            <h2 className="text-lg font-semibold">Generating your keys…</h2>
            <p className="text-sm text-muted-foreground">
              Creating non-extractable device and recovery keypairs.
            </p>
            <div className="h-1 bg-muted rounded overflow-hidden">
              <div className="h-full bg-primary animate-pulse w-1/2" />
            </div>
          </div>
        )}

        {step === "show-phrase" && ctx && (
          <div className="space-y-4">
            <h2 className="text-lg font-semibold">Your recovery phrase</h2>
            <div
              role="alert"
              className="rounded border border-destructive/50 bg-destructive/10 p-3 text-sm"
            >
              <strong>Warning:</strong> {LOSS_DISCLOSURE}
            </div>
            <div className="rounded border border-border bg-muted p-4 font-mono text-sm break-all select-all">
              {ctx.recoveryPhrase}
            </div>
            <p className="text-xs text-muted-foreground">{BROWSER_EVICTION_WARNING}</p>
            <button
              className="w-full px-4 py-2 rounded bg-primary text-primary-foreground font-medium"
              onClick={() => setStep("confirm-phrase")}
            >
              I have written it down — continue
            </button>
            <button
              className="w-full px-4 py-2 rounded bg-muted text-muted-foreground text-sm"
              onClick={onCancel}
            >
              Cancel (nothing is saved)
            </button>
          </div>
        )}

        {step === "confirm-phrase" && ctx && (
          <div className="space-y-4">
            <h2 className="text-lg font-semibold">Confirm your recovery phrase</h2>
            <p className="text-sm text-muted-foreground">
              Enter the 24-word phrase you just wrote down to confirm it is correct.
            </p>
            <textarea
              className="w-full h-28 rounded border border-input bg-background px-3 py-2 font-mono text-sm resize-none"
              placeholder="Enter your 24-word BIP-39 recovery phrase…"
              autoComplete="off"
              autoCorrect="off"
              autoCapitalize="off"
              spellCheck={false}
              value={confirmedPhrase}
              onChange={(e) => setConfirmedPhrase(e.target.value)}
            />
            {error && (
              <p className="text-sm text-destructive">{error}</p>
            )}
            <button
              className="w-full px-4 py-2 rounded bg-primary text-primary-foreground font-medium"
              onClick={handlePhraseConfirm}
            >
              Confirm phrase
            </button>
          </div>
        )}

        {step === "publishing" && (
          <div className="space-y-2">
            <h2 className="text-lg font-semibold">Publishing your device set…</h2>
            <p className="text-sm text-muted-foreground">
              Registering your device with the account server.
            </p>
          </div>
        )}

        {step === "done" && (
          <div className="space-y-3">
            <h2 className="text-lg font-semibold">Ceremony complete</h2>
            {persistGranted === false && (
              <div
                role="alert"
                className="rounded border border-yellow-500/50 bg-yellow-500/10 p-3 text-sm"
              >
                <strong>Note:</strong> The browser denied persistent storage. Your keys may be
                evicted when browser data is cleared. Enroll a second device for resilience.
              </div>
            )}
            {persistGranted && (
              <p className="text-sm text-muted-foreground">
                Persistent storage granted. Your keys are protected from automatic eviction.
              </p>
            )}
          </div>
        )}

        {step === "error" && (
          <div className="space-y-3">
            <h2 className="text-lg font-semibold text-destructive">Ceremony failed</h2>
            <p className="text-sm">{error ?? "An unexpected error occurred."}</p>
            <button
              className="px-4 py-2 rounded bg-muted text-muted-foreground"
              onClick={onCancel}
            >
              Close
            </button>
          </div>
        )}
      </div>
    </div>
  );
}
