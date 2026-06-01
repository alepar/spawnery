import { afterEach } from "vitest";
import { cleanup } from "@testing-library/react";
import "@testing-library/jest-dom/vitest";

// Unmount React trees between tests so queries don't see stale DOM from a
// previous test's render (Testing Library's standard auto-cleanup).
afterEach(() => {
  cleanup();
});
