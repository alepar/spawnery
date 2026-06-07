import { useCallback, useMemo } from "react";
import { useLocation } from "wouter";
import { pathToNav, navToPath, type Nav } from "./nav";

/**
 * Read/write the current Nav from the browser URL. The setter pushes a history entry by default;
 * pass `{ replace: true }` to swap the current entry instead (e.g. one-time URL normalization that
 * should not leave a back-button trap).
 */
export function useNav(): [Nav, (nav: Nav, opts?: { replace?: boolean }) => void] {
  const [location, setLocation] = useLocation();
  // Memoize on `location` so the Nav identity is stable between renders that don't change the URL —
  // consumers' effects keyed on `nav` then fire only on real navigation, not on every poll/keystroke.
  const nav = useMemo(() => pathToNav(location), [location]);
  // wouter's setLocation is already stable; memoize setNav for referential stability
  // so callers can safely put it in a dependency array without triggering extra renders.
  const setNav = useCallback(
    (next: Nav, opts?: { replace?: boolean }) => setLocation(navToPath(next), { replace: opts?.replace }),
    [setLocation],
  );
  return [nav, setNav];
}
