/**
 * Write text to the clipboard, returning true on success.
 *
 * Prefers the async Clipboard API, which only exists in a secure context
 * (HTTPS or localhost). `just web` is LAN-accessible over plain HTTP, where
 * `navigator.clipboard` is undefined — so fall back to a hidden-textarea
 * `execCommand('copy')`. The textarea must be RENDERED and selectable for the
 * copy to fire, so it is positioned off-screen via opacity, never `display:none`
 * (which makes execCommand a no-op).
 *
 * Focus side effect: the execCommand path moves DOM focus to the temporary
 * textarea. Restoring focus is the CALLER's responsibility — this helper stays
 * focus-agnostic (see TerminalView copy-on-select wiring).
 */
export async function writeClipboard(text: string): Promise<boolean> {
  if (!text) return false;

  if (window.isSecureContext && navigator.clipboard?.writeText) {
    try {
      await navigator.clipboard.writeText(text);
      return true;
    } catch {
      // Permission denied / transient failure — fall through to the legacy path.
    }
  }

  return legacyCopy(text);
}

/** Hidden-textarea fallback for non-secure contexts (plain-HTTP LAN). */
function legacyCopy(text: string): boolean {
  const ta = document.createElement("textarea");
  ta.value = text;
  ta.setAttribute("readonly", "");
  // Off-screen but rendered & selectable. display:none would make copy a no-op.
  ta.style.position = "fixed";
  ta.style.top = "0";
  ta.style.left = "0";
  ta.style.opacity = "0";
  ta.style.pointerEvents = "none";
  document.body.appendChild(ta);
  try {
    ta.select();
    ta.setSelectionRange(0, text.length);
    return document.execCommand("copy");
  } catch {
    return false;
  } finally {
    document.body.removeChild(ta);
  }
}
