/**
 * Recovery flow modal (Phase 6, [WM12]).
 *
 * In-browser recovery for a user whose only device is lost.
 * Flow:
 *   1. Show the M8 trusted-device warning verbatim (spec §3, [WM12]).
 *   2. User enters the BIP-39 recovery phrase.
 *   3. recoverAndRotate():
 *      - Derive recovery keys
 *      - Verify chain + confirm recovery key is enrolled
 *      - Enroll fresh device + new recovery virtual device
 *      - Remove old recovery virtual device
 *      - Store fresh device keys
 *   4. Display the NEW recovery phrase — the old one is now retired.
 *      Mandatory re-display of the loss disclosure copy.
 */

import { useState, useCallback } from "react";
import { recoverAndRotate, M8_TRUSTED_DEVICE_WARNING } from "@/keys/recovery";
import { LOSS_DISCLOSURE } from "@/keys/ceremony";
import { httpASTransport, type OwnerRoot } from "@/keys/deviceset";
import { asHttpUrl } from "@/config/endpoints";

interface RecoveryModalProps {
  bearerToken: string;
  ownerRoot: OwnerRoot;
  pinnedHeadVersion: number;
  secretIds: string[];
  onComplete: (newPhrase: string, persistGranted: boolean) => void;
  onCancel: () => void;
}

type RecoveryStep = "warning" | "enter-phrase" | "recovering" | "new-phrase" | "done" | "error";

export function RecoveryModal({
  bearerToken,
  ownerRoot,
  pinnedHeadVersion,
  secretIds,
  onComplete,
  onCancel,
}: RecoveryModalProps) {
  const [step, setStep] = useState<RecoveryStep>("warning");
  const [phraseInput, setPhraseInput] = useState("");
  const [newPhrase, setNewPhrase] = useState<string | null>(null);
  const [newPhraseConfirmed, setNewPhraseConfirmed] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const handleRecover = useCallback(async () => {
    setError(null);
    setStep("recovering");
    try {
      const result = await recoverAndRotate({
        recoveryPhrase: phraseInput.trim(),
        newDeviceName: "browser",
        ownerRoot,
        pinnedHeadVersion,
        transport: httpASTransport(asHttpUrl(""), bearerToken),
        secretIds,
      });
      setNewPhrase(result.newRecoveryPhrase);
      setStep("new-phrase");
    } catch (e: unknown) {
      setError((e as Error).message);
      setStep("error");
    }
  }, [phraseInput, ownerRoot, pinnedHeadVersion, bearerToken, secretIds]);

  const handleDone = useCallback(() => {
    if (!newPhrase) return;
    setStep("done");
    onComplete(newPhrase, true);
  }, [newPhrase, onComplete]);

  return (
    <div
      role="dialog"
      aria-modal="true"
      aria-label="Account recovery"
      className="fixed inset-0 bg-black/60 flex items-center justify-center z-50"
    >
      <div className="bg-background rounded-lg shadow-xl max-w-lg w-full mx-4 p-6 space-y-4">

        {step === "warning" && (
          <div className="space-y-4">
            <h2 className="text-lg font-semibold">Before you recover</h2>
            {/* M8 trusted-device warning verbatim from spec §3 */}
            <div
              role="alert"
              className="rounded border border-destructive/50 bg-destructive/10 p-4 text-sm"
            >
              <strong>Security warning:</strong> {M8_TRUSTED_DEVICE_WARNING}
            </div>
            <p className="text-sm text-muted-foreground">
              Entering your recovery phrase in an untrusted browser can expose all your secrets.
              This is the one flow where your seed material exists in page memory.
            </p>
            <button
              className="w-full px-4 py-2 rounded bg-primary text-primary-foreground font-medium"
              onClick={() => setStep("enter-phrase")}
            >
              I understand — continue on this trusted device
            </button>
            <button
              className="w-full px-4 py-2 rounded bg-muted text-muted-foreground text-sm"
              onClick={onCancel}
            >
              Cancel — use an enrolled device instead
            </button>
          </div>
        )}

        {step === "enter-phrase" && (
          <div className="space-y-4">
            <h2 className="text-lg font-semibold">Enter your recovery phrase</h2>
            <p className="text-sm text-muted-foreground">
              Enter the 24-word BIP-39 phrase. After recovery, a new phrase will be generated
              and the old one will be retired.
            </p>
            <textarea
              className="w-full h-28 rounded border border-input bg-background px-3 py-2 font-mono text-sm resize-none"
              placeholder="Enter your 24-word BIP-39 recovery phrase…"
              autoComplete="off"
              autoCorrect="off"
              autoCapitalize="off"
              spellCheck={false}
              value={phraseInput}
              onChange={(e) => setPhraseInput(e.target.value)}
            />
            {error && <p className="text-sm text-destructive">{error}</p>}
            <button
              className="w-full px-4 py-2 rounded bg-primary text-primary-foreground font-medium"
              onClick={handleRecover}
              disabled={!phraseInput.trim()}
            >
              Recover account
            </button>
            <button
              className="w-full px-4 py-2 rounded bg-muted text-muted-foreground text-sm"
              onClick={onCancel}
            >
              Cancel
            </button>
          </div>
        )}

        {step === "recovering" && (
          <div className="space-y-2">
            <h2 className="text-lg font-semibold">Recovering…</h2>
            <p className="text-sm text-muted-foreground">
              Enrolling fresh device and rotating recovery code.
            </p>
          </div>
        )}

        {step === "new-phrase" && newPhrase && (
          <div className="space-y-4">
            <h2 className="text-lg font-semibold">Your new recovery phrase</h2>
            <div
              role="alert"
              className="rounded border border-destructive/50 bg-destructive/10 p-3 text-sm"
            >
              <strong>Warning:</strong> {LOSS_DISCLOSURE}
            </div>
            <p className="text-sm text-muted-foreground">
              Your old recovery phrase has been retired. Write down this new phrase immediately.
            </p>
            <div className="rounded border border-border bg-muted p-4 font-mono text-sm break-all select-all">
              {newPhrase}
            </div>
            <label className="flex items-center gap-2 text-sm">
              <input
                type="checkbox"
                checked={newPhraseConfirmed}
                onChange={(e) => setNewPhraseConfirmed(e.target.checked)}
              />
              I have written down the new recovery phrase
            </label>
            <button
              className="w-full px-4 py-2 rounded bg-primary text-primary-foreground font-medium"
              onClick={handleDone}
              disabled={!newPhraseConfirmed}
            >
              Done
            </button>
          </div>
        )}

        {step === "error" && (
          <div className="space-y-3">
            <h2 className="text-lg font-semibold text-destructive">Recovery failed</h2>
            <p className="text-sm">{error}</p>
            <button
              className="px-4 py-2 rounded bg-muted text-muted-foreground"
              onClick={() => {
                setError(null);
                setStep("enter-phrase");
              }}
            >
              Try again
            </button>
            <button
              className="px-4 py-2 rounded bg-muted text-muted-foreground text-sm ml-2"
              onClick={onCancel}
            >
              Cancel
            </button>
          </div>
        )}
      </div>
    </div>
  );
}
