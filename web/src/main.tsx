import { createRoot } from "react-dom/client";
import { Router } from "wouter";
import { App } from "./App";
import { TermSettingsProvider } from "./term/settings";
import "./globals.css";
// Router provides useLocation (browser-history) to the whole tree; no base/config needed.
// TermSettingsProvider holds terminal appearance settings for the whole tree.
createRoot(document.getElementById("root")!).render(
  <Router>
    <TermSettingsProvider>
      <App />
    </TermSettingsProvider>
  </Router>,
);
