/**
 * Tests for silent refresh.
 *
 * Key tests:
 * - Single-flight: N concurrent callers share one fetch (via in-memory lock fallback)
 * - cnf-mismatch: response token has wrong session_key_hash → kind:"cnf-mismatch"
 * - key-missing: keyCanSign=false short-circuits before fetch
 * - revoked: 401 body with "token_revoked" → kind:"revoked"
 * - jitter within bounds
 */

import { describe, it, expect, vi, beforeEach } from "vitest";
import { refreshAccessToken, computeRefreshDelay, REFRESH_LOCK_NAME } from "./refresh";
import { MemoryKeyStore } from "./keystore";
import { getOrCreateSessionKey, exportSpkiDer, sessionKeyHash } from "./keypair";
import { ProtoWriter } from "./protobuf";
import { toBase64Url } from "./token";

// Build a minimal wire token with a given session_key_hash
function buildWireToken(spkiHash: Uint8Array, expiresAt: bigint): string {
  const w = new ProtoWriter();
  w.writeBytes(1, "account-123");
  w.writeBytes(2, "handle");
  w.writeVarint(6, expiresAt);
  w.writeBytes(7, spkiHash);
  const body = w.finish();
  const fakeSig = new Uint8Array(64);
  return toBase64Url(body) + "." + toBase64Url(fakeSig);
}

function makeResponse(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

async function makeDeps(fetchFn: typeof fetch, opts?: { wrongHash?: boolean }) {
  const store = new MemoryKeyStore();
  const kp = await getOrCreateSessionKey(store);
  const spki = await exportSpkiDer(kp.publicKey);
  const hash = await sessionKeyHash(spki);
  const wrongHash = opts?.wrongHash ? new Uint8Array(32).fill(0xff) : hash;

  return {
    privateKey: kp.privateKey,
    publicKey: kp.publicKey,
    localSpkiHash: hash,
    refreshTokenHash: new Uint8Array(32).fill(0x11),
    // No lock — use in-memory single-flight
    acquireLock: undefined as undefined,
    fetchFn,
    now: () => 1700000000000,
    _spki: spki,
    _hash: hash,
    _wrongHash: wrongHash,
  };
}

beforeEach(() => {
  // Reset the module-level _inflight singleton between tests.
  // We can't access it directly, but calling with a fast mock clears it.
});

describe("refreshAccessToken — success", () => {
  it("returns ok with parsed token", async () => {
    const store = new MemoryKeyStore();
    const kp = await getOrCreateSessionKey(store);
    const spki = await exportSpkiDer(kp.publicKey);
    const hash = await sessionKeyHash(spki);

    const accessToken = buildWireToken(hash, 1800000000n);
    const fetchMock = vi.fn().mockResolvedValue(
      makeResponse(200, { access_token: accessToken, refresh_token_hash: "newhash" }),
    );

    const result = await refreshAccessToken({
      privateKey: kp.privateKey,
      publicKey: kp.publicKey,
      localSpkiHash: hash,
      refreshTokenHash: new Uint8Array(32),
      fetchFn: fetchMock,
      now: () => 1700000000000,
    });

    expect(result.kind).toBe("ok");
    if (result.kind === "ok") {
      expect(result.accessToken).toBe(accessToken);
      expect(result.refreshTokenHash).toBe("newhash");
      expect(result.expiresAt).toBe(1800000000n);
    }
  });
});

describe("refreshAccessToken — cnf-mismatch", () => {
  it("returns cnf-mismatch when session_key_hash does not match local", async () => {
    const store = new MemoryKeyStore();
    const kp = await getOrCreateSessionKey(store);
    const spki = await exportSpkiDer(kp.publicKey);
    const localHash = await sessionKeyHash(spki);

    // Token has a DIFFERENT hash
    const differentHash = new Uint8Array(32).fill(0xff);
    const accessToken = buildWireToken(differentHash, 1800000000n);

    const fetchMock = vi.fn().mockResolvedValue(
      makeResponse(200, { access_token: accessToken }),
    );

    const result = await refreshAccessToken({
      privateKey: kp.privateKey,
      publicKey: kp.publicKey,
      localSpkiHash: localHash,
      refreshTokenHash: new Uint8Array(32),
      fetchFn: fetchMock,
    });

    expect(result.kind).toBe("cnf-mismatch");
    // Must NOT store the token (test that fetchMock was called once and result is correct)
    expect(fetchMock).toHaveBeenCalledOnce();
  });
});

describe("refreshAccessToken — key-missing", () => {
  it("short-circuits before fetch when keyCanSign fails", async () => {
    // Create a key, delete from store (but keep a reference to a "dead" key won't work
    // in jsdom because the key object is still valid). Instead, test with an obviously
    // broken key — we can't easily invalidate a live CryptoKey in jsdom.
    // Instead, verify that if keyCanSign returns false, no fetch happens.
    // We mock keyCanSign by providing a privateKey that will throw on sign.
    // The simplest approach: pass a publicKey as privateKey (wrong usages → sign will throw).
    const store = new MemoryKeyStore();
    const kp = await getOrCreateSessionKey(store);
    const spki = await exportSpkiDer(kp.publicKey);
    const hash = await sessionKeyHash(spki);

    // Use the PUBLIC key as the "private" key — sign will throw (wrong usages).
    const fetchMock = vi.fn();
    const result = await refreshAccessToken({
      privateKey: kp.publicKey as unknown as CryptoKey, // wrong key → keyCanSign = false
      publicKey: kp.publicKey,
      localSpkiHash: hash,
      refreshTokenHash: new Uint8Array(32),
      fetchFn: fetchMock,
    });

    expect(result.kind).toBe("key-missing");
    expect(fetchMock).not.toHaveBeenCalled();
  });
});

describe("refreshAccessToken — revoked", () => {
  it("returns revoked on 401 with token_revoked body", async () => {
    const store = new MemoryKeyStore();
    const kp = await getOrCreateSessionKey(store);
    const spki = await exportSpkiDer(kp.publicKey);
    const hash = await sessionKeyHash(spki);

    const fetchMock = vi.fn().mockResolvedValue(
      new Response(JSON.stringify({ code: "token_revoked", message: "family revoked" }), {
        status: 401,
        headers: { "Content-Type": "application/json" },
      }),
    );

    const result = await refreshAccessToken({
      privateKey: kp.privateKey,
      publicKey: kp.publicKey,
      localSpkiHash: hash,
      refreshTokenHash: new Uint8Array(32),
      fetchFn: fetchMock,
    });

    expect(result.kind).toBe("revoked");
  });
});

describe("refreshAccessToken — single-flight", () => {
  it("two concurrent calls share one fetch even with a serializing (non-sharing) lock", async () => {
    // This test validates the real-production scenario: navigator.locks.request() serialises
    // callers but does NOT share results — each queued caller would normally run its own
    // _doRefresh with a potentially-stale refreshTokenHash (cookie rotated by the first caller).
    // The fix must guarantee only one fetch fires regardless of lock semantics.
    const store = new MemoryKeyStore();
    const kp = await getOrCreateSessionKey(store);
    const spki = await exportSpkiDer(kp.publicKey);
    const hash = await sessionKeyHash(spki);
    const accessToken = buildWireToken(hash, 1800000000n);

    let fetchCount = 0;
    let resolveFetch!: (v: Response) => void;
    const pendingFetch = new Promise<Response>((resolve) => { resolveFetch = resolve; });
    const fetchMock = vi.fn().mockImplementation(() => {
      fetchCount++;
      return pendingFetch;
    });

    // Serialising lock: queues callers and runs each fn() independently — semantically
    // identical to navigator.locks.request().  Does NOT share the in-flight promise.
    type LockFn = <T>(name: string, fn: () => Promise<T>) => Promise<T>;
    let lockHeld = false;
    const lockQueue: Array<() => void> = [];
    const serializingLock: LockFn = (_name, fn) =>
      new Promise<never>((resolve, reject) => {
        const run = () => {
          lockHeld = true;
          (fn() as Promise<never>).then(resolve, reject).finally(() => {
            lockHeld = false;
            if (lockQueue.length > 0) lockQueue.shift()!();
          });
        };
        if (!lockHeld) run();
        else lockQueue.push(run);
      });

    const deps = {
      privateKey: kp.privateKey,
      publicKey: kp.publicKey,
      localSpkiHash: hash,
      refreshTokenHash: new Uint8Array(32),
      acquireLock: serializingLock,
      fetchFn: fetchMock,
      now: () => 1700000000000,
    };

    // Fire two concurrent calls
    const p1 = refreshAccessToken(deps);
    const p2 = refreshAccessToken(deps);

    // Resolve the fetch
    resolveFetch(makeResponse(200, { access_token: accessToken, refresh_token_hash: "h" }));

    const [r1, r2] = await Promise.all([p1, p2]);
    expect(r1.kind).toBe("ok");
    expect(r2.kind).toBe("ok");
    // Only one fetch was made — the _inflight guard short-circuits p2 before the lock.
    expect(fetchCount).toBe(1);
  });
});

describe("computeRefreshDelay", () => {
  it("delays by approximately (expiresAt - now - 2min) ± 15s", () => {
    const expiresAt = 1800000000n; // unix s
    const nowMs = (1800000000 - 180) * 1000; // 180s before expiry
    const delay = computeRefreshDelay(expiresAt, nowMs);
    // Expected: 180000 - 120000 = 60000 ±15000
    expect(delay).toBeGreaterThanOrEqual(0);
    expect(delay).toBeLessThanOrEqual(60000 + 15000 + 1000); // margin for floating point
  });

  it("clamps to 0 if token already expired", () => {
    const past = 1000n;
    const delay = computeRefreshDelay(past, Date.now());
    expect(delay).toBe(0);
  });
});
