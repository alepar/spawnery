# Web Terminal Text Selection + Copy-on-Select

**Date:** 2026-06-21
**Status:** draft
**Component:** `web/` (xterm.js `TerminalView`)
**Builds on:** [Tmux Terminal Mode](2026-06-06-tmux-terminal-mode-design.md) (added `set -g mouse on`),
[Terminal Appearance Settings](2026-06-08-terminal-appearance-settings-design.md).

## Problem

Terminal scrollback scrolling in the web UI was enabled by setting `set -g mouse on` in the
spawn's tmux (`deploy/agent/tmux.conf`). That works — the wheel now drives tmux copy-mode
scrollback. But it silently broke **text selection** in the web terminal: dragging selects
nothing, and the user's instinctive `Shift+drag` does nothing either.

## Root cause (verified in source)

`set -g mouse on` makes tmux request mouse tracking from the outer terminal (xterm.js). From then
on xterm.js is in mouse-events mode and forwards all mouse drags to tmux instead of doing its own
native selection. xterm.js only restores native selection when a **forced-selection modifier** is
held. The platform check is hardcoded (verified in the vendored `@xterm/xterm@6.0.0` build,
`shouldForceSelection`):

```js
shouldForceSelection(e){
  return isMac
    ? e.altKey && this._optionsService.rawOptions.macOptionClickForcesSelection  // Mac: Option+drag, gated by the option
    : e.shiftKey;                                                                 // Linux/Windows: Shift+drag, unconditional
}
```

- **Linux/Windows:** `Shift+drag` already forces selection — no config needed.
- **macOS:** `shiftKey` is ignored entirely. Only `Alt/Option+drag` works, and *only* when
  `macOptionClickForcesSelection: true`. The current `new Terminal({ convertEol: false,
  cursorBlink: true })` does **not** set it — so on macOS there is currently **no** working
  selection gesture. The reporting user is on Firefox/macOS, which is exactly this case.

We cannot make `Shift` force selection on macOS via config: the `isMac` branch is hardcoded, and
spoofing non-Mac would break Cmd/Ctrl key handling. `Option` is the only built-in Mac path.

## Key decisions

1. **Fix is a config flag, not a tmux change.** Keep `set -g mouse on` (scrollback scrolling is
   the whole point of it). Set `macOptionClickForcesSelection: true` so Mac gets Option+drag.
2. **Selection alone is insufficient — wire copy.** xterm.js renders selection on its own canvas,
   not as a native DOM selection, so the browser's Cmd/Ctrl+C does not pick it up. We add
   **copy-on-select**: on drag-end, if there is a selection, write it to the clipboard.
3. **Clipboard write must work over plain-HTTP LAN.** `navigator.clipboard.writeText` requires a
   secure context (HTTPS/localhost); `just web` is LAN-accessible over HTTP where that API is
   absent. The helper tries the async Clipboard API first and falls back to a hidden-textarea
   `document.execCommand('copy')`.
4. **Discoverability — a quiet hint.** The gesture is invisible and differs per platform; that is
   what cost the user time. Show a subtle, non-interactive corner badge with the platform-correct
   gesture, flashing "Copied" briefly on a successful copy.

Rejected: **dropping `set -g mouse on`** and synthesizing wheel→copy-mode scroll in xterm — it
re-solves an already-solved problem with a more fragile mechanism. A **settings toggle** for
copy-on-select is YAGNI for now.

## Design

All changes are in `web/src/views/TerminalView.tsx` plus one small clipboard helper. No proto, no
backend, no tmux changes.

### 1. The flag
```ts
const term = new Terminal({
  convertEol: false,
  cursorBlink: true,
  macOptionClickForcesSelection: true,
});
```
Mac-only effect (the option is read only inside the `isMac` branch); harmless on Linux/Windows.

### 2. Clipboard helper (`web/src/term/clipboard.ts`)
```ts
// Returns true on success. Tries the async Clipboard API (secure context only),
// then falls back to a hidden-textarea execCommand for plain-HTTP LAN access.
export async function writeClipboard(text: string): Promise<boolean>
```
Implementation: if `window.isSecureContext && navigator.clipboard`, `await
navigator.clipboard.writeText(text)`; on absence/throw, create an off-screen `<textarea>`, set its
value, `select()`, `document.execCommand('copy')`, remove it; return whether either path succeeded.

### 3. Copy-on-select wiring
Attach a `mouseup` listener to the host element (registered/disposed inside the existing
spawnId-keyed effect, alongside the other listeners). On mouseup, if `term.hasSelection()`, read
`term.getSelection()` and `void writeClipboard(sel)`; on success, trigger the "Copied" flash.
Using `mouseup` (not `onSelectionChange`) copies once per completed drag and avoids clobbering the
clipboard mid-drag.

### 4. Quiet hint + flash
Wrap the bare host `<div>` in a `relative` container and add an absolutely-positioned corner badge:
- Platform detection: `const isMac = /Mac|iPhone|iPad/.test(navigator.platform) ||
  navigator.userAgent.includes("Macintosh")`.
- Text: `isMac ? "⌥ drag to select" : "Shift+drag to select"`.
- On a successful copy, swap the text to `"Copied"` for ~1.2s (a ref-tracked timeout, cleared on
  unmount), then revert.
- Styling: low opacity, small, `pointer-events-none`, so it never blocks terminal interaction.
  React state (`useState`) holds the current badge text.

## Acceptance criteria

- **Firefox/macOS:** `Option+drag` produces a visible selection; releasing copies it; pasting
  elsewhere yields the selected text. Works on both localhost/HTTPS **and** a plain-HTTP LAN IP
  (fallback path).
- **Linux:** `Shift+drag` selects and copies (it already selected; copy-on-select is the new part).
- Wheel scrolling through tmux scrollback is unchanged.
- The hint is visible but unobtrusive and shows the correct platform gesture; it flashes "Copied"
  on copy.
- No regression to keystroke input, resize/fit, reconnect, or appearance-settings live updates.

## Load-bearing assumption to verify during implementation

Firefox/macOS delivers `altKey: true` on the mousedown/drag MouseEvents when Option is held (it
should — Option maps to `altKey` in browser content — but it gates the entire Mac fix). Verify by
manual repro in the running web UI before closing the task.

## Post-Implementation Notes

*As this design is implemented and iterated on — bug fixes, adjustments, anything that diverged
from the assumptions above — append a dated note here, whether or not a formal debugging skill was
used.*
