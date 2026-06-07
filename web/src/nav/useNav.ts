import { useCallback } from "react";
import { useLocation } from "wouter";
import { pathToNav, navToPath, type Nav } from "./nav";

/** Read/write the current Nav from the browser URL (pushes a history entry on write). */
export function useNav(): [Nav, (nav: Nav) => void] {
  const [location, setLocation] = useLocation();
  const nav = pathToNav(location);
  // wouter's setLocation is already stable; memoize setNav for referential stability
  // so callers can safely put it in a dependency array without triggering extra renders.
  const setNav = useCallback((next: Nav) => setLocation(navToPath(next)), [setLocation]);
  return [nav, setNav];
}
