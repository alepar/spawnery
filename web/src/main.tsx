import { createRoot } from "react-dom/client";
import { Router } from "wouter";
import { App } from "./App";
import "./globals.css";
// Router provides useLocation (browser-history) to the whole tree; no base/config needed.
createRoot(document.getElementById("root")!).render(<Router><App /></Router>);
