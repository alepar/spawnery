import { createRoot } from "react-dom/client";
import { Router } from "wouter";
import { App } from "./App";
import { TermSettingsProvider } from "./term/settings";
import { useSessionStore } from "./auth/session";
import "./globals.css";

// Bootstrap auth BEFORE rendering the app (session.ts handles dev-mode bypass).
// This is synchronous-enough: bootstrap() is async but React defers to its result
// via status=loading until complete.
useSessionStore.getState().bootstrap().catch((e: unknown) => {
  console.error("auth bootstrap failed:", e);
});

// Router provides useLocation (browser-history) to the whole tree; no base/config needed.
// TermSettingsProvider holds terminal appearance settings for the whole tree.
createRoot(document.getElementById("root")!).render(
  <Router>
    <TermSettingsProvider>
      <App />
    </TermSettingsProvider>
  </Router>,
);
