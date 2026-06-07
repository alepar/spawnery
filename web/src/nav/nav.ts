// Pure URL<->Nav mapping. No router imports here — stays testable in Node.
export type Nav =
  | { section: "templates" }
  | { section: "app"; appId: string }
  | { section: "my-apps" }
  | { section: "publish" }
  | { section: "spawn"; spawnId: string }
  | { section: "settings" };

const TEMPLATES: Nav = { section: "templates" };

/** Parse a URL pathname (may include query/hash) into a Nav. Unknown paths -> templates. */
export function pathToNav(raw: string): Nav {
  // Strip query string and hash, handle empty string as root.
  const path = (raw || "/").split("?")[0].split("#")[0];
  // Normalise trailing slash (but keep lone "/" intact so split works uniformly).
  const clean = path.length > 1 ? path.replace(/\/$/, "") : path;
  const parts = clean.split("/").filter(Boolean); // ["templates"] or ["spawn","abc"]

  switch (parts[0]) {
    case undefined:                 // "/"
      return TEMPLATES;
    case "templates":
      if (parts.length === 1) return TEMPLATES;
      if (parts.length === 2) return { section: "app", appId: decodeURIComponent(parts[1]) };
      return TEMPLATES; // unexpected depth -> fallback
    case "my-apps":   return { section: "my-apps" };
    case "publish":   return { section: "publish" };
    case "settings":  return { section: "settings" };
    case "spawn":
      if (parts.length === 2) return { section: "spawn", spawnId: decodeURIComponent(parts[1]) };
      return TEMPLATES;
    default:
      return TEMPLATES;
  }
}

/** Serialise a Nav back to a URL pathname. */
export function navToPath(nav: Nav): string {
  switch (nav.section) {
    case "templates": return "/templates";
    case "app":       return `/templates/${encodeURIComponent(nav.appId)}`;
    case "my-apps":   return "/my-apps";
    case "publish":   return "/publish";
    case "spawn":     return `/spawn/${encodeURIComponent(nav.spawnId)}`;
    case "settings":  return "/settings";
  }
}
