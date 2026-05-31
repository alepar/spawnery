# Frontend Setup & Conventions

This is the implementer's quickstart for our UI stack. Read it once before writing components.

## The stack (what & why)

| Layer | Choice | Why |
|---|---|---|
| Framework | **React 19 + TypeScript** | Baseline. |
| Build | **Vite** | Fast, boring, no SSR complexity for a desktop app. (Use Next.js only if we add a public marketing site.) |
| Styling | **Tailwind CSS v4** | CSS-first config, build-time only, biggest React ecosystem. No runtime CSS-in-JS. |
| Components | **shadcn/ui** (Radix primitives) | We *own* the component source. Accessible, unstyled-by-default, swappable to Base UI later via one flag. |
| Theming | **CSS custom properties + `.dark` class** | One token surface drives chat, sidebar, marketplace, settings. |
| Chat list | **react-virtuoso** | Variable-height items, reverse scroll, stick-to-bottom — for free. |
| Markdown | **streamdown** (streaming) / `react-markdown` (static) | Handles half-streamed tokens, code fences, KaTeX. |
| Toasts | **Sonner** | shadcn's current default; honors `.dark` automatically. |
| Class helpers | **clsx + tailwind-merge** (via `cn()`) | Stable, conflict-free className composition. |

**Hard rules:** no styled-components / Emotion (runtime CSS-in-JS is on the way out). Don't mix component libraries (no MUI + shadcn). Treat shadcn-generated files as *our* code, reviewed in PRs.

## Bootstrap

```bash
# 1. Scaffold
npm create vite@latest app -- --template react-ts
cd app && npm install

# 2. Tailwind v4 (Vite plugin — no PostCSS config needed)
npm install tailwindcss @tailwindcss/vite

# 3. Helpers
npm install clsx tailwind-merge

# 4. Chat + content deps
npm install react-virtuoso streamdown sonner
```

`vite.config.ts`:

```ts
import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";
import path from "path";

export default defineConfig({
  plugins: [react(), tailwindcss()],
  resolve: {
    alias: { "@": path.resolve(__dirname, "./src") }, // shadcn expects "@/..."
  },
});
```

Then initialize shadcn and add components as needed:

```bash
npx shadcn@latest init          # choose Radix (default) when prompted
npx shadcn@latest add button card dialog dropdown-menu input \
    select switch tabs sidebar command sonner
```

> If shadcn ever asks Radix vs. Base UI: pick **Radix** for now. The escape hatch is real — because the component source lives in `src/components/ui/`, switching primitives later is a per-component edit, not a rewrite.

## Token architecture (the important part)

All theming lives in **one file**, `src/globals.css`, in three layers. Everything downstream — chat bubbles, sidebar, marketplace cards, settings — reads semantic tokens only. Components must **never** hardcode `bg-gray-900`; they use `bg-background`, `text-foreground`, `border-border`, etc.

```css
@import "tailwindcss";

/* dark is class-based so React controls it (not just OS preference) */
@custom-variant dark (&:where(.dark, .dark *));

/* Layer 1 — primitives (raw values, no meaning) */
@theme {
  --font-sans: "Inter", ui-sans-serif, system-ui, sans-serif;
  --font-mono: "JetBrains Mono", ui-monospace, monospace;
  --text-base: 0.9375rem;     /* ~15px, easier for long reading */
  --leading-relaxed: 1.65;
}

/* Layer 2 — semantic tokens, light default */
:root {
  --color-background: oklch(0.99 0 0);
  --color-foreground: oklch(0.20 0 0);
  --color-muted:      oklch(0.55 0 0);
  --color-border:     oklch(0.92 0 0);
  --color-surface:    oklch(1 0 0);
  --color-surface-hover: oklch(0.96 0 0);
  --color-accent:     oklch(0.55 0.2 255);
}

/* Layer 2 — dark overrides */
.dark {
  --color-background: oklch(0.16 0 0);
  --color-foreground: oklch(0.97 0 0);
  --color-muted:      oklch(0.65 0 0);
  --color-border:     oklch(0.27 0 0);
  --color-surface:    oklch(0.21 0 0);
  --color-surface-hover: oklch(0.27 0 0);
  --color-accent:     oklch(0.70 0.16 255);
}

/* Expose semantic tokens as utilities: bg-background, text-foreground, ... */
@theme inline {
  --color-background: var(--color-background);
  --color-foreground: var(--color-foreground);
  --color-muted: var(--color-muted);
  --color-border: var(--color-border);
  --color-surface: var(--color-surface);
  --color-surface-hover: var(--color-surface-hover);
  --color-accent: var(--color-accent);
}
```

Layer 3 (component variants) lives inside each shadcn component file via `cva`.

> Note: don't use pure `#000`/`#fff` for text or background — the off-black/off-white above is easier on the eyes over long sessions.

## Dark mode without the flash

Persist the user's choice, fall back to OS preference. Precedence: **explicit choice > system > light**.

Inline this in `index.html` `<head>` **before** any stylesheet — this runs before first paint and prevents the flash-of-wrong-theme:

```html
<script>
  (function () {
    try {
      var s = localStorage.getItem("theme");
      var d = s ? s === "dark" : matchMedia("(prefers-color-scheme: dark)").matches;
      if (d) document.documentElement.classList.add("dark");
    } catch (e) {}
  })();
</script>
```

The settings toggle just flips the class and writes the key:

```ts
function setTheme(theme: "light" | "dark") {
  document.documentElement.classList.toggle("dark", theme === "dark");
  localStorage.setItem("theme", theme);
}
```

## The `cn()` helper

Every component uses this for className composition (merges Tailwind conflicts correctly):

```ts
// src/lib/utils.ts
import { clsx, type ClassValue } from "clsx";
import { twMerge } from "tailwind-merge";

export const cn = (...inputs: ClassValue[]) => twMerge(clsx(inputs));
```

## Chat surface usage

```tsx
import { Virtuoso } from "react-virtuoso";
import { Streamdown } from "streamdown";

<Virtuoso
  data={messages}
  followOutput="smooth"          // stick to bottom on new message
  startReached={loadOlder}       // reverse-infinite scroll
  itemContent={(_, msg) => (
    <div className="mx-auto max-w-[70ch] px-4 py-3 text-foreground">
      <Streamdown>{msg.content}</Streamdown>
    </div>
  )}
/>
```

- **Memoize message blocks.** Without it, every streaming token re-parses the *entire* transcript. Wrap each message in `React.memo` keyed by a stable message id.
- **Constrain measure** to ~`max-w-[70ch]` for assistant prose — readability over width.
- Static (already-saved) history can use plain `react-markdown` + `remark-gfm`; reserve `streamdown` for the live-streaming message.

## Reusing tokens across other surfaces

Because everything reads the same semantic tokens, coherence is free:

- **Sidebar** (chats / spawns): `npx shadcn add sidebar` — collapsible, keyboard-accessible, uses `--surface`/`--border`/`--muted`.
- **Marketplace**: shadcn `Card` + `Dialog` on a CSS grid. Same `bg-surface border-border` as chat bubbles → visually consistent.
- **Settings**: shadcn `Tabs` + `Switch` + `Select`; the theme toggle writes the same `localStorage` key the inline script reads.
- **Command palette / menus**: `npx shadcn add command dropdown-menu context-menu`.

## Conventions checklist (enforce in review)

- [ ] Components read **semantic tokens only** (`bg-background`, never `bg-zinc-900`).
- [ ] No new color appears outside `globals.css`. New color = new semantic token, defined for both modes.
- [ ] Use `cn()` for all conditional classes — never string-concatenate classNames.
- [ ] Keep the type scale to 4–5 sizes, weights to 2–3, ~5 core colors per mode. Restraint is the whole "minimal" aesthetic.
- [ ] shadcn components are committed as our source and reviewed; re-pull upstream fixes deliberately via `shadcn add --overwrite`.
- [ ] Message components are memoized; chat list is virtualized (never `.map()` a long transcript directly).
- [ ] One component library only. No MUI/Mantine/Chakra alongside shadcn.

## Upgrade & longevity notes

- **Radix** cadence has slowed. We're insulated because we own the component code. If it stalls hard, switch primitives to **Base UI** per-component (shadcn supports both since Dec 2025).
- **Tailwind v4** relies on modern CSS (`oklch`, `color-mix`, `@property`). Fine in Electron (we control Chromium). If we ship **Tauri**, test on the WebKit target (macOS/Linux) for `-webkit-` quirks.
- Re-evaluate the Radix/Base UI question once a year; otherwise this stack should hold for years without churn.
