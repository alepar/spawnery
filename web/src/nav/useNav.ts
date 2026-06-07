import { useLocation } from "wouter";
import { pathToNav, navToPath, type Nav } from "./nav";

/** Read/write the current Nav from the browser URL (pushes a history entry on write). */
export function useNav(): [Nav, (nav: Nav) => void] {
  const [location, setLocation] = useLocation();
  const nav = pathToNav(location);
  const setNav = (next: Nav) => setLocation(navToPath(next));
  return [nav, setNav];
}
