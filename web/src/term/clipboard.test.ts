import { afterEach, describe, expect, it, vi } from "vitest";
import { writeClipboard } from "./clipboard";

// Helpers to drive the two branches: secure-context async Clipboard API vs the
// plain-HTTP LAN execCommand fallback. jsdom implements neither, so we define them.
function setSecureContext(v: boolean) {
  Object.defineProperty(window, "isSecureContext", { value: v, configurable: true });
}

function stubExecCommand(impl: () => boolean) {
  const fn = vi.fn(impl);
  // execCommand is not implemented in jsdom — define it so the fallback path can run.
  Object.defineProperty(document, "execCommand", { value: fn, configurable: true, writable: true });
  return fn;
}

afterEach(() => {
  vi.restoreAllMocks();
  delete (navigator as { clipboard?: unknown }).clipboard;
  delete (document as { execCommand?: unknown }).execCommand;
});

describe("writeClipboard", () => {
  it("returns false for empty text without touching the clipboard", async () => {
    const writeText = vi.fn();
    Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true });
    setSecureContext(true);

    expect(await writeClipboard("")).toBe(false);
    expect(writeText).not.toHaveBeenCalled();
  });

  it("uses the async Clipboard API in a secure context", async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true });
    setSecureContext(true);
    const exec = stubExecCommand(() => true);

    expect(await writeClipboard("hello")).toBe(true);
    expect(writeText).toHaveBeenCalledWith("hello");
    expect(exec).not.toHaveBeenCalled(); // secure path wins; no fallback
  });

  it("falls back to execCommand when the Clipboard API rejects", async () => {
    const writeText = vi.fn().mockRejectedValue(new Error("denied"));
    Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true });
    setSecureContext(true);
    const exec = stubExecCommand(() => true);

    expect(await writeClipboard("world")).toBe(true);
    expect(writeText).toHaveBeenCalled();
    expect(exec).toHaveBeenCalledWith("copy");
  });

  it("uses the execCommand fallback in a non-secure context (plain-HTTP LAN)", async () => {
    setSecureContext(false); // navigator.clipboard absent / unusable
    const exec = stubExecCommand(() => true);

    expect(await writeClipboard("lan-text")).toBe(true);
    expect(exec).toHaveBeenCalledWith("copy");
  });

  it("renders the temporary textarea during copy and removes it afterward", async () => {
    setSecureContext(false);
    let textareaPresentDuringCopy = false;
    stubExecCommand(() => {
      textareaPresentDuringCopy = document.querySelector("textarea") !== null;
      return true;
    });

    await writeClipboard("cleanup");
    expect(textareaPresentDuringCopy).toBe(true); // rendered & selectable during copy
    expect(document.querySelector("textarea")).toBeNull(); // cleaned up afterward
  });

  it("returns false when the fallback execCommand fails", async () => {
    setSecureContext(false);
    stubExecCommand(() => false);

    expect(await writeClipboard("nope")).toBe(false);
  });
});
