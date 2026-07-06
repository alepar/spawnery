/**
 * Connect transport wrapper: wires an AuthProvider into the generated Connect client via a
 * bearer-token interceptor with single silent-refresh-and-retry on Unauthenticated, mirroring
 * web's `unary()` (web/src/api/connect.ts).
 */

import { createConnectTransport } from "@connectrpc/connect-web";
import { Code, ConnectError, type Interceptor, type Transport } from "@connectrpc/connect";
import type { PoPHeaders } from "./signing/pop.js";

/**
 * AuthProvider supplies the bearer token for every RPC and (optionally) the machinery to
 * refresh it on a 401/Unauthenticated and to sign a PoP challenge. Both refresh and PoP-signing
 * are app-side concerns (they need the session KeyStore + refresh-token plumbing); the SDK only
 * calls through this interface.
 */
export interface AuthProvider {
  getBearer(): Promise<string>;
  signPoP?(refreshTokenHash: Uint8Array): Promise<PoPHeaders>;
  refresh?(): Promise<void>;
}

export interface TransportOptions {
  baseUrl: string;
  auth?: AuthProvider;
  fetch?: typeof fetch;
}

/**
 * authInterceptor sets the Authorization header from the AuthProvider before every call. On a
 * ConnectError with code=Unauthenticated, if the provider supports refresh, it refreshes once and
 * retries the call a single time (mirrors web's unary() one-shot silent refresh). With no
 * AuthProvider, requests go out unauthenticated (the dev/unenforced CP path).
 */
function authInterceptor(auth?: AuthProvider): Interceptor {
  return (next) => async (req) => {
    if (!auth) return next(req);

    req.header.set("Authorization", `Bearer ${await auth.getBearer()}`);
    try {
      return await next(req);
    } catch (e: unknown) {
      if (
        e instanceof ConnectError &&
        e.code === Code.Unauthenticated &&
        auth.refresh
      ) {
        await auth.refresh();
        req.header.set("Authorization", `Bearer ${await auth.getBearer()}`);
        return next(req);
      }
      throw e;
    }
  };
}

/** createTransport builds a connect-web fetch transport with the auth interceptor installed. */
export function createTransport(opts: TransportOptions): Transport {
  return createConnectTransport({
    baseUrl: opts.baseUrl,
    fetch: opts.fetch ?? fetch,
    interceptors: [authInterceptor(opts.auth)],
  });
}
