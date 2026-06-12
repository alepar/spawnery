/**
 * Tests for OAuth authorize + callback handling.
 */

import { describe, it, expect, beforeEach } from "vitest";
import { buildAuthorizeUrl, parseCallback, StateStorage, HistoryFacade } from "./oauth";

// ── Test doubles ──────────────────────────────────────────────────────────────

function makeMemStorage(): StateStorage & { store: Map<string, string> } {
  const store = new Map<string, string>();
  return {
    store,
    get: (k) => store.get(k) ?? null,
    set: (k, v) => { store.set(k, v); },
    remove: (k) => { store.delete(k); },
  };
}

function makeHistory(search = "", pathname = "/callback"): HistoryFacade & { replaced: string[] } {
  const replaced: string[] = [];
  return {
    replaced,
    replaceState: (url) => { replaced.push(url); },
    locationSearch: () => search,
    locationPathname: () => pathname,
  };
}

function makeFakeSpkiDer(): Uint8Array {
  // 65-byte fake SPKI (enough to base64-encode; AS doesn't validate in this test)
  return new Uint8Array(65).fill(0x42);
}

// ── buildAuthorizeUrl tests ───────────────────────────────────────────────────

describe("buildAuthorizeUrl", () => {
  it("includes redirect_uri, state, session_pubkey in URL", () => {
    const storage = makeMemStorage();
    const spkiDer = makeFakeSpkiDer();
    const url = buildAuthorizeUrl({
      redirectUri: "https://app.example.com/callback",
      route: "/spawns",
      spkiDer,
      storage,
    });

    // URL may be relative (dev proxy) or absolute; parse params from the query string.
    expect(url).toContain("/oauth/authorize");
    const qStart = url.indexOf("?");
    const params = new URLSearchParams(qStart >= 0 ? url.slice(qStart + 1) : "");
    expect(params.get("redirect_uri")).toBe("https://app.example.com/callback");
    expect(params.get("state")).toBeTruthy();
    expect(params.get("session_pubkey")).toBeTruthy();
  });

  it("stores state and route in storage", () => {
    const storage = makeMemStorage();
    buildAuthorizeUrl({
      redirectUri: "https://app.example.com/callback",
      route: "/my-route",
      spkiDer: makeFakeSpkiDer(),
      storage,
    });
    const stored = JSON.parse(storage.store.values().next().value!);
    expect(stored.route).toBe("/my-route");
    expect(stored.state).toBeTruthy();
  });

  it("generates a different state on each call", () => {
    const s1 = makeMemStorage();
    const s2 = makeMemStorage();
    const spki = makeFakeSpkiDer();
    const opts = { redirectUri: "https://app.example.com/cb", route: "/", spkiDer: spki };
    const url1 = buildAuthorizeUrl({ ...opts, storage: s1 });
    const url2 = buildAuthorizeUrl({ ...opts, storage: s2 });
    const getState = (u: string) => {
      const q = u.slice(u.indexOf("?") + 1);
      return new URLSearchParams(q).get("state")!;
    };
    expect(getState(url1)).not.toBe(getState(url2));
  });
});

// ── parseCallback tests ───────────────────────────────────────────────────────

describe("parseCallback — none", () => {
  it("returns none when no params", () => {
    const storage = makeMemStorage();
    const history = makeHistory("");
    expect(parseCallback(storage, history)).toEqual({ kind: "none" });
  });
});

describe("parseCallback — error", () => {
  it("returns error for AS error params", () => {
    const storage = makeMemStorage();
    const history = makeHistory("?error=registration_closed&error_description=closed");
    const result = parseCallback(storage, history);
    expect(result).toMatchObject({ kind: "error", code: "registration_closed", description: "closed" });
    // Should strip URL
    expect(history.replaced).toHaveLength(1);
    expect(history.replaced[0]).toBe("/callback");
  });

  it("returns state_mismatch when no stored state", () => {
    const storage = makeMemStorage(); // empty
    const history = makeHistory("?access_token=tok&state=abc123");
    const result = parseCallback(storage, history);
    expect(result).toMatchObject({ kind: "error", code: "state_mismatch" });
    expect(history.replaced).toHaveLength(1);
  });

  it("returns state_mismatch when state does not match", () => {
    const storage = makeMemStorage();
    storage.set("spawnery-oauth-state", JSON.stringify({ state: "correct-state", route: "/" }));
    const history = makeHistory("?access_token=tok&state=wrong-state");
    const result = parseCallback(storage, history);
    expect(result).toMatchObject({ kind: "error", code: "state_mismatch" });
  });
});

describe("parseCallback — success", () => {
  it("returns token + route on valid state match", () => {
    const storage = makeMemStorage();
    const state = "my-state-123";
    storage.set("spawnery-oauth-state", JSON.stringify({ state, route: "/spawns" }));
    const history = makeHistory(
      `?access_token=token-wire&state=${state}&refresh_token_hash=abc123`,
    );

    const result = parseCallback(storage, history);
    expect(result).toMatchObject({
      kind: "ok",
      accessToken: "token-wire",
      refreshTokenHash: "abc123",
      route: "/spawns",
    });
    // Token stripped from URL
    expect(history.replaced).toHaveLength(1);
    expect(history.replaced[0]).toBe("/callback");
    // State consumed
    expect(storage.store.size).toBe(0);
  });

  it("returns empty refreshTokenHash if missing from URL", () => {
    const storage = makeMemStorage();
    const state = "st123";
    storage.set("spawnery-oauth-state", JSON.stringify({ state, route: "/" }));
    const history = makeHistory(`?access_token=tok&state=${state}`);
    const result = parseCallback(storage, history);
    expect(result).toMatchObject({ kind: "ok", refreshTokenHash: "" });
  });
});
